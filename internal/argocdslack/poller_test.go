package argocdslack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type harness struct {
	t     *testing.T
	app   *App
	store *memStore
	slack *fakeSlack
	cl    *fakeCluster
	clock time.Time
	n     int
}

func newHarness(t *testing.T) *harness {
	cfg := testConfig(t)
	cfg.WatchSeconds = 1
	h := &harness{t: t, store: newMemStore(), slack: &fakeSlack{}, cl: apiCluster(t), clock: time.Now().UTC().Truncate(time.Second)}
	h.cl.apps = map[string]json.RawMessage{}
	h.app = &App{
		Cfg: cfg, Store: h.store, Slack: h.slack, Log: quietLog(),
		Connect: func(context.Context, Cluster) (ClusterAPI, error) { return h.cl, nil },
		Now:     func() time.Time { return h.clock },
	}
	return h
}

func (h *harness) poll(advance time.Duration) {
	h.t.Helper()
	h.clock = h.clock.Add(advance)
	h.n++
	if err := h.app.Poll(context.Background(), "poll"); err != nil {
		h.t.Fatal(err)
	}
}

func setHealth(status string, since time.Time) func(map[string]any) {
	return func(m map[string]any) {
		hl := obj(m, "status", "health")
		hl["status"] = status
		hl["lastTransitionTime"] = since.Format(time.RFC3339)
		if status == "Healthy" {
			delete(hl, "message")
		}
	}
}

func TestPollEndToEnd(t *testing.T) {
	h := newHarness(t)
	start := h.clock
	api := fixture(t, "app-api-degraded.json")
	web := fixture(t, "app-web-healthy.json")
	h.cl.set(t, api, setHealth("Degraded", start.Add(-21*time.Hour)))
	h.cl.set(t, web, setHealth("Healthy", start.Add(-5*time.Hour)))

	// First poll: one baseline message, nothing else.
	h.poll(0)
	top := h.slack.topLevel()
	if len(top) != 1 || !strings.Contains(top[0].Text, "Now watching dev-use1") || top[0].Channel != "CTEST" {
		t.Fatalf("want one baseline post to the override channel, got %d: %+v", len(top), top)
	}
	if !strings.Contains(mustJSON(top[0]), "api") || !strings.Contains(mustJSON(top[0]), "CrashLoopBackOff") {
		t.Fatalf("baseline should list api with its reason: %s", mustJSON(top[0]))
	}

	// web degrades: nothing for the first minutes, then one incident.
	h.cl.set(t, web, setHealth("Degraded", h.clock.Add(time.Minute)))
	h.poll(time.Minute)
	h.poll(3 * time.Minute)
	if n := len(h.slack.topLevel()); n != 1 {
		t.Fatalf("incident posted inside the grace period (%d posts)", n)
	}
	h.poll(3 * time.Minute)
	top = h.slack.topLevel()
	if len(top) != 2 || !strings.Contains(top[1].Text, "web is degraded on dev-use1") {
		t.Fatalf("want one incident post, got %+v", top)
	}
	incidentTS := "100.2"
	threads := 0
	for _, p := range h.slack.posts {
		if p.ThreadTS == incidentTS {
			threads++
		}
	}
	if threads != 2 {
		t.Fatalf("want 2 silent thread replies on the incident, got %d", threads)
	}

	// Many more polls while still degraded: no new top-level messages.
	for i := 0; i < 5; i++ {
		h.poll(10 * time.Minute)
	}
	if n := len(h.slack.topLevel()); n != 2 {
		t.Fatalf("degraded app re-posted (%d top-level)", n)
	}

	// Recovery edits the same message.
	h.cl.set(t, web, setHealth("Healthy", h.clock.Add(time.Minute)))
	updatesBefore := len(h.slack.updates)
	h.poll(time.Minute)
	var resolved bool
	for _, u := range h.slack.updates[updatesBefore:] {
		if u.TS == incidentTS && strings.Contains(u.Text, "web recovered on dev-use1") {
			resolved = true
		}
	}
	if !resolved || len(h.slack.topLevel()) != 2 {
		t.Fatalf("recovery should edit the incident message in place; updates: %+v", h.slack.updates[updatesBefore:])
	}

	// api recovering ticks its baseline row.
	h.cl.set(t, api, setHealth("Healthy", h.clock))
	updatesBefore = len(h.slack.updates)
	h.poll(time.Minute)
	var rowDone bool
	for _, u := range h.slack.updates[updatesBefore:] {
		if u.TS == "100.1" && strings.Contains(mustJSON(u), "healthy now") {
			rowDone = true
		}
	}
	if !rowDone {
		t.Fatal("baseline row should turn healthy")
	}
}

