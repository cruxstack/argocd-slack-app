// Command preview serves the setup page on localhost with an in-memory secret
// store and a fake Slack, for working on the page and taking screenshots.
// It is never released.
package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/cruxstack/argocd-slack-app/internal/argocdslack"
)

const config = `{
	"channel": "C0123456789",
	"setup": {"enabled": true, "base_url": "https://argocd-slack.example.com"},
	"clusters": [
		{"name": "dev-use1", "env": "dev", "cluster": "dev-cluster", "channel": "C0987654321", "enabled": true},
		{"name": "prod-use1", "env": "prod", "cluster": "prod-cluster", "enabled": true}
	]
}`

type secrets struct {
	mu   sync.Mutex
	vals map[string]string
}

func (s *secrets) Get(_ context.Context, k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vals[k]
}

func (s *secrets) Put(_ context.Context, k, v string, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[k] = v
	return nil
}

// slack creates apps and finishes installs without calling Slack.
type slack struct{}

func (slack) CreateApp(context.Context, string, any) (argocdslack.CreatedApp, error) {
	return argocdslack.CreatedApp{AppID: "A0123456789", ClientID: "1234567890.1234567890", ClientSecret: strings.Repeat("0", 32), SigningSecret: strings.Repeat("1", 32)}, nil
}

func (slack) SetIcon(context.Context, string, string, []byte) error { return nil }

func (slack) ExchangeCode(context.Context, string, string, string, string) (argocdslack.Installation, error) {
	return argocdslack.Installation{BotToken: "xoxb-preview", TeamID: "T0123456789", TeamName: "Acme", AppID: "A0123456789"}, nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	step := flag.Int("step", 1, "start at step 1 (create), 2 (install) or 3 (installed)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := argocdslack.LoadConfig(config)
	if err != nil {
		log.Error("failed to load preview config", "error", err)
		os.Exit(1)
	}
	sec := &secrets{vals: map[string]string{}}
	if *step >= 2 {
		sec.vals[argocdslack.SecretSigningSecret] = strings.Repeat("1", 32)
		sec.vals[argocdslack.SecretClientID] = "1234567890.1234567890"
		sec.vals[argocdslack.SecretClientSecret] = strings.Repeat("0", 32)
	}
	if *step >= 3 {
		sec.vals[argocdslack.SecretBotToken] = "xoxb-preview"
		sec.vals[argocdslack.SecretTeam] = "T0123456789|Acme"
	}
	app := &argocdslack.App{Cfg: cfg, Log: log, Secrets: sec, SetupAPI: slack{}}

	srv := &http.Server{
		Addr:              *addr,
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			req := events.APIGatewayV2HTTPRequest{RawPath: r.URL.Path, Body: string(body), Headers: map[string]string{}, QueryStringParameters: map[string]string{}}
			req.RequestContext.HTTP.Method = r.Method
			req.RequestContext.DomainName = r.Host
			for k := range r.Header {
				req.Headers[strings.ToLower(k)] = r.Header.Get(k)
			}
			for k := range r.URL.Query() {
				req.QueryStringParameters[k] = r.URL.Query().Get(k)
			}
			for _, c := range r.Cookies() {
				req.Cookies = append(req.Cookies, c.Name+"="+c.Value)
			}
			resp := app.HTTP(r.Context(), req, time.Now())
			for k, v := range resp.Headers {
				w.Header().Set(k, v)
			}
			for _, c := range resp.Cookies {
				w.Header().Add("Set-Cookie", c)
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.WriteString(w, resp.Body)
		}),
	}
	log.Info("serving the setup page", "url", "http://"+*addr+"/setup", "step", *step)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
