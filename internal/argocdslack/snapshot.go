package argocdslack

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Snapshot is the part of an Argo CD Application the state machines use.
type Snapshot struct {
	App             string
	Namespace       string // destination namespace
	ResourceVersion string
	Deleting        bool

	Health        string
	HealthMessage string
	HealthSince   time.Time
	Sync          string
	Revision      string

	Op *Operation

	Repo        string
	Commit      string
	Version     string
	Images      []string
	Actor       string
	ApprovedBy  string
	PR          string
	CommitTitle string
	RunURL      string
	Changes     []Change

	Culprit   *ResourceRef
	Resources []ResourceRef
}

// Operation is the app's last or current sync.
type Operation struct {
	Phase      string
	Message    string
	StartedAt  time.Time
	FinishedAt time.Time
	Revision   string
	Automated  bool
	Username   string
	RetryCount int
	Results    []ResourceResult
}

// ResourceResult is one resource's outcome in a sync.
type ResourceResult struct {
	Kind      string
	Namespace string
	Name      string
	Status    string
	HookType  string
	HookPhase string
	Message   string
}

// ResourceRef names a Kubernetes object.
type ResourceRef struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

func (r ResourceRef) String() string {
	if r.Namespace == "" {
		return r.Kind + " " + r.Name
	}
	return r.Kind + " " + r.Namespace + "/" + r.Name
}

// Unhealthy reports a state that can open an incident.
func (s Snapshot) Unhealthy() bool {
	switch s.Health {
	case "Degraded", "Missing", "Unknown":
		return true
	}
	return false
}

// AutoscalerOnly is true when the only culprit is an HPA, whose metric gaps
// get a longer grace.
func (s Snapshot) AutoscalerOnly() bool {
	return s.Culprit != nil && s.Culprit.Kind == "HorizontalPodAutoscaler"
}

var causedBy = regexp.MustCompile(`Caused by ([^/\s]*)/?([A-Za-z]+):([^/\s]*)/(\S+)`)