func TestPollMutedIncidentPostsWhenMuteEnds(t *testing.T) {
	h := newHarness(t)
	web := fixture(t, "app-web-healthy.json")
	h.cl.set(t, web, setHealth("Healthy", h.clock.Add(-time.Hour)))
	h.poll(0) // baseline
	_ = h.store.PutMute(context.Background(), Mute{ID: "m1", App: "web", Cluster: "dev-use1", Kinds: []string{KindIncidents}, Until: h.clock.Add(30 * time.Minute), By: "U1"})
	h.cl.set(t, web, setHealth("Degraded", h.clock))
	h.poll(time.Minute)
	h.poll(10 * time.Minute)
	if n := len(h.slack.topLevel()); n != 1 {
		t.Fatalf("muted incident was posted (%d)", n)
	}
	h.poll(30 * time.Minute)
	top := h.slack.topLevel()
	if len(top) != 2 || !strings.Contains(top[1].Text, "web is degraded") {
		t.Fatalf("incident should post once the mute ends, got %+v", top)
	}
}

func TestPollDeployOneMessage(t *testing.T) {
	h := newHarness(t)
	web := fixture(t, "app-web-healthy.json")
	h.cl.set(t, web, setHealth("Healthy", h.clock.Add(-time.Hour)))
	h.poll(0)
	started := h.clock.Add(30 * time.Second)
	setOp := func(phase string, finished *time.Time, health string) func(map[string]any) {
		return func(m map[string]any) {
			setHealth(health, h.clock)(m)
			op := obj(m, "status", "operationState")
			op["phase"], op["startedAt"] = phase, started.Format(time.RFC3339)
			delete(op, "finishedAt")
			if finished != nil {
				op["finishedAt"] = finished.Format(time.RFC3339)
			}
		}
	}
	h.cl.set(t, web, setOp("Running", nil, "Healthy"))
	h.poll(time.Minute)
	synced := started.Add(5 * time.Second)
	h.cl.set(t, web, setOp("Succeeded", &synced, "Progressing"))
	h.poll(time.Minute)
	h.cl.set(t, web, setOp("Succeeded", &synced, "Healthy"))
	h.poll(time.Minute)
	top := h.slack.topLevel()
	if len(top) != 2 {
		t.Fatalf("want baseline + one deploy message, got %d", len(top))
	}
	last := h.slack.updates[len(h.slack.updates)-1]
	if last.TS != "100.2" || !strings.Contains(last.Text, "web deployed to dev-use1") {
		t.Fatalf("deploy message should end healthy in place, got %q on %s", last.Text, last.TS)
	}
}

func TestRichBlocksFallBack(t *testing.T) {
	h := newHarness(t)
	h.slack.rejectRich = true
	web := fixture(t, "app-web-healthy.json")
	h.cl.set(t, web, setHealth("Healthy", h.clock.Add(-time.Hour)))
	h.poll(0)
	started := h.clock
	h.cl.set(t, web, func(m map[string]any) {
		op := obj(m, "status", "operationState")
		op["phase"], op["startedAt"] = "Running", started.Add(10*time.Second).Format(time.RFC3339)
		delete(op, "finishedAt")
	})
	h.poll(time.Minute)
	if len(h.slack.topLevel()) != 2 || !h.app.richOff.Load() {
		t.Fatal("deploy should post with basic blocks after Slack refuses rich ones")
	}
}
