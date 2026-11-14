//go:build live

package argocdslack

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cruxstack/argocd-slack-app/internal/kube"
)

// TestLiveDryRun polls a real Argo CD through `kubectl proxy` and writes the
// messages it would send to LIVE_OUT. Nothing reaches Slack or AWS.
//
//	kubectl proxy --port 8099 &
//	LIVE_PROXY=http://127.0.0.1:8099 LIVE_OUT=/tmp/out.json go test -tags live -run TestLiveDryRun ./internal/argocdslack
//
// LIVE_ARGOCD_URL and LIVE_NAMESPACE (default argocd) are optional.
func TestLiveDryRun(t *testing.T) {
	proxy, out := os.Getenv("LIVE_PROXY"), os.Getenv("LIVE_OUT")
	if proxy == "" || out == "" {
		t.Skip("set LIVE_PROXY and LIVE_OUT")
	}
	cfg := testConfig(t)
	cfg.WatchSeconds = 1
	cfg.Annotations = Annotations{}
	cfg.Clusters[0].ArgoCDURL = os.Getenv("LIVE_ARGOCD_URL")
	if ns := os.Getenv("LIVE_NAMESPACE"); ns != "" {
		cfg.Clusters[0].Namespace = ns
	}
	client, err := kube.New(proxy, nil, func(context.Context) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	slack := &fakeSlack{}
	app := &App{Cfg: cfg, Store: newMemStore(), Slack: slack, Log: quietLog(),
		Connect: func(context.Context, Cluster) (ClusterAPI, error) { return client, nil }}
	if err := app.Poll(context.Background(), "live"); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(slack.posts, "", "  ")
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d posts written to %s", len(slack.posts), out)
}
