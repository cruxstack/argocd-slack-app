package argocdslack

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC)

func snap(health string, op *Operation) Snapshot {
	s := Snapshot{App: "api", Namespace: "apps", Health: health, Sync: "Synced", Op: op, Repo: "acme/api", Commit: "5e6f7a80000"}
	if health == "Degraded" {
		s.HealthMessage = "Caused by apps/Deployment:apps/api-server"
		s.Culprit = &ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "apps", Name: "api-server"}
	}
	return s
}

func at(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

func TestIncidentBlipIsDropped(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	Advance(cfg, rec, snap("Healthy", nil), at(0), false)
	Advance(cfg, rec, snap("Degraded", nil), at(1), false)
	if rec.Incident == nil || rec.Incident.State != IncidentPending {
		t.Fatalf("want pending incident, got %+v", rec.Incident)
	}
	st := Advance(cfg, rec, snap("Healthy", nil), at(3), false)
	if rec.Incident != nil || !st.Blip {
		t.Fatalf("blip should be dropped, got %+v %+v", rec.Incident, st)
	}
}

func TestIncidentOpensAfterGraceAndResolves(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	Advance(cfg, rec, snap("Degraded", nil), at(0), false)
	Advance(cfg, rec, snap("Degraded", nil), at(4), false)
	if rec.Incident.State != IncidentPending {
		t.Fatal("opened before the 5 minute grace")
	}
	st := Advance(cfg, rec, snap("Degraded", nil), at(5), false)
	if rec.Incident.State != IncidentOpen || !rec.Incident.Dirty || !st.NeedIncidentReason {
		t.Fatalf("want open incident, got %+v", rec.Incident)
	}
	// Progressing holds the incident open.
	Advance(cfg, rec, snap("Progressing", nil), at(20), false)
	if rec.Incident == nil {
		t.Fatal("progressing should not resolve")
	}
	st = Advance(cfg, rec, snap("Healthy", nil), at(30), false)
	if rec.Incident != nil || rec.LastIncident.State != IncidentResolved || !st.IncidentResolved {
		t.Fatalf("want resolved, got %+v", rec.LastIncident)
	}
	if got := rec.LastIncident.Downtime(at(30)); got != 30*time.Minute {
		t.Fatalf("downtime %s", got)
	}
}

func TestAutoscalerOnlyGetsLongerGrace(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "worker", OpStartedAt: t0.Add(-time.Hour)}
	s := snap("Degraded", nil)
	s.Culprit = &ResourceRef{Kind: "HorizontalPodAutoscaler", Namespace: "worker", Name: "worker-api"}
	Advance(cfg, rec, s, at(0), false)
	Advance(cfg, rec, s, at(6), false)
	if rec.Incident.State != IncidentPending {
		t.Fatal("HPA-only degradation opened before 10 minutes")
	}
	Advance(cfg, rec, s, at(10), false)
	if rec.Incident.State != IncidentOpen {
		t.Fatal("HPA-only degradation should open at 10 minutes")
	}
}

func TestFlapReopensSameIncident(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	Advance(cfg, rec, snap("Degraded", nil), at(0), false)
	Advance(cfg, rec, snap("Degraded", nil), at(5), false)
	rec.Incident.Msg = Message{Channel: "C", TS: "1.1"}
	Advance(cfg, rec, snap("Healthy", nil), at(8), false)
	Advance(cfg, rec, snap("Degraded", nil), at(12), false)
	if rec.Incident == nil || rec.Incident.Msg.TS != "1.1" || rec.Incident.Flaps != 1 || rec.Incident.State != IncidentOpen {
		t.Fatalf("want reopened incident on the same message, got %+v", rec.Incident)
	}
}

func TestNoRepeatOnGitopsCommit(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	s := snap("Degraded", nil)
	Advance(cfg, rec, s, at(0), false)
	Advance(cfg, rec, s, at(6), false)
	rec.Incident.Msg = Message{Channel: "C", TS: "1.1"}
	rec.Incident.Dirty = false
	for i := 0; i < 5; i++ {
		s.Revision = strings.Repeat("a", i+1) // the shared repo moves on
		Advance(cfg, rec, s, at(7+i*30), false)
	}
	if rec.Incident.Msg.TS != "1.1" || rec.LastIncident != nil {
		t.Fatalf("incident must stay one message, got %+v", rec.Incident)
	}
}

