package argocdslack

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Config is ARGOCD_SLACK_CONFIG.
type Config struct {
	DryRun          bool              `json:"dry_run"`
	Channel         string            `json:"channel"`
	ChannelOverride string            `json:"channel_override"`
	Timezone        string            `json:"timezone"`
	WatchSeconds    int               `json:"watch_seconds"`
	RichBlocks      *bool             `json:"rich_blocks"`
	PreviewPrefix   string            `json:"preview_prefix"`
	Grace           Grace             `json:"grace"`
	Owners          map[string]string `json:"owners"`
	Annotations     Annotations       `json:"annotations"`
	SCM             SCM               `json:"scm"`
	Links           []Link            `json:"links"`
	Clusters        []Cluster         `json:"clusters"`
	Setup           SetupConfig       `json:"setup"`
	IconBaseURL     string            `json:"icon_base_url"`

	loc *time.Location
}

// SetupConfig controls the web installer that creates and installs the Slack
// app. Turn it on for the first deploy and off once the app is installed.
type SetupConfig struct {
	Enabled bool   `json:"enabled"`
	AppName string `json:"app_name"`
	BaseURL string `json:"base_url"`
	Command string `json:"command"`
}

// Grace holds every timing rule.
type Grace struct {
	Incident       Duration `json:"incident"`
	Autoscaler     Duration `json:"autoscaler"`
	Flap           Duration `json:"flap"`
	DeployWindow   Duration `json:"deploy_window"`
	Stall          Duration `json:"stall"`
	LongRunning    Duration `json:"long_running"`
	PreviewCleanup Duration `json:"preview_cleanup"`
}

// Annotations names the Application annotations that carry deploy metadata.
// Empty names fall back to what Argo CD records itself.
type Annotations struct {
	Repo       string `json:"repo"`
	Commit     string `json:"commit"`
	Version    string `json:"version"`
	Actor      string `json:"actor"`
	ApprovedBy string `json:"approved_by"`
	PR         string `json:"pr"`
	Title      string `json:"title"`
	RunURL     string `json:"run_url"`
	Changes    string `json:"changes"`
}

// SCM builds links from {repo}, {commit}, {pr}, {version}, {from} and {to}.
type SCM struct {
	CommitURL  string `json:"commit_url"`
	PRURL      string `json:"pr_url"`
	VersionURL string `json:"version_url"`
	CompareURL string `json:"compare_url"`
}

// Link is a button on every message; URL is a template.
type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// Cluster is one Argo CD instance the app watches.
type Cluster struct {
	Name      string            `json:"name"`
	Env       string            `json:"env"`
	Region    string            `json:"region"`
	Cluster   string            `json:"cluster"`
	RoleARN   string            `json:"role_arn"`
	Namespace string            `json:"namespace"`
	ArgoCDURL string            `json:"argocd_url"`
	Channel   string            `json:"channel"`
	Enabled   bool              `json:"enabled"`
	LogLines  bool              `json:"log_lines"`
	Links     []Link            `json:"links"`
	Vars      map[string]string `json:"vars"`
}

// Duration reads "5m" style JSON strings, plus d (days) and w (weeks).
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := parseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if n := len(s); n > 1 && (s[n-1] == 'd' || s[n-1] == 'w') {
		var v int
		if _, err := fmt.Sscanf(s[:n-1], "%d", &v); err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		unit := 24 * time.Hour
		if s[n-1] == 'w' {
			unit *= 7
		}
		return time.Duration(v) * unit, nil
	}
	return time.ParseDuration(s)
}

// LoadConfig parses the config, fills defaults and reports every problem.
func LoadConfig(raw string) (Config, error) {
	var c Config
	if strings.TrimSpace(raw) == "" {
		return c, errors.New("ARGOCD_SLACK_CONFIG is empty")
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, fmt.Errorf("failed to parse ARGOCD_SLACK_CONFIG: %w", err)
	}
	def := func(d *Duration, v time.Duration) {
		if d.Duration == 0 {
			d.Duration = v
		}
	}
	def(&c.Grace.Incident, 5*time.Minute)
	def(&c.Grace.Autoscaler, 10*time.Minute)
	def(&c.Grace.Flap, 15*time.Minute)
	def(&c.Grace.DeployWindow, 10*time.Minute)
	def(&c.Grace.Stall, 15*time.Minute)
	def(&c.Grace.LongRunning, 24*time.Hour)
	def(&c.Grace.PreviewCleanup, 72*time.Hour)
	if c.WatchSeconds <= 0 || c.WatchSeconds > 58 {
		c.WatchSeconds = 55
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if c.PreviewPrefix == "" {
		c.PreviewPrefix = "preview-"
	}
	if c.SCM.CommitURL == "" {
		c.SCM.CommitURL = "https://github.com/{repo}/commit/{commit}"
	}
	if c.SCM.PRURL == "" {
		c.SCM.PRURL = "https://github.com/{repo}/pull/{pr}"
	}
	if c.SCM.CompareURL == "" {
		c.SCM.CompareURL = "https://github.com/{repo}/compare/{from}...{to}"
	}
	if c.Setup.AppName == "" {
		c.Setup.AppName = "Argo CD"
	}
	if c.Setup.Command == "" {
		c.Setup.Command = "/argocd"
	}
	if c.IconBaseURL == "" {
		c.IconBaseURL = DefaultIconBaseURL("main")
	}

	var problems []string
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		problems = append(problems, fmt.Sprintf("timezone %q: %v", c.Timezone, err))
	}
	c.loc = loc
	seen := map[string]bool{}
	for i := range c.Clusters {
		cl := &c.Clusters[i]
		switch {
		case cl.Name == "":
			problems = append(problems, fmt.Sprintf("clusters[%d] needs a name", i))
		case seen[cl.Name]:
			problems = append(problems, fmt.Sprintf("cluster %q is listed twice", cl.Name))
		case cl.Name == DemoCluster:
			problems = append(problems, fmt.Sprintf("cluster name %q is kept for `/argocd demo`", DemoCluster))
		}
		seen[cl.Name] = true
		if cl.Cluster == "" {
			problems = append(problems, fmt.Sprintf("cluster %q needs the EKS cluster name in `cluster`", cl.Name))
		}
		if cl.Enabled && c.ChannelFor(*cl) == "" {
			problems = append(problems, fmt.Sprintf("cluster %q has no channel and there is no default channel", cl.Name))
		}
		if cl.Namespace == "" {
			cl.Namespace = "argocd"
		}
		if cl.Region == "" {
			cl.Region = "us-east-1"
		}
	}
	if !strings.HasPrefix(c.Setup.Command, "/") || strings.ContainsAny(c.Setup.Command, " \t") {
		problems = append(problems, "setup.command must look like /argocd")
	}
	for _, l := range c.Links {
		if l.Text == "" || l.URL == "" {
			problems = append(problems, "every link needs text and url")
		}
	}
	if len(problems) > 0 {
		return c, errors.New("invalid ARGOCD_SLACK_CONFIG: " + strings.Join(problems, "; "))
	}
	return c, nil
}

