package argocdslack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func demoApp(t *testing.T) (*App, *fakeSlack, *[]Job) {
	t.Helper()
	slack := &fakeSlack{}
	jobs := &[]Job{}
	app := &App{Cfg: testConfig(t), Log: quietLog(), Slack: slack, Store: newMemStore(), Now: func() time.Time { return t0 },
		Sleep:  func(context.Context, time.Duration) {},
		Invoke: func(_ context.Context, j Job) error { *jobs = append(*jobs, j); return nil }}
	return app, slack, jobs
}

// finalMessages maps each top-level message to the last version Slack got.
func finalMessages(f *fakeSlack) (map[string]Payload, []string) {
	last, history := map[string]Payload{}, map[string][]string{}
	var order []string
	for i, p := range f.posts {
		if p.ThreadTS != "" {
			continue
		}
		ts := fmt.Sprintf("100.%d", i+1)
		last[ts] = p
		order = append(order, ts)
	}
	for _, p := range f.updates {
		last[p.TS] = p
		history[p.TS] = append(history[p.TS], mustJSON(p))
	}
	return last, order
}

func runDemo(t *testing.T, app *App, jobs *[]Job, text string) {
	t.Helper()
	resp := app.command(context.Background(), url.Values{"user_id": {"U9"}, "channel_id": {"CDEMO"}, "text": {text}}, t0)
	if resp.StatusCode != http.StatusOK || resp.Body != "" {
		t.Fatalf("demo command: %d %q", resp.StatusCode, resp.Body)
	}
	for len(*jobs) > 0 {
		j := (*jobs)[0]
		*jobs = (*jobs)[1:]
		if err := app.RunJob(context.Background(), j, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDemoPlaysEveryStory(t *testing.T) {
	app, slack, jobs := demoApp(t)
	runDemo(t, app, jobs, "demo")
	last, order := finalMessages(slack)

	var titles []string
	for _, ts := range order {
		p := last[ts]
		if p.Channel != "CDEMO" {
			t.Errorf("posted to %q", p.Channel)
		}
		if len(p.Attachments) > 0 && len(p.Blocks) != 1 {
			t.Errorf("the headline should be the only top-level block: %s", mustJSON(p))
		}
		if ts != order[0] {
			checkPayload(t, "demo "+p.Text, p)
		}
		titles = append(titles, p.Text)
	}
	want := []string{
		"Argo CD demo* by <@U9> is done",
		"Now watching demo · 1 of 10 apps is still unhealthy",
		"checkout-api deployed to demo in 2m 20s",
		"payments-worker failed to deploy to demo",
		"search-indexer recovered on demo",
		"web-frontend recovered on demo",
		"payments-worker deployed to demo",
		"Daily summary · dev",
	}
	if len(titles) != len(want) {
		t.Fatalf("got %d top-level messages:\n%s", len(titles), strings.Join(titles, "\n"))
	}
	for i, w := range want {
		if !strings.Contains(titles[i], w) {
			t.Errorf("message %d: want %q in %q", i, w, titles[i])
		}
	}

	all := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(mustJSON(slack.updates))
	for _, w := range []string{"flapped 1×", "Muted until healthy by <@U9>", "<@U9> is on it", "rolling out 2/3 pods", "Fixed by the", "legacy-reports recovered at", "OOMKilled"} {
		if !strings.Contains(all, w) {
			t.Errorf("no update shows %q", w)
		}
	}
	// Only the flapping incident has history worth a thread.
	if threads := len(slack.posts) - len(order); threads != 2 {
		t.Errorf("want 2 thread replies (flap and recovery), got %d", threads)
	}
}

func TestDemoPlaysOneStory(t *testing.T) {
	app, slack, jobs := demoApp(t)
	runDemo(t, app, jobs, "demo incident")
	last, order := finalMessages(slack)
	if len(order) != 2 || !strings.Contains(last[order[1]].Text, "web-frontend recovered") {
		t.Fatalf("want the intro and the incident, got %v", order)
	}
	if !strings.Contains(last[order[0]].Text, "is done") {
		t.Fatal("intro should say the demo is done")
	}
}

func TestDemoHandsOffBeforeTheLambdaTimesOut(t *testing.T) {
	app, slack, jobs := demoApp(t)
	resp := app.command(context.Background(), url.Values{"user_id": {"U9"}, "channel_id": {"CDEMO"}, "text": {"demo"}}, t0)
	if resp.StatusCode != http.StatusOK || len(*jobs) != 1 {
		t.Fatal("demo should start a job")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := app.RunJob(ctx, (*jobs)[0], "test"); err != nil {
		t.Fatal(err)
	}
	if len(*jobs) != 2 || (*jobs)[1].Demo.Next == 0 || (*jobs)[1].Demo.Hops != 1 {
		t.Fatalf("want a hand-off job, got %+v", *jobs)
	}
	if err := app.RunJob(context.Background(), (*jobs)[1], "test"); err != nil {
		t.Fatal(err)
	}
	last, order := finalMessages(slack)
	if len(order) != 8 || !strings.Contains(last[order[0]].Text, "is done") {
		t.Fatalf("hand-off lost track: %d messages", len(order))
	}
}

func TestDemoRefusals(t *testing.T) {
	if _, err := ParseDemoStories([]string{"nope"}); err == nil {
		t.Error("unknown story accepted")
	}
	if s, _ := ParseDemoStories([]string{"incident", "all"}); s != nil {
		t.Error("all should play everything")
	}

	app, slack, _ := demoApp(t)
	app.Cfg.DryRun = true
	if resp := app.command(context.Background(), url.Values{"text": {"demo"}, "channel_id": {"C1"}}, t0); !strings.Contains(resp.Body, "Dry run is on") {
		t.Errorf("dry run: %s", resp.Body)
	}

	app.Cfg.DryRun = false
	body := `{"type":"block_actions","user":{"id":"U9"},"channel":{"id":"CDEMO"},"actions":[{"action_id":"ack","value":"demo|web-frontend|demo/web-frontend/1"}]}`
	app.interaction(context.Background(), []byte(body), t0)
	if len(slack.ephem) != 1 || !strings.Contains(slack.ephem[0], "demo message") {
		t.Errorf("demo buttons should explain themselves: %v", slack.ephem)
	}

	if _, err := LoadConfig(`{"channel":"C1","clusters":[{"name":"demo","cluster":"x"}]}`); err == nil {
		t.Error("a real cluster can't be called demo")
	}
}

// TestWriteDemoPayloads saves every demo message version for Block Kit
// Builder when DEMO_PAYLOADS is a directory: `make demo-payloads`.
func TestWriteDemoPayloads(t *testing.T) {
	dir := os.Getenv("DEMO_PAYLOADS")
	if dir == "" {
		t.Skip("set DEMO_PAYLOADS to a directory")
	}
	app, slack, jobs := demoApp(t)
	runDemo(t, app, jobs, "demo")
	var links []string
	write := func(i int, p Payload) {
		meta := "intro"
		if p.Metadata != nil {
			meta = strings.TrimPrefix(p.Metadata.EventType, "argocd_")
			if s, ok := p.Metadata.EventPayload["state"].(string); ok {
				meta += "-" + s
			}
			if a, ok := p.Metadata.EventPayload["app"].(string); ok {
				meta = a + "-" + meta
			}
		}
		body, _ := json.MarshalIndent(map[string]any{"blocks": p.Blocks, "attachments": p.Attachments}, "", "  ")
		name := fmt.Sprintf("%02d-%s.json", i, meta)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
		compactBody, _ := json.Marshal(map[string]any{"blocks": p.Blocks, "attachments": p.Attachments})
		links = append(links, name+" https://app.slack.com/block-kit-builder/#"+url.PathEscape(string(compactBody)))
	}
	i := 0
	for _, p := range append(slack.topLevel(), slack.updates...) {
		if len(p.Blocks) == 0 && len(p.Attachments) == 0 {
			continue
		}
		i++
		write(i, p)
	}
	if err := os.WriteFile(filepath.Join(dir, "links.txt"), []byte(strings.Join(links, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
