package argocdslack

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Mute kinds.
const (
	KindIncidents = "incidents"
	KindDeploys   = "deploys" // successful deploys
	KindFailed    = "failed"  // failed or unhealthy deploys
)

// MaxMute caps every mute.
const MaxMute = 7 * 24 * time.Hour

// Mute silences one scope until a time or until the app is healthy.
type Mute struct {
	ID           string    `json:"id"`
	App          string    `json:"app,omitempty"`
	Cluster      string    `json:"cluster,omitempty"`
	Env          string    `json:"env,omitempty"`
	Kinds        []string  `json:"kinds"`
	Until        time.Time `json:"until"`
	UntilHealthy bool      `json:"until_healthy,omitempty"`
	By           string    `json:"by"`
	Reason       string    `json:"reason,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Announced    Message   `json:"announced,omitzero"`
}

// Scope is how the mute reads to people.
func (m Mute) Scope() string {
	switch {
	case m.App != "" && m.Cluster != "":
		return m.App + "@" + m.Cluster
	case m.App != "":
		return m.App + " everywhere"
	case m.Cluster != "":
		return "all of " + m.Cluster
	case m.Env != "":
		return "all of " + m.Env
	}
	return "everything"
}

// Wide mutes are announced in the channel.
func (m Mute) Wide() bool { return m.App == "" }

// Active reports whether the mute still applies.
func (m Mute) Active(now time.Time) bool { return now.Before(m.Until) }

// Matches reports whether the mute covers kind for app on cluster.
func (m Mute) Matches(cl Cluster, app, kind string, now time.Time) bool {
	if !m.Active(now) || !slices.Contains(m.Kinds, kind) {
		return false
	}
	if m.App != "" && m.App != app {
		return false
	}
	if m.Cluster != "" && m.Cluster != cl.Name {
		return false
	}
	if m.Env != "" && m.Env != cl.Env {
		return false
	}
	return true
}

// UntilText is the expiry as people read it.
func (m Mute) UntilText() string {
	if m.UntilHealthy {
		return "until healthy"
	}
	return "until " + slackTime(m.Until, "{date_short_pretty} {time}")
}

// Note is the context line shown on muted messages.
func (m Mute) Note() string {
	s := fmt.Sprintf("🔕 Muted %s by <@%s>", m.UntilText(), m.By)
	if m.Reason != "" {
		s += ": " + m.Reason
	}
	return s
}

// FindMute returns the first active mute covering kind.
func FindMute(mutes []Mute, cl Cluster, app, kind string, now time.Time) *Mute {
	for i := range mutes {
		if mutes[i].Matches(cl, app, kind, now) {
			return &mutes[i]
		}
	}
	return nil
}

// NewMuteID returns a short random ID people can type.
func NewMuteID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ParseMuteArgs reads "/argocd mute <scope> [duration|until-healthy] [reason...]".
// scope is app@cluster, a cluster name, an environment, or an app name.
func ParseMuteArgs(cfg Config, args []string, now time.Time) (Mute, error) {
	m := Mute{Kinds: []string{KindIncidents, KindDeploys, KindFailed}, CreatedAt: now}
	if len(args) == 0 {
		return m, errors.New("say what to mute, e.g. `/argocd mute api@prod-use1 4h waiting on a fix`")
	}
	scope := strings.ToLower(args[0])
	envs := map[string]bool{}
	for _, cl := range cfg.Clusters {
		envs[cl.Env] = true
	}
	switch {
	case strings.Contains(scope, "@"):
		app, cluster, _ := strings.Cut(scope, "@")
		if _, ok := cfg.ClusterByName(cluster); !ok {
			return m, fmt.Errorf("no cluster called %q; try %s", cluster, clusterNames(cfg))
		}
		m.App, m.Cluster = app, cluster
	case isCluster(cfg, scope):
		m.Cluster = scope
	case envs[scope]:
		m.Env = scope
	default:
		m.App = scope
	}
	rest := args[1:]
	dur := 4 * time.Hour
	if len(rest) > 0 {
		switch t := strings.ToLower(rest[0]); t {
		case "until-healthy", "healthy", "until_healthy":
			if m.App == "" {
				return m, errors.New("until-healthy only works for one app")
			}
			m.UntilHealthy, dur = true, MaxMute
			rest = rest[1:]
		default:
			if d, err := parseDuration(t); err == nil && d > 0 {
				dur = d
				rest = rest[1:]
			}
		}
	}
	if dur > MaxMute {
		return m, errors.New("mutes last a week at most")
	}
	m.Until = now.Add(dur)
	m.Reason = trimTo(strings.Join(rest, " "), 140)
	return m, nil
}

func isCluster(cfg Config, s string) bool {
	_, ok := cfg.ClusterByName(s)
	return ok
}

func clusterNames(cfg Config) string {
	var n []string
	for _, cl := range cfg.Clusters {
		n = append(n, "`"+cl.Name+"`")
	}
	return strings.Join(n, ", ")
}
