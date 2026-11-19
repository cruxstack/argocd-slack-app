package argocdslack

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// unescaped is a payload's JSON with <, > and & readable again.
func unescaped(v any) string {
	return strings.NewReplacer("\\u003c", "<", "\\u003e", ">", "\\u0026", "&").Replace(mustJSON(v))
}

func layoutView(t *testing.T, rich bool) View {
	cfg := testConfig(t)
	cfg.SCM.VersionURL = "https://github.com/{repo}/releases/tag/{version}"
	cl, _ := cfg.ClusterByName("dev-use1")
	return View{Cfg: cfg, Cluster: cl, App: "web", Namespace: "web", Rich: rich, Now: at(30)}
}

func releaseDeploy() *Deploy {
	return &Deploy{ID: "x", State: DeployHealthy, StartedAt: at(0), FinishedAt: at(0).Add(75 * time.Second),
		Repo: "acme/web", Commit: "def5678000", Version: "v1.5.0",
		From:  Release{Repo: "acme/web", Commit: "abc1234000", Version: "v1.4.0"},
		Actor: "mara-lee", ApprovedBy: "mara-lee", RunURL: "https://ci.example.com/runs/9",
		Changes: []Change{{ID: "#12", Title: "feat: one-tap checkout", URL: "https://github.com/acme/web/pull/12", Author: "mara-lee"}}}
}

func TestDeployLayout(t *testing.T) {
	for _, rich := range []bool{true, false} {
		v := layoutView(t, rich)
		p := RenderDeploy(v, releaseDeploy())
		checkPayload(t, fmt.Sprintf("release rich=%v", rich), p)
		body := unescaped(p)
		if p.Text != "web v1.5.0 deployed to dev-use1" {
			t.Errorf("heading: %q", p.Text)
		}
		for _, want := range []string{
			"<https://github.com/acme/web/compare/v1.4.0...v1.5.0|v1.4.0 → v1.5.0> · started and approved by mara-lee · 1m 15s",
			"https://github.com/acme/web/releases/tag/v1.5.0",
			"<https://ci.example.com/runs/9|Deployment> · <https://argocd.dev.example.com/applications/argocd/web|Argo CD> · ",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("rich=%v: want %q in %s", rich, want, body)
			}
		}
		if strings.Contains(body, "one-tap checkout") {
			t.Errorf("a deploy with a change list keeps titles for the thread: %s", body)
		}
	}

	v := layoutView(t, true)
	cases := []struct {
		name string
		edit func(d *Deploy)
		want string
	}{
		{"commits without versions", func(d *Deploy) { d.Version, d.From.Version = "", "" },
			"<https://github.com/acme/web/compare/abc1234000...def5678000|abc1234 → def5678> · started and approved by mara-lee"},
		{"nothing before", func(d *Deploy) { d.From = Release{} },
			"<https://github.com/acme/web/commit/def5678000|def5678> · started and approved by mara-lee"},
		{"another repo before", func(d *Deploy) { d.From.Repo = "acme/old-web" }, "|def5678> ·"},
		{"someone else approved", func(d *Deploy) { d.ApprovedBy = "jun-park" }, "by mara-lee, approved by jun-park · 1m 15s"},
		{"one title, no list", func(d *Deploy) { d.Changes, d.PR, d.Title = nil, "12", "feat: one-tap checkout" },
			"1m 15s\\n<https://github.com/acme/web/pull/12|#12> feat: one-tap checkout"},
		{"manual sync", func(d *Deploy) { d.Actor, d.ApprovedBy, d.Username = "", "", "ops@example.com" }, "synced by ops@example.com · 1m 15s"},
		{"rolling out", func(d *Deploy) { d.State, d.SyncedAt, d.Progress = DeployRolling, at(0).Add(5*time.Second), "2/4" },
			"started and approved by mara-lee · synced in 5s, rolling out 2/4 pods"},
	}
	for _, c := range cases {
		d := releaseDeploy()
		c.edit(d)
		p := RenderDeploy(v, d)
		checkPayload(t, c.name, p)
		if body := unescaped(p); !strings.Contains(body, c.want) {
			t.Errorf("%s: want %q in %s", c.name, c.want, body)
		}
	}

	d := releaseDeploy()
	d.State, d.Reason = DeployUnhealthy, &Reason{Category: CatCrashLoop, Resource: "Deployment web/web", Ready: "1/3",
		Summary: "CrashLoopBackOff · 6 restarts · last exit 1", Excerpt: "Error: connect ECONNREFUSED", Image: "web:1.5.0",
		Details: []string{"Readiness probe failed ×18"}, Pods: 3}
	body := unescaped(RenderDeploy(v, d))
	for _, want := range []string{"web v1.5.0 is degraded after deploying to dev-use1", "*CrashLoopBackOff*, 1 of 3 pods ready · 6 restarts · last exit 1",
		"Deployment web/web · web:1.5.0 · Readiness probe failed ×18"} {
		if !strings.Contains(body, want) {
			t.Errorf("trouble: want %q in %s", want, body)
		}
	}
	if strings.Contains(body, "1/3 ready") || strings.Contains(body, "rollout failed") {
		t.Errorf("trouble should fold pod counts into the reason line: %s", body)
	}
}

