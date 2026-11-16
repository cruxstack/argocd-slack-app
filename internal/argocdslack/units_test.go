package argocdslack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const apiLog = `{"time":"2026-11-14T13:45:30.001Z","level":"info","msg":"running migrations"}
{"time":"2026-11-14T13:45:36.105Z","level":"error","msg":"migration failed; refusing to start","err":{"name":"DatabaseError","code":"ERR_CONNECTION_TIMEOUT","message":"connection timeout after 30000ms"}}
{"time":"2026-11-14T13:45:36.200Z","level":"info","msg":"shutting down"}`

const apiExcerpt = "migration failed; refusing to start · DatabaseError ERR_CONNECTION_TIMEOUT"

func apiCluster(t *testing.T) *fakeCluster {
	return &fakeCluster{
		apps: map[string]json.RawMessage{},
		objects: map[string]string{
			"/apis/apps/v1/namespaces/apps/deployments/api-server":                                       string(fixture(t, "deploy-api.json")),
			"/api/v1/namespaces/apps/pods?app.kubernetes.io/component=server,app.kubernetes.io/name=api": string(fixture(t, "pods-api.json")),
			"/api/v1/namespaces/apps/events":                                                             string(fixture(t, "events-api.json")),
		},
		logs: apiLog,
	}
}

