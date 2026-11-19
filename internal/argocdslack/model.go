package argocdslack

import "time"

// AppRecord is everything stored per app per cluster, one DynamoDB item.
type AppRecord struct {
	Cluster string `json:"cluster"`
	App     string `json:"app"`

	Namespace      string    `json:"namespace,omitempty"`
	Health         string    `json:"health,omitempty"`
	Sync           string    `json:"sync,omitempty"`
	UnhealthySince time.Time `json:"unhealthy_since,omitzero"`
	OpStartedAt    time.Time `json:"op_started_at,omitzero"`
	LastSeen       time.Time `json:"last_seen,omitzero"`
	MissingPolls   int       `json:"missing_polls,omitempty"`

	Deploy       *Deploy   `json:"deploy,omitempty"`
	LastDeploy   *Deploy   `json:"last_deploy,omitempty"`
	Incident     *Incident `json:"incident,omitempty"`
	LastIncident *Incident `json:"last_incident,omitempty"`

	Version int64 `json:"-"`
}

// Message is a posted Slack message.
type Message struct {
	Channel string `json:"channel,omitempty"`
	TS      string `json:"ts,omitempty"`
}

func (m Message) Posted() bool { return m.TS != "" }

// Entry is one thread reply; Posted ones are already in Slack.
type Entry struct {
	At     time.Time `json:"at"`
	Text   string    `json:"text"`
	Posted bool      `json:"posted,omitempty"`
}

// DeployState is where a deploy message stands.
type DeployState string

const (
	DeploySyncing    DeployState = "syncing"
	DeployRolling    DeployState = "rolling"
	DeployStalled    DeployState = "stalled"
	DeployHealthy    DeployState = "healthy"
	DeployFailed     DeployState = "failed"
	DeployUnhealthy  DeployState = "unhealthy"
	DeployRecovered  DeployState = "recovered"
	DeploySuperseded DeployState = "superseded"
)

// Final states never change again, except a failed deploy's fix link.
func (s DeployState) Final() bool {
	switch s {
	case DeployHealthy, DeployFailed, DeployRecovered, DeploySuperseded:
		return true
	}
	return false
}

// Deploy is one sync operation of one app.
type Deploy struct {
	ID          string      `json:"id"`
	State       DeployState `json:"state"`
	StartedAt   time.Time   `json:"started_at"`
	OpStartedAt time.Time   `json:"op_started_at,omitzero"` // a later sync of the same commit it follows
	SyncedAt    time.Time   `json:"synced_at,omitzero"`
	HealthyAt   time.Time   `json:"healthy_at,omitzero"`
	FinishedAt  time.Time   `json:"finished_at,omitzero"`
	DegradedAt  time.Time   `json:"degraded_at,omitzero"`

	Revision  string   `json:"revision,omitempty"`
	Repo      string   `json:"repo,omitempty"`
	Commit    string   `json:"commit,omitempty"`
	Images    []string `json:"images,omitempty"`
	Actor     string   `json:"actor,omitempty"`
	PR        string   `json:"pr,omitempty"`
	Title     string   `json:"title,omitempty"`
	RunURL    string   `json:"run_url,omitempty"`
	Automated bool     `json:"automated,omitempty"`
	Username  string   `json:"username,omitempty"`

	Attempts  int      `json:"attempts,omitempty"`
	Progress  string   `json:"progress,omitempty"` // "2/4"
	Failures  []string `json:"failures,omitempty"`
	OpMessage string   `json:"op_message,omitempty"`
	Reason    *Reason  `json:"reason,omitempty"`
	Health    string   `json:"health,omitempty"` // current app health, for failed deploys
	FixedBy   string   `json:"fixed_by,omitempty"`
	Baseline  bool     `json:"baseline,omitempty"`
	Quiet     bool     `json:"quiet,omitempty"` // resync at an unchanged commit: thread note only

	Msg      Message `json:"msg,omitzero"`
	Timeline []Entry `json:"timeline,omitempty"`
	Dirty    bool    `json:"dirty,omitempty"`
	Muted    bool    `json:"muted,omitempty"`
	MuteNote string  `json:"mute_note,omitempty"`
	Mention  string  `json:"mention,omitempty"`
}

