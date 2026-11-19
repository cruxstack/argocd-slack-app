package argocdslack

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	oldCommit = "5e6f7a80000"
	newCommit = "9a8b7c60000"
)

// settledRecord is an app running v1.4.0 whose last deploy was posted.
func settledRecord() *AppRecord {
	running := Release{Repo: "acme/api", Commit: oldCommit, Version: "v1.4.0"}
	return &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour), Running: running,
		LastDeploy: &Deploy{State: DeployHealthy, Repo: "acme/api", Commit: oldCommit, Version: "v1.4.0", Msg: Message{Channel: "C", TS: "1.1"}}}
}

// stale is what the Application says before its ApplicationSet catches up.
func stale(o *Operation) Snapshot {
	s := snap("Healthy", o)
	s.Version = "v1.4.0"
	return s
}

// fresh is the same sync once the annotations name the new release.
func fresh(o *Operation) Snapshot {
	s := snap("Healthy", o)
	s.Commit, s.Version, s.RunURL, s.ApprovedBy = newCommit, "v1.5.0", "https://ci.example.com/runs/2", "mara-lee"
	s.Changes = []Change{{ID: "#12", Title: "feat: one-tap checkout", URL: "https://github.com/acme/api/pull/12"}}
	return s
}

func TestLateAnnotationsRelabelTheDeploy(t *testing.T) {
	cfg := testConfig(t)
	rec := settledRecord()
	rec.LastDeploy.Commit = "1111111000" // not a repeat, so the deploy is posted at once
	Advance(cfg, rec, stale(op("Running", at(0), time.Time{})), at(0), false)
	done := op("Succeeded", at(0), at(0).Add(5*time.Second))
	Advance(cfg, rec, stale(done), at(1), false)
	d := rec.Deploy
	if d.Quiet || d.State != DeployHealthy || d.Version != "v1.4.0" {
		t.Fatalf("setup: want a posted healthy deploy with stale annotations, got %+v", d)
	}
	d.Dirty, d.ChangesPosted = false, true

	Advance(cfg, rec, fresh(done), at(3), false)
	if d.Commit != newCommit || d.Version != "v1.5.0" || d.ApprovedBy != "mara-lee" || d.RunURL == "" || len(d.Changes) != 1 {
		t.Fatalf("deploy should take the late annotations, got %+v", d)
	}
	if !d.Dirty || d.ChangesPosted || d.From.Version != "v1.4.0" || rec.Running.Version != "v1.5.0" {
		t.Fatalf("want a re-render from v1.4.0, a new change list and v1.5.0 running, got %+v running %+v", d, rec.Running)
	}
	body := unescaped(RenderDeploy(layoutView(t, true), d))
	if !strings.Contains(body, "v1.5.0 deployed to dev-use1") || !strings.Contains(body, "v1.4.0 → v1.5.0") {
		t.Errorf("message should show the new release: %s", body)
	}
}

func TestLateAnnotationsTurnAResyncIntoADeploy(t *testing.T) {
	cfg := testConfig(t)
	rec := settledRecord()
	prev := rec.LastDeploy
	Advance(cfg, rec, stale(op("Running", at(0), time.Time{})), at(0), false)
	done := op("Succeeded", at(0), at(0).Add(5*time.Second))
	Advance(cfg, rec, stale(done), at(1), false)
	if !rec.Deploy.Quiet {
		t.Fatal("setup: stale annotations look like a resync")
	}
	st := Advance(cfg, rec, fresh(done), at(3), false)
	if rec.Deploy.Quiet || !st.NewDeploy || rec.Deploy.Version != "v1.5.0" {
		t.Fatalf("the resync was a deploy after all, got %+v", rec.Deploy)
	}
	for m := 4; m <= 30; m++ {
		if rec.Deploy != nil {
			rec.Deploy.Dirty = false // as if sent
		}
		Advance(cfg, rec, fresh(done), at(m), false)
	}
	if rec.LastDeploy.Version != "v1.5.0" || prev.Resyncs != 0 {
		t.Fatalf("no resync should be noted on v1.4.0, got %+v", prev)
	}
}

func TestLateAnnotationsLeaveRealDeploysAlone(t *testing.T) {
	cfg := testConfig(t)
	done := op("Succeeded", at(0), at(0).Add(5*time.Second))

	rec := settledRecord()
	Advance(cfg, rec, fresh(op("Running", at(0), time.Time{})), at(0), false)
	Advance(cfg, rec, fresh(done), at(1), false)
	next := fresh(done)
	next.Commit, next.Version = "2222222000", "v1.6.0"
	Advance(cfg, rec, next, at(2), false)
	if rec.Deploy.Version != "v1.5.0" {
		t.Fatalf("a deploy that named its release keeps it when the next one's annotations land early, got %+v", rec.Deploy)
	}

	rec = settledRecord()
	Advance(cfg, rec, stale(op("Running", at(0), time.Time{})), at(0), false)
	Advance(cfg, rec, stale(done), at(1), false)
	Advance(cfg, rec, fresh(done), at(6), false)
	if rec.Deploy.Version != "v1.4.0" || rec.LastDeploy.Resyncs != 1 {
		t.Fatalf("annotations after the lag belong to the next sync, got %+v last %+v", rec.Deploy, rec.LastDeploy)
	}
}

func TestChangesReplyEditedInPlace(t *testing.T) {
	slack := &fakeSlack{}
	cfg := testConfig(t)
	cl, _ := cfg.ClusterByName("dev-use1")
	w := &worker{a: &App{Cfg: cfg, Slack: slack, Log: quietLog(), Now: func() time.Time { return at(30) }}, cl: cl}
	v := layoutView(t, true)
	d := releaseDeploy()
	d.Msg = Message{Channel: "CDEV", TS: "1.1"}
	w.deployReplies(context.Background(), v, d)
	if len(slack.posts) != 1 || !d.ChangesMsg.Posted() {
		t.Fatalf("want one change list reply, got %d", len(slack.posts))
	}
	d.Changes = append(d.Changes, Change{ID: "#13", Title: "fix: totals", URL: "https://github.com/acme/web/pull/13"})
	d.ChangesPosted = false
	w.deployReplies(context.Background(), v, d)
	if len(slack.posts) != 1 || len(slack.updates) != 1 || slack.updates[0].TS != d.ChangesMsg.TS || slack.updates[0].ThreadTS != "" {
		t.Fatalf("a new list should edit the reply, got %d posts and %+v", len(slack.posts), slack.updates)
	}
	if !strings.Contains(slack.updates[0].Text, "2 PRs") || !d.ChangesPosted {
		t.Errorf("edited reply: %q", slack.updates[0].Text)
	}
}