func TestParseApplications(t *testing.T) {
	cfg := testConfig(t)
	s, err := ParseApplication(fixture(t, "app-api-degraded.json"), cfg.Annotations)
	if err != nil {
		t.Fatal(err)
	}
	if s.App != "api" || s.Health != "Degraded" || s.Namespace != "apps" || s.Repo != "acme/api" || s.Commit[:7] != "4f1c2d9" {
		t.Fatalf("api parsed wrong: %+v", s)
	}
	if s.Culprit == nil || s.Culprit.Kind != "Deployment" || s.Culprit.Name != "api-server" || s.Culprit.Namespace != "apps" {
		t.Fatalf("culprit: %+v", s.Culprit)
	}
	if s.Op == nil || s.Op.Phase != "Succeeded" || !s.Op.Automated || len(s.Images) != 1 {
		t.Fatalf("op or images: %+v", s)
	}
	if s.Actor != "" || s.PR != "" {
		t.Fatal("missing annotations must read as empty")
	}
	w, _ := ParseApplication(fixture(t, "app-worker-syncing.json"), cfg.Annotations)
	if w.Health != "Missing" || w.Op.Phase != "Running" || len(w.Workloads()) != 1 {
		t.Fatalf("worker parsed wrong: %+v", w)
	}
	// Without annotation names, repo and commit come from the source and revision.
	n, _ := ParseApplication(fixture(t, "app-web-healthy.json"), Annotations{})
	if n.Repo != "acme/gitops" || n.Commit != "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b" || n.CommitTitle != "" {
		t.Fatalf("fallbacks: %+v", n)
	}
}

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/gitops.git": "acme/gitops",
		"git@github.com:acme/gitops.git":     "acme/gitops",
		"ssh://git@gitlab.example.com/a/b":   "a/b",
		"acme/api":                           "acme/api",
	} {
		if got := repoName(in); got != want {
			t.Errorf("repoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaceholderAnnotationsAreEmpty(t *testing.T) {
	ann := Annotations{Actor: "app_actor", PR: "app_pr", Title: "app_commit_title"}
	s, _ := ParseApplication([]byte(`{"metadata":{"name":"x","annotations":{"app_actor":"{{app_actor}}","app_pr":"<no value>","app_commit_title":"fix: thing"}}}`), ann)
	if s.Actor != "" || s.PR != "" || s.CommitTitle != "fix: thing" {
		t.Fatalf("got %+v", s)
	}
}

func TestReasonForCrashLoop(t *testing.T) {
	s, _ := ParseApplication(fixture(t, "app-api-degraded.json"), testConfig(t).Annotations)
	r := Enricher{R: apiCluster(t), LogLines: true}.ForHealth(context.Background(), s)
	if r.Category != CatCrashLoop {
		t.Fatalf("category %q (%+v)", r.Category, r)
	}
	if r.Resource != "Deployment apps/api-server" || r.Ready != "0/2" {
		t.Fatalf("resource/ready: %q %q", r.Resource, r.Ready)
	}
	if !strings.HasPrefix(r.Summary, "CrashLoopBackOff · 7 restarts · last exit 1") {
		t.Fatalf("summary %q", r.Summary)
	}
	if r.Excerpt != apiExcerpt {
		t.Fatalf("excerpt %q", r.Excerpt)
	}
	if len(r.Details) == 0 || r.Image != "api:v1.4.2" {
		t.Fatalf("details %v image %q", r.Details, r.Image)
	}
	noLogs := Enricher{R: apiCluster(t)}.ForHealth(context.Background(), s)
	if noLogs.Excerpt != "" {
		t.Fatal("log lines must stay off unless enabled")
	}
}

func TestReasonForFailedHook(t *testing.T) {
	s, _ := ParseApplication(fixture(t, "app-worker-syncing.json"), testConfig(t).Annotations)
	s.Op = &Operation{Phase: "Failed", Message: "one or more synchronization tasks completed unsuccessfully",
		Results: []ResourceResult{{Kind: "Job", Namespace: "jobs", Name: "worker-migrate", HookType: "PreSync", HookPhase: "Failed", Message: "Job has reached the specified backoff limit"}}}
	r := Enricher{R: &fakeCluster{objects: map[string]string{}}}.ForDeploy(context.Background(), s)
	if r.Category != CatMissing && r.Category != CatHook {
		t.Fatalf("category %q", r.Category)
	}
}

func TestExtractAndRedact(t *testing.T) {
	cases := map[string]string{
		apiLog: apiExcerpt,
		"starting\nError: connect ECONNREFUSED 10.0.0.1:6379\nbye": "Error: connect ECONNREFUSED 10.0.0.1:6379",
		`{"level":50,"msg":"boom","err":"bad"}`:                    "boom · bad",
	}
	for in, want := range cases {
		if got := ExtractErrorLine(in); got != want {
			t.Errorf("ExtractErrorLine = %q, want %q", got, want)
		}
	}
	secret := `failed to call https://user:hunter2@api.example.com token=abc123def456 Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U AKIAABCDEFGHIJKLMNOP ops@example.com xoxb-1234567890-abcdefghij`
	got := Redact(secret)
	for _, leak := range []string{"hunter2", "abc123def456", "eyJhbGci", "AKIAABCDEFGHIJKLMNOP", "ops@example.com", "xoxb-1234567890"} {
		if strings.Contains(got, leak) {
			t.Errorf("redaction leaked %q: %s", leak, got)
		}
	}
	keys := Redact("aws_secret wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY hex 0123456789abcdef0123456789abcdef01234567")
	if strings.Contains(keys, "EXAMPLEKEY") || strings.Contains(keys, "0123456789abcdef") {
		t.Errorf("long keys should be redacted: %s", keys)
	}
	names := "describe cluster acme-plat-use1-dev-common-shop-eks-cluster: assumed-role/acme-core-use2-tools-argocd-slack/acme-core-use2-tools-argocd-slack"
	if got := Redact(names); got != names {
		t.Errorf("lowercase names and paths should stay readable: %s", got)
	}
}

func TestMuteParsingAndMatching(t *testing.T) {
	cfg := testConfig(t)
	dev, _ := cfg.ClusterByName("dev-use1")
	prod, _ := cfg.ClusterByName("prod-use1")
	m, err := ParseMuteArgs(cfg, strings.Fields("api@dev-use1 until-healthy waiting on a database fix"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if m.App != "api" || m.Cluster != "dev-use1" || !m.UntilHealthy || m.Reason != "waiting on a database fix" {
		t.Fatalf("got %+v", m)
	}
	if !m.Matches(dev, "api", KindIncidents, t0.Add(time.Hour)) || m.Matches(prod, "api", KindIncidents, t0) || m.Matches(dev, "web", KindIncidents, t0) {
		t.Fatal("app@cluster matching is wrong")
	}
	env, _ := ParseMuteArgs(cfg, []string{"dev", "1d"}, t0)
	if env.Env != "dev" || !env.Matches(dev, "anything", KindDeploys, t0) || env.Matches(prod, "x", KindDeploys, t0) {
		t.Fatalf("env mute: %+v", env)
	}
	if env.Active(t0.Add(25 * time.Hour)) {
		t.Fatal("1d mute should have ended")
	}
	if _, err := ParseMuteArgs(cfg, []string{"dev-use1", "2w"}, t0); err == nil {
		t.Fatal("mutes over a week must be refused")
	}
	if _, err := ParseMuteArgs(cfg, []string{"dev-use1", "until-healthy"}, t0); err == nil {
		t.Fatal("until-healthy needs one app")
	}
	cl, _ := ParseMuteArgs(cfg, []string{"dev-use1"}, t0)
	if cl.Cluster != "dev-use1" || cl.Until != t0.Add(4*time.Hour) {
		t.Fatalf("default cluster mute: %+v", cl)
	}
}

func checkPayload(t *testing.T, name string, p Payload) {
	t.Helper()
	if strings.TrimSpace(p.Text) == "" {
		t.Errorf("%s: empty notification text", name)
	}
	if strings.ContainsAny(p.Text, "*`") || strings.Contains(p.Text, "<http") {
		t.Errorf("%s: text keeps markup: %q", name, p.Text)
	}
	blocks := p.Blocks
	for _, a := range p.Attachments {
		blocks = append(blocks, a.Blocks...)
	}
	if len(blocks) == 0 || len(blocks) > 50 {
		t.Errorf("%s: %d blocks", name, len(blocks))
	}
	for _, b := range blocks {
		if b == nil {
			t.Errorf("%s: nil block", name)
		}
		if b["type"] == "section" {
			if txt, ok := b["text"].(Block); ok && len(txt["text"].(string)) > 3000 {
				t.Errorf("%s: section over 3000 chars", name)
			}
		}
	}
	if _, err := json.Marshal(p); err != nil {
		t.Errorf("%s: %v", name, err)
	}
}

func TestRenderEveryState(t *testing.T) {
	cfg := testConfig(t)
	cl, _ := cfg.ClusterByName("dev-use1")
	reason := &Reason{Category: CatCrashLoop, Resource: "Deployment apps/api-server", Ready: "0/2", Summary: "CrashLoopBackOff · 259 restarts · last exit 1",
		Details: []string{"Startup probe failed ×565", "Back-off restarting container ×1386"}, Excerpt: apiExcerpt, Image: "api:v1.4.2", Pods: 2}
	mute := &Mute{ID: "a1", App: "api", Cluster: "dev-use1", Kinds: []string{KindIncidents}, Until: t0.Add(time.Hour), By: "U1", Reason: "waiting"}
	for _, rich := range []bool{true, false} {
		v := View{Cfg: cfg, Cluster: cl, App: "api", Namespace: "apps", Rich: rich, Now: at(30)}
		for _, st := range []DeployState{DeploySyncing, DeployRolling, DeployStalled, DeployHealthy, DeployFailed, DeployUnhealthy, DeployRecovered, DeploySuperseded} {
			d := &Deploy{ID: "x", State: st, StartedAt: at(0), SyncedAt: at(0).Add(5 * time.Second), FinishedAt: at(2), Attempts: 5, Repo: "acme/web", Commit: "c3d4e5f0000",
				PR: "128", Title: "feat: render the pricing page on the server", Progress: "2/4", Reason: reason,
				Failures: []string{"*PreSync job* `worker-migrate`: job has reached the specified backoff limit"}}
			p := RenderDeploy(v, d)
			checkPayload(t, fmt.Sprintf("deploy %s rich=%v", st, rich), p)
			if rich && (len(p.Blocks) != 1 || p.Blocks[0]["type"] != "container" || !strings.Contains(mustJSON(p), "/assets/status/")) {
				t.Errorf("rich deploy %s should be one container with a status icon", st)
			}
			if !rich && strings.Contains(mustJSON(p), `"container"`) {
				t.Errorf("basic deploy %s must not use a container", st)
			}
		}
		for _, st := range []IncidentState{IncidentOpen, IncidentResolved, IncidentClosed} {
			inc := &Incident{ID: "i", State: st, Health: "Degraded", FirstUnhealthyAt: at(0), ResolvedAt: at(30), Reason: reason, Flaps: 1}
			vv := v
			vv.Mute = mute
			checkPayload(t, fmt.Sprintf("incident %s rich=%v", st, rich), RenderIncident(vv, inc))
		}
		checkPayload(t, "digest", RenderDigest("Daily summary · dev", []DigestRow{{App: "preview-06", Clusters: "dev-use1", For: "3d", Reason: "image not found", Note: "cleanup?"}}, Stats{Deploys: 31}, rich))
	}
	for _, rich := range []bool{true, false} {
		v := View{Cfg: cfg, Cluster: cl, Rich: rich, Now: at(90)}
		down := RenderUnreachable(v, at(0), "list applications: dial tcp 10.0.0.1:443: i/o timeout <``` & > token=hunter2")
		checkPayload(t, "unreachable", down)
		body := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(mustJSON(down))
		if !strings.Contains(body, "```list applications: dial tcp 10.0.0.1:443: i/o timeout &lt;''' &amp; &gt; token=[redacted]```") {
			t.Errorf("unreachable notice should show the escaped, redacted error in a code block: %s", body)
		}
		up := RenderReachable(v, at(0))
		checkPayload(t, "reachable", up)
		if !strings.Contains(mustJSON(up), "paused for 1h 30m") {
			t.Error("reachable notice should say how long notifications were paused")
		}
		checkPayload(t, "baseline", RenderBaseline(cfg, cl, 18, []BaselineRow{{App: "api", Kind: "incident", Summary: "CrashLoopBackOff · database timeout", Since: at(-1300)}, {App: "worker", Kind: "deploy", Summary: "sync failed", Resolved: true}}, rich))
	}
	digest := mustJSON(RenderDigest("Daily summary · dev", []DigestRow{{App: "preview-06", Clusters: "dev-use1", For: "3d", Age: 72 * time.Hour, Reason: "image not found", URL: "https://argo/x"}}, Stats{}, true))
	if !strings.Contains(digest, `"data_table"`) || !strings.Contains(digest, `"mute_open"`) || !strings.Contains(digest, `"value":72`) {
		t.Errorf("rich digest should be a sortable data table with mute buttons: %s", digest)
	}
	inc := &Incident{ID: "i", State: IncidentOpen, Health: "Degraded", FirstUnhealthyAt: at(0), Mention: "<!subteam^S1>"}
	p := RenderIncident(View{Cfg: cfg, Cluster: cl, App: "api", Rich: true, Now: at(1)}, inc)
	if !strings.HasPrefix(p.Text, "<!subteam^S1> ") || !strings.Contains(mustJSON(p), `"usergroup_id":"S1"`) {
		t.Errorf("mention should notify and show: %s", mustJSON(p))
	}
	if cfg.IconURL(iconOK) != DefaultIconBaseURL("main")+"/ok.png" {
		t.Errorf("icon url: %s", cfg.IconURL(iconOK))
	}
	home := RenderHome([]HomeCluster{{Name: "dev-use1", Enabled: true, Healthy: 11, Unhealthy: 7, Muted: 1}}, []Mute{*mute}, at(0))
	if home["type"] != "home" {
		t.Fatal("home view")
	}
	modal := RenderMuteModal(cfg, MuteContext{Cluster: "dev-use1", App: "api", Kind: KindIncidents})
	if len(modal["blocks"].([]Block)) != 5 {
		t.Fatal("modal blocks")
	}
}

func TestMuteFromForm(t *testing.T) {
	values := map[string]map[string]json.RawMessage{
		"scope":    {"v": json.RawMessage(`{"selected_option":{"value":"app@api@dev-use1"}}`)},
		"duration": {"v": json.RawMessage(`{"selected_option":{"value":"healthy"}}`)},
		"kinds":    {"v": json.RawMessage(`{"selected_options":[{"value":"incidents"}]}`)},
		"reason":   {"v": json.RawMessage(`{"value":"waiting on a database fix"}`)},
	}
	m, field, err := muteFromForm(values, t0)
	if err != nil || field != "" {
		t.Fatal(field, err)
	}
	if m.App != "api" || m.Cluster != "dev-use1" || !m.UntilHealthy || len(m.Kinds) != 1 {
		t.Fatalf("got %+v", m)
	}
	values["duration"]["v"] = json.RawMessage(`{"selected_option":{"value":"custom"}}`)
	if _, field, err := muteFromForm(values, t0); err == nil || field != "until" {
		t.Fatal("custom duration needs a time")
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte("token=x&command=%2Fargocd&text=mutes")
	ts := fmt.Sprint(t0.Unix())
	mac := hmac.New(sha256.New, []byte("s3cret"))
	fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if err := VerifySignature("s3cret", ts, sig, body, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if VerifySignature("other", ts, sig, body, t0) == nil {
		t.Fatal("wrong secret accepted")
	}
	if VerifySignature("s3cret", ts, sig, body, t0.Add(10*time.Minute)) == nil {
		t.Fatal("stale request accepted")
	}
}

func TestLinks(t *testing.T) {
	cfg := testConfig(t)
	cl, _ := cfg.ClusterByName("dev-use1")
	if got := cl.AppURL("api"); got != "https://argocd.dev.example.com/applications/argocd/api" {
		t.Fatal(got)
	}
	links := cfg.ResolvedLinks(cl, "api", "apps")
	if len(links) != 2 || links[0].URL != "https://grafana.example.com/explore?cluster=dev-main&ns=apps" || links[1].Text != "Metrics" {
		t.Fatalf("links: %+v", links)
	}
	if got := cfg.CommitURL("acme/api", "abc"); got != "https://github.com/acme/api/commit/abc" {
		t.Fatal(got)
	}
	cfg.SCM.PRURL = "https://gitlab.example.com/{repo}/-/merge_requests/{pr}"
	if got := cfg.PRURL("acme/api", "12"); got != "https://gitlab.example.com/acme/api/-/merge_requests/12" {
		t.Fatal(got)
	}
}

func TestConfigReportsProblems(t *testing.T) {
	_, err := LoadConfig(`{"clusters": [{"name": "a", "enabled": true}, {"name": "a", "cluster": "x", "channel": "C1"}]}`)
	if err == nil || !strings.Contains(err.Error(), "listed twice") || !strings.Contains(err.Error(), "needs the EKS cluster name") || !strings.Contains(err.Error(), "no channel") {
		t.Fatalf("want every problem reported, got %v", err)
	}
}