// IncidentState is where an incident message stands.
type IncidentState string

const (
	IncidentPending  IncidentState = "pending"
	IncidentOpen     IncidentState = "open"
	IncidentResolved IncidentState = "resolved"
	IncidentClosed   IncidentState = "closed"
)

// Incident is one stretch of an app being unhealthy outside a deploy.
type Incident struct {
	ID               string        `json:"id"`
	State            IncidentState `json:"state"`
	Health           string        `json:"health,omitempty"`
	FirstUnhealthyAt time.Time     `json:"first_unhealthy_at"`
	OpenedAt         time.Time     `json:"opened_at,omitzero"`
	ResolvedAt       time.Time     `json:"resolved_at,omitzero"`
	ReasonAt         time.Time     `json:"reason_at,omitzero"`
	Reason           *Reason       `json:"reason,omitempty"`
	Flaps            int           `json:"flaps,omitempty"`
	AckBy            string        `json:"ack_by,omitempty"`
	Baseline         bool          `json:"baseline,omitempty"`
	CloseNote        string        `json:"close_note,omitempty"`

	Msg      Message `json:"msg,omitzero"`
	Timeline []Entry `json:"timeline,omitempty"`
	Dirty    bool    `json:"dirty,omitempty"`
	Muted    bool    `json:"muted,omitempty"`
	MuteNote string  `json:"mute_note,omitempty"`
	Mention  string  `json:"mention,omitempty"`
}

// Downtime is from the first unhealthy reading to recovery (or now).
func (i *Incident) Downtime(now time.Time) time.Duration {
	end := i.ResolvedAt
	if end.IsZero() {
		end = now
	}
	return end.Sub(i.FirstUnhealthyAt)
}

// Reason explains a failure in human terms.
type Reason struct {
	Category string   `json:"category"`
	Resource string   `json:"resource,omitempty"` // "Deployment apps/api-server"
	Ready    string   `json:"ready,omitempty"`    // "0/2"
	Summary  string   `json:"summary"`            // "CrashLoopBackOff · 259 restarts · last exit 1"
	Details  []string `json:"details,omitempty"`  // event lines
	Excerpt  string   `json:"excerpt,omitempty"`  // one redacted log line
	Image    string   `json:"image,omitempty"`
	Pods     int      `json:"pods,omitempty"`
}

// Reason categories.
const (
	CatCrashLoop  = "crashloop"
	CatImagePull  = "imagepull"
	CatOOM        = "oom"
	CatConfig     = "config"
	CatProbe      = "probe"
	CatPending    = "pending"
	CatAutoscaler = "autoscaler"
	CatHook       = "hook"
	CatSync       = "sync"
	CatMissing    = "missing"
	CatUnknown    = "unknown"
)

// ClusterMeta is per-cluster bookkeeping, one DynamoDB item.
type ClusterMeta struct {
	Cluster         string            `json:"cluster"`
	BaselineDone    bool              `json:"baseline_done,omitempty"`
	Baseline        Message           `json:"baseline,omitzero"`
	BaselineRows    []BaselineRow     `json:"baseline_rows,omitempty"`
	BaselineDirty   bool              `json:"baseline_dirty,omitempty"`
	LastPoll        time.Time         `json:"last_poll,omitzero"`
	FailingSince    time.Time         `json:"failing_since,omitzero"`
	Unreachable     Message           `json:"unreachable,omitzero"`
	Stats           map[string]*Stats `json:"stats,omitempty"` // by local date
	ResourceVersion string            `json:"resource_version,omitempty"`

	Version int64 `json:"-"`
}

// BaselineRow is one already-failing app in the cut-over message.
type BaselineRow struct {
	App        string    `json:"app"`
	Kind       string    `json:"kind"` // incident | deploy
	Summary    string    `json:"summary"`
	Detail     string    `json:"detail,omitempty"` // a log line or the first failure
	Since      time.Time `json:"since,omitzero"`
	Resolved   bool      `json:"resolved,omitempty"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
}

// Stats are daily counters for the digest.
type Stats struct {
	Deploys  int `json:"deploys,omitempty"`
	Failed   int `json:"failed,omitempty"`
	Resolved int `json:"resolved,omitempty"`
	Blips    int `json:"blips,omitempty"`
}
