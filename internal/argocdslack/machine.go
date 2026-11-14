package argocdslack

import (
	"fmt"
	"strings"
	"time"
)

// Step reports what Advance changed and what it needs from the cluster.
type Step struct {
	NewDeploy          bool
	NeedDeployReason   bool
	NeedIncidentReason bool
	NeedProgress       bool
	DeployFailed       bool
	IncidentResolved   bool
	Blip               bool
}

// Advance moves one app's deploy and incident state forward from a snapshot.
// baseline is true on a cluster's first poll, when existing failures are
// summarised once instead of alerted one by one.
func Advance(cfg Config, rec *AppRecord, s Snapshot, now time.Time, baseline bool) Step {
	var st Step
	rec.LastSeen, rec.MissingPolls = now, 0
	rec.Namespace = s.Namespace
	rec.Health, rec.Sync = s.Health, s.Sync
	switch {
	case s.Unhealthy():
		if rec.UnhealthySince.IsZero() {
			rec.UnhealthySince = now
			if !s.HealthSince.IsZero() && s.HealthSince.Before(now) {
				rec.UnhealthySince = s.HealthSince
			}
		}
	case healthyish(s.Health):
		rec.UnhealthySince = time.Time{}
	}

	if s.Op != nil && s.Op.StartedAt.After(rec.OpStartedAt) {
		first := rec.OpStartedAt.IsZero()
		rec.OpStartedAt = s.Op.StartedAt
		switch {
		case first && baseline && s.Op.Phase != "Running":
			if failedPhase(s.Op.Phase) {
				rec.Deploy = baselineFailedDeploy(rec, s)
			}
		default:
			startDeploy(rec, s, now)
			st.NewDeploy = true
		}
	}

	if d := rec.Deploy; d != nil && !d.Baseline {
		advanceDeploy(cfg, rec, d, s, now, &st)
	} else if d != nil && d.Baseline {
		d.Health = s.Health
	}

	allowOpen := !ownsHealth(cfg, rec.Deploy, now)
	advanceIncident(cfg, rec, s, now, baseline, allowOpen, &st)

	if d := rec.Deploy; d != nil && (d.Quiet || !d.Dirty) && (d.State == DeployHealthy || d.State == DeployRecovered || d.State == DeploySuperseded) &&
		now.Sub(d.FinishedAt) > cfg.Grace.DeployWindow.Duration {
		if !d.Quiet || d.Msg.Posted() {
			rec.LastDeploy = d
		}
		rec.Deploy = nil
	}
	return st
}

func healthyish(h string) bool { return h == "Healthy" || h == "Suspended" }

func failedPhase(p string) bool { return p == "Failed" || p == "Error" }

// ownsHealth: unhealthiness during a deploy, or until DeployWindow after it,
// belongs to the deploy message. A failed deploy owns it until the next one.
func ownsHealth(cfg Config, d *Deploy, now time.Time) bool {
	if d == nil {
		return false
	}
	switch d.State {
	case DeploySyncing, DeployRolling, DeployStalled, DeployUnhealthy, DeployFailed:
		return true
	case DeployHealthy, DeployRecovered:
		return now.Sub(d.FinishedAt) < cfg.Grace.DeployWindow.Duration
	}
	return false
}

func entry(at time.Time, format string, args ...any) Entry {
	return Entry{At: at, Text: fmt.Sprintf(format, args...)}
}

func startDeploy(rec *AppRecord, s Snapshot, now time.Time) {
	if old := rec.Deploy; old != nil {
		if !old.State.Final() {
			old.State = DeploySuperseded
			old.FinishedAt = now
			old.Timeline = append(old.Timeline, entry(now, "Superseded by a newer sync."))
			old.Dirty = true
		}
		if !old.Quiet || old.Msg.Posted() {
			rec.LastDeploy = old
		}
	}
	op := s.Op
	d := &Deploy{
		ID:        fmt.Sprintf("%s/%s/%d", rec.Cluster, rec.App, op.StartedAt.Unix()),
		State:     DeploySyncing,
		StartedAt: op.StartedAt,
		Revision:  op.Revision,
		Repo:      s.Repo,
		Commit:    s.Commit,
		Images:    s.Images,
		Actor:     s.Actor,
		PR:        s.PR,
		Title:     s.CommitTitle,
		RunURL:    s.RunURL,
		Automated: op.Automated,
		Username:  op.Username,
		Attempts:  op.RetryCount + 1,
		Dirty:     true,
	}
	who := "automated"
	if !op.Automated && op.Username != "" {
		who = "by " + op.Username
	}
	d.Timeline = append(d.Timeline, entry(op.StartedAt, "Sync started (%s).", who))
	if last := rec.LastDeploy; last != nil && s.Commit != "" && last.Commit == s.Commit &&
		(last.State == DeployHealthy || last.State == DeployRecovered) {
		d.Quiet = true
	}
	rec.Deploy = d
}