func op(phase string, started time.Time, finished time.Time) *Operation {
	return &Operation{Phase: phase, StartedAt: started, FinishedAt: finished, Automated: true, Revision: "abc"}
}

func TestDeployLifecycle(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	st := Advance(cfg, rec, snap("Healthy", op("Running", at(0), time.Time{})), at(0), false)
	if !st.NewDeploy || rec.Deploy.State != DeploySyncing {
		t.Fatalf("want syncing deploy, got %+v", rec.Deploy)
	}
	st = Advance(cfg, rec, snap("Progressing", op("Succeeded", at(0), at(0).Add(5*time.Second))), at(1), false)
	if rec.Deploy.State != DeployRolling || !st.NeedProgress {
		t.Fatalf("want rolling, got %s", rec.Deploy.State)
	}
	Advance(cfg, rec, snap("Healthy", op("Succeeded", at(0), at(0).Add(5*time.Second))), at(2), false)
	if rec.Deploy.State != DeployHealthy || rec.Deploy.FinishedAt != at(2) {
		t.Fatalf("want healthy, got %+v", rec.Deploy)
	}
	// Degraded inside the deploy window belongs to the deploy, not a new incident.
	Advance(cfg, rec, snap("Degraded", op("Succeeded", at(0), at(0).Add(5*time.Second))), at(4), false)
	if rec.Incident != nil {
		t.Fatal("incident opened inside the deploy window")
	}
	// After the window (and once rendered) the deploy is archived.
	rec.Deploy.Dirty = false
	Advance(cfg, rec, snap("Healthy", op("Succeeded", at(0), at(0).Add(5*time.Second))), at(20), false)
	if rec.Deploy != nil || rec.LastDeploy == nil {
		t.Fatalf("deploy should be archived, got %+v", rec.Deploy)
	}
}

func TestDeployCarriesOnThroughASyncOfTheSameCommit(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	first := op("Succeeded", at(0), at(0).Add(4*time.Second))
	Advance(cfg, rec, snap("Healthy", op("Running", at(0), time.Time{})), at(0), false)
	Advance(cfg, rec, snap("Progressing", first), at(0).Add(5*time.Second), false)
	id := rec.Deploy.ID

	// Argo CD's self-heal re-applies a few resources a second later, at a
	// revision another app's commit moved on.
	heal := op("Running", at(0).Add(6*time.Second), time.Time{})
	heal.Revision = "def"
	st := Advance(cfg, rec, snap("Progressing", heal), at(0).Add(6*time.Second), false)
	if st.NewDeploy || rec.Deploy.ID != id || rec.LastDeploy != nil {
		t.Fatalf("a sync of the same commit should carry on the deploy, got %+v", rec.Deploy)
	}
	if rec.Deploy.State != DeployRolling {
		t.Fatalf("the deploy should keep rolling, got %s", rec.Deploy.State)
	}
	heal.Phase, heal.FinishedAt = "Succeeded", at(0).Add(7*time.Second)
	Advance(cfg, rec, snap("Healthy", heal), at(1), false)
	if rec.Deploy.State != DeployHealthy || rec.Deploy.FinishedAt.Sub(rec.Deploy.StartedAt) != time.Minute {
		t.Fatalf("want one healthy deploy timed from the first sync, got %+v", rec.Deploy)
	}

	// A new commit still replaces a deploy in progress.
	rec = &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	Advance(cfg, rec, snap("Progressing", op("Running", at(2), time.Time{})), at(2), false)
	next := snap("Progressing", op("Running", at(3), time.Time{}))
	next.Commit = "9a8b7c60000"
	if st := Advance(cfg, rec, next, at(3), false); !st.NewDeploy || rec.LastDeploy == nil || rec.LastDeploy.State != DeploySuperseded {
		t.Fatalf("a new commit should replace the open deploy, got %+v", rec.LastDeploy)
	}
}

func TestDeployUnhealthyThenRecovered(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	o := op("Succeeded", at(0), at(0).Add(10*time.Second))
	Advance(cfg, rec, snap("Healthy", op("Running", at(0), time.Time{})), at(0), false)
	Advance(cfg, rec, snap("Degraded", o), at(1), false)
	st := Advance(cfg, rec, snap("Degraded", o), at(6), false)
	if rec.Deploy.State != DeployUnhealthy || !st.NeedDeployReason {
		t.Fatalf("want unhealthy, got %s", rec.Deploy.State)
	}
	if rec.Incident != nil {
		t.Fatal("unhealthy deploy must not open a separate incident")
	}
	Advance(cfg, rec, snap("Healthy", o), at(30), false)
	if rec.Deploy.State != DeployRecovered {
		t.Fatalf("want recovered, got %s", rec.Deploy.State)
	}
}