// DefaultIconBaseURL serves the status icons from this repository at ref.
func DefaultIconBaseURL(ref string) string {
	return "https://raw.githubusercontent.com/cruxstack/argocd-slack-app/" + ref + "/assets/status"
}

// IconURL is the address of a status icon, or "" when icons are off.
func (c Config) IconURL(name string) string {
	if c.IconBaseURL == "" || c.IconBaseURL == "none" {
		return ""
	}
	return strings.TrimRight(c.IconBaseURL, "/") + "/" + name + ".png"
}

// Rich reports whether the newer Block Kit blocks are enabled.
func (c Config) Rich() bool { return c.RichBlocks == nil || *c.RichBlocks }

// Location is the zone used for the digest and its date.
func (c Config) Location() *time.Location {
	if c.loc == nil {
		return time.UTC
	}
	return c.loc
}

// ClusterByName finds a configured cluster.
func (c Config) ClusterByName(name string) (Cluster, bool) {
	for _, cl := range c.Clusters {
		if cl.Name == name {
			return cl, true
		}
	}
	return Cluster{}, false
}

// ChannelFor is where a cluster posts; the override sends everything to one
// channel, for example while testing.
func (c Config) ChannelFor(cl Cluster) string {
	switch {
	case c.ChannelOverride != "":
		return c.ChannelOverride
	case cl.Channel != "":
		return cl.Channel
	}
	return c.Channel
}

// IsPreview marks preview environments.
func (c Config) IsPreview(app string) bool {
	return c.PreviewPrefix != "" && strings.HasPrefix(app, c.PreviewPrefix)
}

// AppURL links the app in this cluster's Argo CD.
func (cl Cluster) AppURL(app string) string {
	if cl.ArgoCDURL == "" {
		return ""
	}
	return strings.TrimRight(cl.ArgoCDURL, "/") + "/applications/" + url.PathEscape(cl.Namespace) + "/" + url.PathEscape(app)
}

var placeholder = regexp.MustCompile(`\{([a-z0-9_]+)\}`)

// expand fills {name} placeholders from vars, URL-escaping each value and
// leaving unknown names as they are.
func expand(tmpl string, vars map[string]string) string {
	return placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		if v, ok := vars[m[1:len(m)-1]]; ok {
			return url.QueryEscape(v)
		}
		return m
	})
}

// ResolvedLinks are the link buttons for one app: the cluster's links first,
// then the global ones, three at most.
func (c Config) ResolvedLinks(cl Cluster, app, namespace string) []Link {
	vars := map[string]string{}
	for k, v := range cl.Vars {
		vars[k] = v
	}
	for k, v := range map[string]string{"app": app, "namespace": namespace, "cluster": cl.Name, "env": cl.Env, "region": cl.Region} {
		vars[k] = v
	}
	var out []Link
	for _, l := range append(append([]Link{}, cl.Links...), c.Links...) {
		if len(out) == 3 {
			break
		}
		out = append(out, Link{Text: l.Text, URL: expand(l.URL, vars)})
	}
	return out
}

// CommitURL links a commit, or "" without a repo and commit.
func (c Config) CommitURL(repo, commit string) string {
	if repo == "" || commit == "" {
		return ""
	}
	return expandRaw(c.SCM.CommitURL, map[string]string{"repo": repo, "commit": commit})
}

// PRURL links a pull request, or "" without a repo and number.
func (c Config) PRURL(repo, pr string) string {
	if repo == "" || pr == "" {
		return ""
	}
	return expandRaw(c.SCM.PRURL, map[string]string{"repo": repo, "pr": pr})
}

// VersionURL links a version, or "" without a repo, version and template.
func (c Config) VersionURL(repo, version string) string {
	if repo == "" || version == "" || c.SCM.VersionURL == "" {
		return ""
	}
	return expandRaw(c.SCM.VersionURL, map[string]string{"repo": repo, "version": version})
}

// CompareURL links the changes between two versions or commits.
func (c Config) CompareURL(repo, from, to string) string {
	if repo == "" || from == "" || to == "" || c.SCM.CompareURL == "" {
		return ""
	}
	return expandRaw(c.SCM.CompareURL, map[string]string{"repo": repo, "from": from, "to": to})
}

// expandRaw fills placeholders without escaping, so repo names keep their slash.
func expandRaw(tmpl string, vars map[string]string) string {
	return placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		if v, ok := vars[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}