func baselineFailedDeploy(rec *AppRecord, s Snapshot) *Deploy {
	op := s.Op
	return &Deploy{
		ID:         fmt.Sprintf("%s/%s/%d", rec.Cluster, rec.App, op.StartedAt.Unix()),
		State:      DeployFailed,
		StartedAt:  op.StartedAt,
		FinishedAt: op.FinishedAt,
		Revision:   op.Revision,
		Repo:       s.Repo,
		Commit:     s.Commit,
		Attempts:   op.RetryCount + 1,
		OpMessage:  op.Message,
		Failures:   describeFailures(op),
		Health:     s.Health,
		Baseline:   true,
	}
}

func advanceDeploy(cfg Config, rec *AppRecord, d *Deploy, s Snapshot, now time.Time, st *Step) {
	op := s.Op
	if op == nil || !op.StartedAt.Equal(d.StartedAt) {
		return
	}
	set := func(state DeployState, format string, args ...any) {
		if d.State == state {
			return
		}
		d.State = state
		d.Dirty = true
		if format != "" {
			d.Timeline = append(d.Timeline, entry(now, format, args...))
		}
	}
	switch {
	case op.Phase == "Running" || op.Phase == "Terminating":
		if n := op.RetryCount + 1; n != d.Attempts {
			d.Attempts, d.Dirty = n, true
			d.Timeline = append(d.Timeline, entry(now, "Attempt %d.", n))
		}
		set(DeploySyncing, "")
	case failedPhase(op.Phase):
		if d.State != DeployFailed {
			d.FinishedAt = op.FinishedAt
			if d.FinishedAt.IsZero() {
				d.FinishedAt = now
			}
			d.Attempts = op.RetryCount + 1
			d.OpMessage = op.Message
			d.Failures = describeFailures(op)
			d.Quiet = false
			set(DeployFailed, "Sync failed after %d attempt%s: %s", d.Attempts, plural(d.Attempts), firstLine(op.Message))
			st.NeedDeployReason, st.DeployFailed = true, true
		}
		if d.Health != s.Health {
			d.Health, d.Dirty = s.Health, true
		}
	case op.Phase == "Succeeded":
		if d.SyncedAt.IsZero() {
			d.SyncedAt = op.FinishedAt
			if d.SyncedAt.IsZero() {
				d.SyncedAt = now
			}
			d.Dirty = true
			d.Timeline = append(d.Timeline, entry(d.SyncedAt, "Synced in %s.", short(d.SyncedAt.Sub(d.StartedAt))))
		}
		switch {
		case healthyish(s.Health):
			d.DegradedAt = time.Time{}
			switch d.State {
			case DeploySyncing, DeployRolling, DeployStalled:
				if len(s.Images) > 0 {
					d.Images = s.Images
				}
				d.HealthyAt = now
				if s.HealthSince.After(d.SyncedAt) && s.HealthSince.Before(now) {
					d.HealthyAt = s.HealthSince
				}
				d.FinishedAt = d.HealthyAt
				d.Progress = ""
				set(DeployHealthy, "Healthy %s after the sync started.", short(d.HealthyAt.Sub(d.StartedAt)))
				if last := rec.LastDeploy; d.Quiet && last != nil && last.Msg.Posted() {
					last.Timeline = append(last.Timeline, entry(now, "Resynced %s with no new commit; healthy.", slackTime(d.StartedAt, "{time}")))
					last.Dirty = true
				}
			case DeployUnhealthy:
				d.FinishedAt = now
				set(DeployRecovered, "Healthy again after %s.", short(now.Sub(d.DegradedAt)))
			}
		case s.Unhealthy():
			if d.DegradedAt.IsZero() {
				d.DegradedAt = now
			}
			grace := cfg.Grace.Incident.Duration
			if s.AutoscalerOnly() {
				grace = cfg.Grace.Autoscaler.Duration
			}
			if d.State != DeployUnhealthy && now.Sub(d.DegradedAt) >= grace {
				d.Quiet = false
				set(DeployUnhealthy, "%s for %s after the sync.", s.Health, short(grace))
				st.NeedDeployReason = true
			} else if d.State == DeploySyncing {
				set(DeployRolling, "")
			}
		default:
			d.DegradedAt = time.Time{}
			switch d.State {
			case DeploySyncing:
				set(DeployRolling, "Rolling out.")
			case DeployRolling:
				if now.Sub(d.SyncedAt) > cfg.Grace.Stall.Duration {
					set(DeployStalled, "Still rolling out after %s.", short(cfg.Grace.Stall.Duration))
				}
			}
			if d.State == DeployRolling || d.State == DeployStalled {
				st.NeedProgress = true
			}
		}
	}
	if d.State == DeployHealthy && rec.LastDeploy != nil && rec.LastDeploy.State == DeployFailed && rec.LastDeploy.FixedBy == "" {
		rec.LastDeploy.FixedBy = fmt.Sprintf("Fixed by the %s deploy%s.", slackTime(d.HealthyAt, "{time}"), commitSuffix(d.Commit))
		rec.LastDeploy.Dirty = true
	}
}