type rawApp struct {
	Metadata struct {
		Name              string            `json:"name"`
		ResourceVersion   string            `json:"resourceVersion"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Destination struct {
			Namespace string `json:"namespace"`
		} `json:"destination"`
		Source *struct {
			RepoURL string `json:"repoURL"`
		} `json:"source"`
		Sources []struct {
			RepoURL string `json:"repoURL"`
		} `json:"sources"`
	} `json:"spec"`
	Status struct {
		Health struct {
			Status             string    `json:"status"`
			Message            string    `json:"message"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"health"`
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Summary struct {
			Images []string `json:"images"`
		} `json:"summary"`
		Resources []struct {
			Group     string `json:"group"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"resources"`
		OperationState *struct {
			Phase      string     `json:"phase"`
			Message    string     `json:"message"`
			StartedAt  time.Time  `json:"startedAt"`
			FinishedAt *time.Time `json:"finishedAt"`
			RetryCount int        `json:"retryCount"`
			Operation  struct {
				InitiatedBy struct {
					Username  string `json:"username"`
					Automated bool   `json:"automated"`
				} `json:"initiatedBy"`
				Sync struct {
					Revision string `json:"revision"`
				} `json:"sync"`
			} `json:"operation"`
			SyncResult *struct {
				Revision  string `json:"revision"`
				Resources []struct {
					Kind      string `json:"kind"`
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
					Status    string `json:"status"`
					HookType  string `json:"hookType"`
					HookPhase string `json:"hookPhase"`
					Message   string `json:"message"`
				} `json:"resources"`
			} `json:"syncResult"`
		} `json:"operationState"`
	} `json:"status"`
}

// ParseApplication reads an Application object. ann names the annotations
// that carry deploy metadata; without them the repo and commit come from the
// Application's own source and sync revision.
func ParseApplication(b []byte, ann Annotations) (Snapshot, error) {
	var a rawApp
	if err := json.Unmarshal(b, &a); err != nil {
		return Snapshot{}, err
	}
	get := func(k string) string {
		if k == "" {
			return ""
		}
		v := strings.TrimSpace(a.Metadata.Annotations[k])
		// An ApplicationSet leaves "{{key}}" when config.yaml lacks the key.
		if strings.Contains(v, "{{") || v == "<no value>" {
			return ""
		}
		return v
	}
	s := Snapshot{
		App:             a.Metadata.Name,
		Namespace:       a.Spec.Destination.Namespace,
		ResourceVersion: a.Metadata.ResourceVersion,
		Deleting:        a.Metadata.DeletionTimestamp != nil,
		Health:          a.Status.Health.Status,
		HealthMessage:   a.Status.Health.Message,
		HealthSince:     a.Status.Health.LastTransitionTime,
		Sync:            a.Status.Sync.Status,
		Revision:        a.Status.Sync.Revision,
		Repo:            repoName(get(ann.Repo)),
		Commit:          get(ann.Commit),
		Version:         get(ann.Version),
		Images:          a.Status.Summary.Images,
		Actor:           get(ann.Actor),
		ApprovedBy:      get(ann.ApprovedBy),
		PR:              strings.TrimPrefix(get(ann.PR), "#"),
		CommitTitle:     get(ann.Title),
		RunURL:          get(ann.RunURL),
		Changes:         parseChanges(get(ann.Changes)),
	}
	if s.Repo == "" && s.Commit == "" {
		switch {
		case a.Spec.Source != nil:
			s.Repo = repoName(a.Spec.Source.RepoURL)
		case len(a.Spec.Sources) > 0:
			s.Repo = repoName(a.Spec.Sources[0].RepoURL)
		}
		s.Commit = a.Status.Sync.Revision
	}
	if m := causedBy.FindStringSubmatch(s.HealthMessage); m != nil {
		s.Culprit = &ResourceRef{Group: m[1], Kind: m[2], Namespace: m[3], Name: m[4]}
	}
	for _, r := range a.Status.Resources {
		s.Resources = append(s.Resources, ResourceRef{Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name})
	}
	if st := a.Status.OperationState; st != nil && !st.StartedAt.IsZero() {
		op := &Operation{
			Phase:      st.Phase,
			Message:    st.Message,
			StartedAt:  st.StartedAt,
			Revision:   st.Operation.Sync.Revision,
			Automated:  st.Operation.InitiatedBy.Automated,
			Username:   st.Operation.InitiatedBy.Username,
			RetryCount: st.RetryCount,
		}
		if st.FinishedAt != nil {
			op.FinishedAt = *st.FinishedAt
		}
		if st.SyncResult != nil {
			if op.Revision == "" {
				op.Revision = st.SyncResult.Revision
			}
			for _, r := range st.SyncResult.Resources {
				op.Results = append(op.Results, ResourceResult(r))
			}
		}
		s.Op = op
	}
	return s, nil
}

// parseChanges reads a JSON array of changes, plain or base64-encoded so it
// survives YAML templating. Anything else counts as no changes.
func parseChanges(v string) []Change {
	if v == "" {
		return nil
	}
	b := []byte(v)
	if v[0] != '[' {
		var err error
		if b, err = base64.StdEncoding.DecodeString(v); err != nil {
			return nil
		}
	}
	var out []Change
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	keep := out[:0]
	for _, c := range out {
		if c.Title = strings.TrimSpace(c.Title); c.Title != "" {
			keep = append(keep, c)
		}
	}
	return keep
}

// FailedResults are hook or resource failures worth showing.
func (op *Operation) FailedResults() []ResourceResult {
	if op == nil {
		return nil
	}
	var out []ResourceResult
	for _, r := range op.Results {
		if r.HookPhase == "Failed" || r.HookPhase == "Error" || r.Status == "SyncFailed" {
			out = append(out, r)
		}
	}
	return out
}

// Workloads are the app's pod-owning resources, for rollout progress.
func (s Snapshot) Workloads() []ResourceRef {
	var out []ResourceRef
	for _, r := range s.Resources {
		switch r.Kind {
		case "Deployment", "StatefulSet", "DaemonSet":
			out = append(out, r)
		}
	}
	return out
}

// repoName turns a Git URL or owner/name into owner/name; anything else is
// returned as given.
func repoName(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if j := strings.Index(u, "@"); j >= 0 {
			u = u[j+1:]
		}
		if j := strings.Index(u, "/"); j >= 0 {
			return u[j+1:]
		}
		return u
	}
	if i := strings.Index(u, ":"); i >= 0 && strings.Contains(u[:i], "@") {
		return u[i+1:]
	}
	return u
}
