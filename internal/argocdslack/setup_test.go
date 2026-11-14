package argocdslack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

type memSecrets struct {
	mu   sync.Mutex
	vals map[string]string
}

func (m *memSecrets) Get(_ context.Context, k string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.vals[k]
}

func (m *memSecrets) Put(_ context.Context, k, v string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vals == nil {
		m.vals = map[string]string{}
	}
	m.vals[k] = v
	return nil
}

type fakeSetupAPI struct {
	manifest any
	token    string
	team     string
	failWith error
}

func (f *fakeSetupAPI) CreateApp(_ context.Context, token string, manifest any) (CreatedApp, error) {
	if f.failWith != nil {
		return CreatedApp{}, f.failWith
	}
	f.token, f.manifest = token, manifest
	return CreatedApp{AppID: "A01", ClientID: "111.222", ClientSecret: strings.Repeat("a", 32), SigningSecret: strings.Repeat("b", 32)}, nil
}

func (f *fakeSetupAPI) ExchangeCode(_ context.Context, clientID, _, code, redirect string) (Installation, error) {
	if clientID != "111.222" || code != "c0de" || !strings.HasSuffix(redirect, "/slack/oauth/callback") {
		return Installation{}, errors.New("bad exchange")
	}
	return Installation{BotToken: "xoxb-1", TeamID: f.team, TeamName: "Acme", AppID: "A01"}, nil
}

func setupApp(t *testing.T) (*App, *memSecrets, *fakeSetupAPI) {
	cfg := testConfig(t)
	cfg.Setup.Enabled = true
	sec := &memSecrets{}
	api := &fakeSetupAPI{team: "T1"}
	return &App{Cfg: cfg, Log: quietLog(), Secrets: sec, SetupAPI: api}, sec, api
}

func request(method, path string, form url.Values, cookies []string, query map[string]string) events.APIGatewayV2HTTPRequest {
	req := events.APIGatewayV2HTTPRequest{RawPath: path, Cookies: cookies, QueryStringParameters: query}
	req.RequestContext.HTTP.Method = method
	req.RequestContext.DomainName = "abc123.execute-api.us-east-1.amazonaws.com"
	if form != nil {
		req.Body = form.Encode()
	}
	return req
}

func csrfFrom(t *testing.T, resp events.APIGatewayV2HTTPResponse) (string, string) {
	t.Helper()
	for _, c := range resp.Cookies {
		if strings.HasPrefix(c, csrfCookie+"=") {
			v := strings.TrimPrefix(strings.SplitN(c, ";", 2)[0], csrfCookie+"=")
			return v, csrfCookie + "=" + v
		}
	}
	t.Fatal("no csrf cookie")
	return "", ""
}