func commitSuffix(commit string) string {
	if commit == "" {
		return ""
	}
	return " of `" + shortSHA(commit) + "`"
}

func advanceIncident(cfg Config, rec *AppRecord, s Snapshot, now time.Time, baseline, allowOpen bool, st *Step) {
	inc := rec.Incident
	switch {
	case s.Unhealthy():
		if inc == nil {
			if !allowOpen {
				return
			}
			if last := rec.LastIncident; last != nil && last.State == IncidentResolved && last.Msg.Posted() &&
				now.Sub(last.ResolvedAt) <= cfg.Grace.Flap.Duration {
				last.State, last.ResolvedAt = IncidentOpen, time.Time{}
				last.Flaps++
				last.Health = s.Health
				last.Dirty = true
				last.Timeline = append(last.Timeline, entry(now, "%s again (flap %d).", s.Health, last.Flaps))
				rec.Incident, rec.LastIncident = last, nil
				st.NeedIncidentReason = true
				return
			}
			first := rec.UnhealthySince
			if first.IsZero() {
				first = now
			}
			inc = &Incident{
				ID:               fmt.Sprintf("%s/%s/%d", rec.Cluster, rec.App, first.Unix()),
				State:            IncidentPending,
				Health:           s.Health,
				FirstUnhealthyAt: first,
			}
			inc.Timeline = append(inc.Timeline, entry(first, "%s: %s", s.Health, healthText(s)))
			rec.Incident = inc
		}
		inc.Health = s.Health
		switch inc.State {
		case IncidentPending:
			grace := cfg.Grace.Incident.Duration
			if s.AutoscalerOnly() {
				grace = cfg.Grace.Autoscaler.Duration
			}
			if baseline || now.Sub(inc.FirstUnhealthyAt) >= grace {
				inc.State, inc.OpenedAt, inc.Baseline = IncidentOpen, now, baseline
				inc.Dirty = true
				if !baseline {
					inc.Timeline = append(inc.Timeline, entry(now, "Still %s after %s, so this incident is open.", strings.ToLower(s.Health), short(grace)))
				}
				st.NeedIncidentReason = true
			}
		case IncidentOpen:
			if now.Sub(inc.ReasonAt) > 10*time.Minute {
				st.NeedIncidentReason = true
			}
		}
	case healthyish(s.Health):
		if inc == nil {
			return
		}
		switch inc.State {
		case IncidentPending:
			rec.Incident = nil
			st.Blip = true
		case IncidentOpen:
			inc.State, inc.ResolvedAt = IncidentResolved, now
			inc.Dirty = true
			inc.Timeline = append(inc.Timeline, entry(now, "Healthy again after %s.", short(inc.Downtime(now))))
			rec.LastIncident, rec.Incident = inc, nil
			st.IncidentResolved = true
		}
	}
}

// Remove closes whatever is open when an app disappears from Argo CD.
func Remove(rec *AppRecord, now time.Time) {
	if inc := rec.Incident; inc != nil {
		if inc.State == IncidentOpen {
			inc.State, inc.ResolvedAt, inc.CloseNote = IncidentClosed, now, "App removed from Argo CD."
			inc.Dirty = true
			inc.Timeline = append(inc.Timeline, entry(now, "App removed from Argo CD."))
			rec.LastIncident = inc
		}
		rec.Incident = nil
	}
	if d := rec.Deploy; d != nil && !d.State.Final() {
		d.State, d.FinishedAt = DeploySuperseded, now
		d.Timeline = append(d.Timeline, entry(now, "App removed from Argo CD."))
		d.Dirty = true
	}
}

func describeFailures(op *Operation) []string {
	var out []string
	for _, r := range op.FailedResults() {
		what := r.Kind
		if r.HookType != "" {
			what = r.HookType + " " + strings.ToLower(r.Kind)
		}
		out = append(out, fmt.Sprintf("*%s* `%s`: %s", what, r.Name, lowerFirst(trimSentence(r.Message))))
	}
	if len(out) == 0 && op.Message != "" {
		out = append(out, firstLine(op.Message))
	}
	return out
}

func healthText(s Snapshot) string {
	if s.Culprit != nil {
		return s.Culprit.String()
	}
	if s.HealthMessage != "" {
		return s.HealthMessage
	}
	return "no detail from Argo CD"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func trimSentence(s string) string { return strings.TrimSuffix(strings.TrimSpace(firstLine(s)), ".") }

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// short formats durations the way people say them: 42s, 6m, 1h 12m, 2d 21h.
func short(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		if s := int(d.Seconds()) % 60; m < 10 && s > 0 {
			return fmt.Sprintf("%dm %02ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	case d < 48*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		days, h := int(d.Hours())/24, int(d.Hours())%24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd %dh", days, h)
	}
}

// slackTime renders a time every reader sees in their own zone.
func slackTime(t time.Time, format string) string {
	return fmt.Sprintf("<!date^%d^%s|%s>", t.Unix(), format, t.UTC().Format("15:04 UTC"))
}
