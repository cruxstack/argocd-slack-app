package argocdslack

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ExtractErrorLine picks the most useful line from a crashed container's log:
// the last error-level JSON entry (msg plus error name and code), else the last
// line that mentions an error, else the last line.
func ExtractErrorLine(logs string) string {
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	var lastText string
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if lastText == "" {
			lastText = l
		}
		if strings.HasPrefix(l, "{") {
			if s, ok := structuredError(l); ok {
				return s
			}
			continue
		}
		low := strings.ToLower(l)
		for _, w := range []string{"error", "fatal", "panic", "exception", "refused", "timeout"} {
			if strings.Contains(low, w) {
				return l
			}
		}
	}
	if strings.HasPrefix(lastText, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(lastText), &m) == nil {
			if msg := str(m, "msg", "message"); msg != "" {
				return msg
			}
		}
	}
	return lastText
}

func structuredError(line string) (string, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(line), &m) != nil {
		return "", false
	}
	lvl := strings.ToLower(str(m, "level", "severity", "lvl", "log.level"))
	if n, ok := m["level"].(float64); ok && n >= 50 { // pino numeric levels
		lvl = "error"
	}
	switch lvl {
	case "error", "fatal", "panic", "critical", "crit", "emerg", "alert":
	default:
		return "", false
	}
	parts := []string{}
	if msg := str(m, "msg", "message"); msg != "" {
		parts = append(parts, msg)
	}
	for _, k := range []string{"err", "error"} {
		switch v := m[k].(type) {
		case map[string]any:
			id := strings.TrimSpace(str(v, "name", "type") + " " + str(v, "code"))
			if id == "" {
				id = str(v, "message")
			}
			if id != "" {
				parts = append(parts, id)
			}
		case string:
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, " · "), true
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return fmt.Sprint(v)
		}
	}
	return ""
}

var redactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[jwt]"},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 [redacted]"},
	{regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "$1[redacted]@"},
	{regexp.MustCompile(`(?i)((?:pass(?:word|wd)?|secret|token|api[_-]?key|access[_-]?key|auth[a-z_-]*|session|cookie|private[_-]?key)["']?\s*[:=]\s*["']?)[^\s"',;&}]+`), "$1[redacted]"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "[aws-key]"},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), "[slack-token]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), "[github-token]"},
	{regexp.MustCompile(`\b[A-Za-z0-9+/_-]{40,}={0,2}`), "[redacted]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[email]"},
}

// Redact strips likely secrets and personal data before text reaches Slack.
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