func TestFailedDeployFixedByNext(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "worker", OpStartedAt: t0.Add(-time.Hour)}
	failed := op("Failed", at(0), at(40))
	failed.Message = "one or more synchronization tasks completed unsuccessfully (retried 5 times)."
	failed.RetryCount = 4
	failed.Results = []ResourceResult{{Kind: "Job", Name: "worker-migrate", HookType: "PreSync", HookPhase: "Failed", Message: "Job has reached the specified backoff limit"}}
	Advance(cfg, rec, snap("Healthy", op("Running", at(0), time.Time{})), at(0), false)
	st := Advance(cfg, rec, snap("Missing", failed), at(41), false)
	if rec.Deploy.State != DeployFailed || !st.DeployFailed || rec.Deploy.Attempts != 5 {
		t.Fatalf("want failed after 5 attempts, got %+v", rec.Deploy)
	}
	if !strings.Contains(rec.Deploy.Failures[0], "worker-migrate") {
		t.Fatalf("failures: %v", rec.Deploy.Failures)
	}
	// Missing health is the failed deploy's story; no incident.
	Advance(cfg, rec, snap("Missing", failed), at(60), false)
	if rec.Incident != nil {
		t.Fatal("failed deploy owns the app's health")
	}
	next := op("Succeeded", at(90), at(91))
	Advance(cfg, rec, snap("Healthy", op("Running", at(90), time.Time{})), at(90), false)
	Advance(cfg, rec, snap("Healthy", next), at(92), false)
	if rec.LastDeploy.FixedBy == "" || !rec.LastDeploy.Dirty {
		t.Fatalf("failed deploy should link its fix, got %+v", rec.LastDeploy)
	}
}

func TestQuietResyncAtSameCommit(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	rec.LastDeploy = &Deploy{State: DeployHealthy, Commit: "5e6f7a80000", Msg: Message{Channel: "C", TS: "1.1"}}
	Advance(cfg, rec, snap("Healthy", op("Running", at(0), time.Time{})), at(0), false)
	if !rec.Deploy.Quiet {
		t.Fatal("same-commit resync should be quiet")
	}
	done := snap("Healthy", op("Succeeded", at(0), at(0).Add(time.Second)))
	Advance(cfg, rec, done, at(1), false)
	last := rec.LastDeploy
	if last.Resyncs != 0 {
		t.Fatal("a resync waits out the annotation lag before it counts")
	}
	Advance(cfg, rec, done, at(5), false)
	Advance(cfg, rec, done, at(6), false)
	if !last.Dirty || last.Resyncs != 1 || !last.ResyncAt.Equal(at(0)) {
		t.Fatalf("resync should count once on the previous deploy, got %+v", last)
	}
}

func TestBaselineOpensExistingFailuresAtOnce(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api"}
	s := snap("Degraded", op("Succeeded", t0.Add(-20*time.Hour), t0.Add(-20*time.Hour)))
	s.HealthSince = t0.Add(-21 * time.Hour)
	st := Advance(cfg, rec, s, at(0), true)
	if st.NewDeploy || rec.Deploy != nil {
		t.Fatal("old operations are not deploys at baseline")
	}
	if rec.Incident == nil || rec.Incident.State != IncidentOpen || !rec.Incident.Baseline {
		t.Fatalf("want baseline incident, got %+v", rec.Incident)
	}
	if !rec.Incident.FirstUnhealthyAt.Equal(s.HealthSince) {
		t.Fatalf("incident should date from Argo CD's transition time")
	}
}

func TestShort(t *testing.T) {
	for d, want := range map[time.Duration]string{
		42 * time.Second:                "42s",
		66 * time.Second:                "1m 06s",
		12 * time.Minute:                "12m",
		72 * time.Minute:                "1h 12m",
		69 * time.Hour:                  "2d 21h",
		41*time.Minute + 27*time.Second: "41m",
	} {
		if got := short(d); got != want {
			t.Errorf("short(%s) = %q, want %q", d, got, want)
		}
	}
}