func TestSetupDisabledIs404(t *testing.T) {
	app, _, _ := setupApp(t)
	app.Cfg.Setup.Enabled = false
	if resp := app.HTTP(context.Background(), request("GET", "/setup", nil, nil, nil), t0); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestSetupWithConfigToken(t *testing.T) {
	app, sec, api := setupApp(t)
	ctx := context.Background()
	page := app.HTTP(ctx, request("GET", "/setup", nil, nil, nil), t0)
	if page.StatusCode != http.StatusOK || !strings.Contains(page.Body, "Create the Slack app") || !strings.Contains(page.Body, "api.slack.com/apps?new_app=1&amp;manifest_json=") {
		t.Fatalf("step 1 page: %d", page.StatusCode)
	}
	if !strings.Contains(page.Headers["Content-Security-Policy"], "frame-ancestors 'none'") {
		t.Fatal("missing security headers")
	}
	csrf, cookie := csrfFrom(t, page)

	// A form posted without the matching cookie is refused.
	bad := app.HTTP(ctx, request("POST", "/setup", url.Values{"csrf": {"nope"}, "action": {"create"}, "config_token": {"xoxe.xoxp-1"}}, []string{cookie}, nil), t0)
	if bad.StatusCode != http.StatusBadRequest || sec.Get(ctx, SecretClientID) != "" {
		t.Fatal("csrf mismatch must not save anything")
	}

	resp := app.HTTP(ctx, request("POST", "/setup", url.Values{"csrf": {csrf}, "action": {"create"}, "config_token": {"xoxe.xoxp-1"}}, []string{cookie}, nil), t0)
	if resp.StatusCode != http.StatusSeeOther || resp.Headers["Location"] != "https://abc123.execute-api.us-east-1.amazonaws.com/slack/oauth/install" {
		t.Fatalf("want redirect to install, got %d %v", resp.StatusCode, resp.Headers)
	}
	if api.token != "xoxe.xoxp-1" || sec.Get(ctx, SecretSigningSecret) != strings.Repeat("b", 32) || sec.Get(ctx, SecretClientID) != "111.222" {
		t.Fatal("credentials not saved")
	}
	m := api.manifest.(map[string]any)
	settings := m["settings"].(map[string]any)
	if settings["interactivity"].(map[string]any)["request_url"] != "https://abc123.execute-api.us-east-1.amazonaws.com/slack/interactivity" {
		t.Fatalf("manifest urls: %+v", settings)
	}

	install := app.HTTP(ctx, request("GET", "/slack/oauth/install", nil, nil, nil), t0)
	loc, _ := url.Parse(install.Headers["Location"])
	state := loc.Query().Get("state")
	if loc.Host != "slack.com" || loc.Query().Get("client_id") != "111.222" || state == "" || !strings.Contains(loc.Query().Get("scope"), "chat:write") {
		t.Fatalf("authorize redirect: %s", install.Headers["Location"])
	}
	stateCookieValue := stateCookie + "=" + state

	// A callback whose state doesn't match the browser's cookie is refused.
	forged := app.HTTP(ctx, request("GET", "/slack/oauth/callback", nil, []string{stateCookie + "=other"}, map[string]string{"code": "c0de", "state": state}), t0)
	if forged.StatusCode != http.StatusBadRequest || sec.Get(ctx, SecretBotToken) != "" {
		t.Fatal("state mismatch must not install")
	}

	done := app.HTTP(ctx, request("GET", "/slack/oauth/callback", nil, []string{stateCookieValue}, map[string]string{"code": "c0de", "state": state}), t0)
	if done.StatusCode != http.StatusOK || !strings.Contains(done.Body, "Installed in Acme") || sec.Get(ctx, SecretBotToken) != "xoxb-1" || sec.Get(ctx, SecretTeam) != "T1|Acme" {
		t.Fatalf("install: %d %q", done.StatusCode, sec.Get(ctx, SecretBotToken))
	}
	if !strings.Contains(done.Body, "app.slack.com/client/T1/CTEST") {
		t.Fatal("done page should link the channel to invite the bot to")
	}

	// Once installed, credentials can't be replaced and the workspace is locked.
	again := app.HTTP(ctx, request("POST", "/setup", url.Values{"csrf": {csrf}, "action": {"create"}, "config_token": {"xoxe.xoxp-2"}}, []string{cookie}, nil), t0)
	if again.StatusCode != http.StatusBadRequest || api.token != "xoxe.xoxp-1" {
		t.Fatal("installed app must not be replaced")
	}
	api.team = "T2"
	moved := app.HTTP(ctx, request("GET", "/slack/oauth/callback", nil, []string{stateCookieValue}, map[string]string{"code": "c0de", "state": state}), t0)
	if moved.StatusCode != http.StatusBadRequest || sec.Get(ctx, SecretTeam) != "T1|Acme" {
		t.Fatal("install into another workspace must be refused")
	}
}

func TestSetupWithPastedCredentials(t *testing.T) {
	app, sec, _ := setupApp(t)
	ctx := context.Background()
	csrf, cookie := csrfFrom(t, app.HTTP(ctx, request("GET", "/setup", nil, nil, nil), t0))
	bad := app.HTTP(ctx, request("POST", "/setup", url.Values{"csrf": {csrf}, "action": {"credentials"}, "client_id": {"x"}, "client_secret": {"y"}, "signing_secret": {"z"}}, []string{cookie}, nil), t0)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatal("malformed credentials accepted")
	}
	ok := app.HTTP(ctx, request("POST", "/setup", url.Values{"csrf": {csrf}, "action": {"credentials"}, "client_id": {"111.222"},
		"client_secret": {strings.Repeat("c", 32)}, "signing_secret": {strings.Repeat("d", 32)}}, []string{cookie}, nil), t0)
	if ok.StatusCode != http.StatusSeeOther || sec.Get(ctx, SecretClientSecret) != strings.Repeat("c", 32) {
		t.Fatalf("credentials: %d", ok.StatusCode)
	}
	page := app.HTTP(ctx, request("GET", "/setup", nil, []string{cookie}, nil), t0)
	if !strings.Contains(page.Body, "Install to Slack") {
		t.Fatal("step 2 should offer the install")
	}
}

func TestSetupReportsSlackErrors(t *testing.T) {
	app, _, api := setupApp(t)
	api.failWith = &SlackError{Method: "apps.manifest.create", Code: "token_expired"}
	ctx := context.Background()
	csrf, cookie := csrfFrom(t, app.HTTP(ctx, request("GET", "/setup", nil, nil, nil), t0))
	resp := app.HTTP(ctx, request("POST", "/setup", url.Values{"csrf": {csrf}, "action": {"create"}, "config_token": {"xoxe.xoxp-1"}}, []string{cookie}, nil), t0)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(resp.Body, "token_expired") {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestEventsURLVerificationBeforeSetup(t *testing.T) {
	app, _, _ := setupApp(t)
	req := request("POST", "/slack/events", nil, nil, nil)
	req.Body = `{"type":"url_verification","challenge":"abc"}`
	resp := app.HTTP(context.Background(), req, t0)
	var out map[string]string
	_ = json.Unmarshal([]byte(resp.Body), &out)
	if resp.StatusCode != http.StatusOK || out["challenge"] != "abc" {
		t.Fatalf("challenge: %d %s", resp.StatusCode, resp.Body)
	}
	other := request("POST", "/slack/commands", nil, nil, nil)
	if got := app.HTTP(context.Background(), other, t0); got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unsigned commands before setup should be refused, got %d", got.StatusCode)
	}
}

func TestManifestShape(t *testing.T) {
	cfg := testConfig(t)
	m := Manifest(cfg, "https://x.example.com/")
	b, _ := json.Marshal(m)
	for _, want := range []string{`"command":"/argocd"`, `"redirect_urls":["https://x.example.com/slack/oauth/callback"]`, `"bot":["chat:write","commands","channels:history","usergroups:read"]`, `"request_url":"https://x.example.com/slack/events"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("manifest missing %s", want)
		}
	}
}