func TestChangesReply(t *testing.T) {
	d := releaseDeploy()
	d.Changes = nil
	for i := 1; i <= 12; i++ {
		d.Changes = append(d.Changes, Change{ID: fmt.Sprintf("#%d", i), Title: fmt.Sprintf("fix `thing` %d", i), URL: fmt.Sprintf("https://github.com/acme/web/pull/%d", i), Author: "jun-park"})
	}
	for _, rich := range []bool{true, false} {
		p := RenderChanges(layoutView(t, rich), d)
		checkPayload(t, fmt.Sprintf("changes rich=%v", rich), p)
		body := unescaped(p)
		if p.Text != "12 PRs in this deploy" || strings.Contains(body, "#11") {
			t.Errorf("rich=%v: want 10 of 12 PRs listed: %s", rich, body)
		}
		if !strings.Contains(body, "https://github.com/acme/web/compare/v1.4.0...v1.5.0") || !strings.Contains(body, "and 2 more") {
			t.Errorf("rich=%v: the rest should link to the comparison: %s", rich, body)
		}
	}
	rich := unescaped(RenderChanges(layoutView(t, true), d))
	for _, want := range []string{`"rich_text_list"`, `"style":{"code":true},"text":"thing"`, `"style":{"italic":true},"text":"  jun-park"`} {
		if !strings.Contains(rich, want) {
			t.Errorf("want %s in %s", want, rich)
		}
	}
	d.Changes = []Change{{Title: "Bump the base image"}}
	if p := RenderChanges(layoutView(t, true), d); p.Text != "1 change in this deploy" {
		t.Errorf("changes without PR ids: %q", p.Text)
	}
}

func TestParseChanges(t *testing.T) {
	raw := `[{"id":"#1","title":"feat: a","url":"https://x/1","author":"a"},{"title":"  "}]`
	for _, v := range []string{raw, base64.StdEncoding.EncodeToString([]byte(raw))} {
		cs := parseChanges(v)
		if len(cs) != 1 || cs[0].ID != "#1" || cs[0].Author != "a" {
			t.Errorf("parseChanges(%.20q) = %+v", v, cs)
		}
	}
	for _, v := range []string{"", "not json", "e30=", `{"id":1}`} {
		if cs := parseChanges(v); cs != nil {
			t.Errorf("parseChanges(%q) = %+v, want none", v, cs)
		}
	}
}

func TestDeployRemembersWhatWasRunning(t *testing.T) {
	cfg := testConfig(t)
	rec := &AppRecord{Cluster: "dev-use1", App: "api", OpStartedAt: t0.Add(-time.Hour)}
	settled := snap("Healthy", op("Succeeded", t0.Add(-time.Hour), t0.Add(-time.Hour)))
	settled.Version = "v1.4.0"
	Advance(cfg, rec, settled, at(0), false)
	if rec.Running.Version != "v1.4.0" || rec.Running.Commit != settled.Commit {
		t.Fatalf("a settled app should record what runs, got %+v", rec.Running)
	}

	s := snap("Healthy", op("Running", at(1), time.Time{}))
	s.Commit, s.Version = "9a8b7c60000", "v1.5.0"
	Advance(cfg, rec, s, at(1), false)
	if rec.Deploy.From.Version != "v1.4.0" || rec.Deploy.Version != "v1.5.0" {
		t.Fatalf("deploy should know what it replaces, got %+v", rec.Deploy)
	}
	s.Op = op("Succeeded", at(1), at(2))
	Advance(cfg, rec, s, at(2), false)
	if rec.Running.Version != "v1.5.0" {
		t.Fatalf("a healthy deploy is what runs now, got %+v", rec.Running)
	}

	next := snap("Healthy", op("Running", at(30), time.Time{}))
	next.Commit, next.Version = "1234567000", "v1.6.0"
	Advance(cfg, rec, next, at(30), false)
	if rec.Deploy.From.Version != "v1.5.0" {
		t.Fatalf("next deploy should compare to v1.5.0, got %+v", rec.Deploy.From)
	}
}

func TestDeployReplies(t *testing.T) {
	slack := &fakeSlack{}
	cfg := testConfig(t)
	cl, _ := cfg.ClusterByName("dev-use1")
	w := &worker{a: &App{Cfg: cfg, Slack: slack, Log: quietLog(), Now: func() time.Time { return at(30) }}, cl: cl}
	v := layoutView(t, true)
	d := releaseDeploy()
	d.Msg = Message{Channel: "CDEV", TS: "1.1"}
	d.Resyncs, d.ResyncAt = 1, at(10)
	w.deployReplies(context.Background(), v, d)
	if len(slack.posts) != 2 || slack.posts[0].ThreadTS != "1.1" || slack.posts[1].ThreadTS != "1.1" {
		t.Fatalf("want the change list and a resync note in the thread, got %s", mustJSON(slack.posts))
	}
	if !strings.Contains(slack.posts[1].Text, "Resynced at") {
		t.Errorf("one resync: %q", slack.posts[1].Text)
	}

	d.Resyncs, d.ResyncAt = 3, at(20)
	w.deployReplies(context.Background(), v, d)
	if len(slack.posts) != 2 || len(slack.updates) != 1 {
		t.Fatalf("later resyncs should edit the one reply, got %d posts and %d updates", len(slack.posts), len(slack.updates))
	}
	if u := slack.updates[0]; u.TS != d.ResyncMsg.TS || !strings.Contains(u.Text, "Resynced 3 times with no new commit, last at") {
		t.Errorf("edited reply: %+v", u)
	}
	w.deployReplies(context.Background(), v, d)
	if len(slack.posts) != 2 || len(slack.updates) != 1 {
		t.Error("nothing new should mean no Slack calls")
	}
}
