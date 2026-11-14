package argocdslack

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// Secret names. Each is an environment variable holding an SSM parameter ARN.
const (
	SecretBotToken      = "SLACK_BOT_TOKEN"
	SecretSigningSecret = "SLACK_SIGNING_SECRET"
	SecretClientID      = "SLACK_CLIENT_ID"
	SecretClientSecret  = "SLACK_CLIENT_SECRET"
	SecretTeam          = "SLACK_TEAM"
)

// Secrets reads and writes the app's Slack credentials.
type Secrets interface {
	Get(ctx context.Context, name string) string
	Put(ctx context.Context, name, value string, secure bool) error
}

const (
	stateCookie = "__Host-argocd_slack_state"
	csrfCookie  = "__Host-argocd_slack_csrf"
)

var (
	clientIDPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	secretPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type setupStatus struct {
	HasApp    bool
	Installed bool
	TeamID    string
	TeamName  string
}

func (a *App) setupStatus(ctx context.Context) setupStatus {
	st := setupStatus{
		HasApp: a.Secrets.Get(ctx, SecretSigningSecret) != "" && a.Secrets.Get(ctx, SecretClientID) != "" &&
			a.Secrets.Get(ctx, SecretClientSecret) != "",
		Installed: a.Secrets.Get(ctx, SecretBotToken) != "",
	}
	st.TeamID, st.TeamName, _ = strings.Cut(a.Secrets.Get(ctx, SecretTeam), "|")
	return st
}

func isSetupPath(p string) bool {
	return p == "/setup" || p == "/slack/oauth/install" || p == "/slack/oauth/callback"
}

func (a *App) baseURL(req events.APIGatewayV2HTTPRequest) string {
	if a.Cfg.Setup.BaseURL != "" {
		return strings.TrimRight(a.Cfg.Setup.BaseURL, "/")
	}
	return "https://" + req.RequestContext.DomainName
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func cookie(req events.APIGatewayV2HTTPRequest, name string) string {
	var all []string
	all = append(all, req.Cookies...)
	if h := req.Headers["cookie"]; h != "" {
		all = append(all, strings.Split(h, ";")...)
	}
	for _, c := range all {
		k, v, ok := strings.Cut(strings.TrimSpace(c), "=")
		if ok && k == name {
			return v
		}
	}
	return ""
}

func setCookie(name, value string, maxAge int) string {
	return name + "=" + value + "; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=" + strconv.Itoa(maxAge)
}

func equal(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func redirect(to string, cookies ...string) events.APIGatewayV2HTTPResponse {
	return events.APIGatewayV2HTTPResponse{StatusCode: http.StatusSeeOther, Headers: map[string]string{"Location": to, "Cache-Control": "no-store"}, Cookies: cookies}
}

func formValues(req events.APIGatewayV2HTTPRequest) url.Values {
	body := req.Body
	if req.IsBase64Encoded {
		if b, err := base64.StdEncoding.DecodeString(body); err == nil {
			body = string(b)
		}
	}
	v, _ := url.ParseQuery(body)
	return v
}

// setup serves the installer: a page that creates the Slack app (from an app
// configuration token or by hand), the OAuth install, and its callback.
func (a *App) setup(ctx context.Context, req events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	if !a.Cfg.Setup.Enabled || a.Secrets == nil {
		return respond(http.StatusNotFound, "not found")
	}
	base := a.baseURL(req)
	method := req.RequestContext.HTTP.Method
	switch {
	case req.RawPath == "/setup" && method == "GET":
		return a.setupPage(ctx, req, base, "", "")
	case req.RawPath == "/setup" && method == "POST":
		return a.setupSubmit(ctx, req, base)
	case req.RawPath == "/slack/oauth/install" && method == "GET":
		return a.oauthStart(ctx, base)
	case req.RawPath == "/slack/oauth/callback" && method == "GET":
		return a.oauthCallback(ctx, req, base)
	}
	return respond(http.StatusMethodNotAllowed, "method not allowed")
}

func (a *App) setupSubmit(ctx context.Context, req events.APIGatewayV2HTTPRequest, base string) events.APIGatewayV2HTTPResponse {
	form := formValues(req)
	if !equal(cookie(req, csrfCookie), form.Get("csrf")) {
		return a.setupPage(ctx, req, base, "", "This form expired. Load the page again and retry.")
	}
	if a.setupStatus(ctx).Installed {
		return a.setupPage(ctx, req, base, "", "The app is already installed, so its credentials can't be replaced here.")
	}
	var app CreatedApp
	switch form.Get("action") {
	case "create":
		token := strings.TrimSpace(form.Get("config_token"))
		if !strings.HasPrefix(token, "xoxe") {
			return a.setupPage(ctx, req, base, "", "That isn't an app configuration token. They start with xoxe.")
		}
		created, err := a.SetupAPI.CreateApp(ctx, token, Manifest(a.Cfg, base))
		if err != nil {
			a.Log.WarnContext(ctx, "failed to create slack app", "error", err)
			return a.setupPage(ctx, req, base, "", "Slack didn't create the app: "+err.Error()+". Generate a fresh token and try again, or create the app yourself below.")
		}
		app = created
	case "credentials":
		app = CreatedApp{
			ClientID:      strings.TrimSpace(form.Get("client_id")),
			ClientSecret:  strings.TrimSpace(form.Get("client_secret")),
			SigningSecret: strings.TrimSpace(form.Get("signing_secret")),
		}
		if !clientIDPattern.MatchString(app.ClientID) || !secretPattern.MatchString(app.ClientSecret) || !secretPattern.MatchString(app.SigningSecret) {
			return a.setupPage(ctx, req, base, "", "Those don't look like Slack credentials. Copy the Client ID, Client Secret and Signing Secret from Basic Information → App Credentials.")
		}
	default:
		return a.setupPage(ctx, req, base, "", "Unknown action.")
	}
	for _, kv := range [][2]string{{SecretSigningSecret, app.SigningSecret}, {SecretClientID, app.ClientID}, {SecretClientSecret, app.ClientSecret}} {
		if err := a.Secrets.Put(ctx, kv[0], kv[1], true); err != nil {
			a.Log.ErrorContext(ctx, "failed to save slack credential", "name", kv[0], "error", err)
			return a.setupPage(ctx, req, base, "", "Couldn't save the credentials to Parameter Store: "+err.Error())
		}
	}
	a.Log.InfoContext(ctx, "slack app credentials saved", "app_id", app.AppID)
	return redirect(base + "/slack/oauth/install")
}

func (a *App) oauthStart(ctx context.Context, base string) events.APIGatewayV2HTTPResponse {
	clientID := a.Secrets.Get(ctx, SecretClientID)
	if clientID == "" {
		return redirect(base + "/setup")
	}
	state := randomToken()
	q := url.Values{
		"client_id":    {clientID},
		"scope":        {strings.Join(BotScopes, ",")},
		"redirect_uri": {base + "/slack/oauth/callback"},
		"state":        {state},
	}
	return redirect("https://slack.com/oauth/v2/authorize?"+q.Encode(), setCookie(stateCookie, state, 600))
}

func (a *App) oauthCallback(ctx context.Context, req events.APIGatewayV2HTTPRequest, base string) events.APIGatewayV2HTTPResponse {
	q := req.QueryStringParameters
	if e := q["error"]; e != "" {
		return a.setupPage(ctx, req, base, "", "Slack didn't install the app ("+e+"). Try Install again.")
	}
	if !equal(cookie(req, stateCookie), q["state"]) {
		return a.setupPage(ctx, req, base, "", "This install link expired or came from another browser. Start the install again.")
	}
	clientID, clientSecret := a.Secrets.Get(ctx, SecretClientID), a.Secrets.Get(ctx, SecretClientSecret)
	inst, err := a.SetupAPI.ExchangeCode(ctx, clientID, clientSecret, q["code"], base+"/slack/oauth/callback")
	if err != nil {
		a.Log.WarnContext(ctx, "failed to finish slack install", "error", err)
		return a.setupPage(ctx, req, base, "", "Slack didn't finish the install: "+err.Error())
	}
	if st := a.setupStatus(ctx); st.TeamID != "" && st.TeamID != inst.TeamID {
		a.Log.WarnContext(ctx, "refused install into another workspace", "team_id", inst.TeamID)
		return a.setupPage(ctx, req, base, "", "This app is already installed in "+st.TeamName+". It can't be moved to another workspace from here.")
	}
	if err := a.Secrets.Put(ctx, SecretBotToken, inst.BotToken, true); err != nil {
		return a.setupPage(ctx, req, base, "", "Couldn't save the bot token to Parameter Store: "+err.Error())
	}
	if err := a.Secrets.Put(ctx, SecretTeam, inst.TeamID+"|"+inst.TeamName, false); err != nil {
		a.Log.WarnContext(ctx, "failed to save slack team", "error", err)
	}
	a.Log.InfoContext(ctx, "slack app installed", "team_id", inst.TeamID, "app_id", inst.AppID)
	resp := a.setupPage(ctx, req, base, "Installed. The app can post now.", "")
	resp.Cookies = append(resp.Cookies, setCookie(stateCookie, "", 0))
	return resp
}

type setupView struct {
	AppName      string
	Command      string
	Status       setupStatus
	Notice       string
	Error        string
	CSRF         string
	ManifestURL  string
	ManifestJSON string
	InstallURL   string
	Channels     []channelLink
	DryRun       bool
}

type channelLink struct {
	ID, URL string
}

func (a *App) setupPage(ctx context.Context, req events.APIGatewayV2HTTPRequest, base, notice, errText string) events.APIGatewayV2HTTPResponse {
	st := a.setupStatus(ctx)
	csrf := cookie(req, csrfCookie)
	if csrf == "" {
		csrf = randomToken()
	}
	manifest, _ := json.MarshalIndent(Manifest(a.Cfg, base), "", "  ")
	compact, _ := json.Marshal(Manifest(a.Cfg, base))
	v := setupView{
		AppName:      a.Cfg.Setup.AppName,
		Command:      a.Cfg.Setup.Command,
		Status:       st,
		Notice:       notice,
		Error:        errText,
		CSRF:         csrf,
		ManifestURL:  "https://api.slack.com/apps?new_app=1&manifest_json=" + url.QueryEscape(string(compact)),
		ManifestJSON: string(manifest),
		InstallURL:   base + "/slack/oauth/install",
		DryRun:       a.Cfg.DryRun,
	}
	seen := map[string]bool{}
	for _, cl := range a.Cfg.Clusters {
		ch := a.Cfg.ChannelFor(cl)
		if cl.Enabled && ch != "" && !seen[ch] {
			seen[ch] = true
			link := ""
			if st.TeamID != "" {
				link = "https://app.slack.com/client/" + url.PathEscape(st.TeamID) + "/" + url.PathEscape(ch)
			}
			v.Channels = append(v.Channels, channelLink{ID: ch, URL: link})
		}
	}
	var buf bytes.Buffer
	if err := setupTemplate.Execute(&buf, v); err != nil {
		return respond(http.StatusInternalServerError, "failed to render the setup page")
	}
	code := http.StatusOK
	if errText != "" {
		code = http.StatusBadRequest
	}
	return events.APIGatewayV2HTTPResponse{
		StatusCode: code,
		Body:       buf.String(),
		Cookies:    []string{setCookie(csrfCookie, csrf, 3600)},
		Headers: map[string]string{
			"Content-Type":            "text/html; charset=utf-8",
			"Cache-Control":           "no-store",
			"Referrer-Policy":         "no-referrer",
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
		},
	}
}

var setupTemplate = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Set up {{.AppName}} for Slack</title>
<style>
:root { --bg:#f5f6f8; --card:#fff; --fg:#16191d; --muted:#5e6672; --line:#dfe3e8; --accent:#4a154b; --ok:#1f8a5b; --bad:#c62f4f; }
@media (prefers-color-scheme: dark) { :root { --bg:#111317; --card:#1a1d22; --fg:#e6e8eb; --muted:#9aa2ad; --line:#2c3138; --accent:#d9a3db; --ok:#45c08a; --bad:#f2647f; } }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--fg); font:15px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; padding:40px 16px; }
main { max-width:640px; margin:0 auto; display:grid; gap:20px; }
h1 { font-size:24px; margin:0; }
h2 { font-size:17px; margin:0 0 6px; }
p { margin:0; }
a { color:var(--accent); }
form { display:grid; gap:12px; }
.muted { color:var(--muted); }
.steps { display:flex; gap:8px; flex-wrap:wrap; font-size:13px; }
.steps span { border:1px solid var(--line); border-radius:999px; padding:2px 10px; color:var(--muted); }
.steps .done { color:var(--ok); border-color:var(--ok); }
.steps .now { color:var(--fg); border-color:var(--fg); font-weight:600; }
.card { background:var(--card); border:1px solid var(--line); border-radius:10px; padding:20px; display:grid; gap:14px; }
label { display:grid; gap:4px; font-weight:600; font-size:14px; }
input { font:inherit; padding:8px 10px; border:1px solid var(--line); border-radius:6px; background:var(--bg); color:var(--fg); width:100%; }
button, a.button { font:inherit; font-weight:600; border:0; border-radius:6px; padding:9px 16px; background:var(--accent); color:var(--card); cursor:pointer; text-decoration:none; display:inline-block; justify-self:start; }
a.button.secondary { background:transparent; color:var(--fg); border:1px solid var(--line); }
details { display:grid; gap:12px; }
.notice { border-left:4px solid var(--ok); padding:10px 14px; background:var(--card); border-radius:6px; }
.error { border-left:4px solid var(--bad); padding:10px 14px; background:var(--card); border-radius:6px; }
details summary { cursor:pointer; font-weight:600; }
h3 { font-size:15px; margin:0; display:flex; gap:8px; align-items:center; }
.option { display:grid; gap:12px; padding:14px; border:1px solid var(--line); border-radius:8px; }
.tag { font-size:12px; font-weight:600; color:var(--ok); border:1px solid var(--ok); border-radius:999px; padding:0 8px; }
.hint { font-size:13px; color:var(--muted); }
pre { overflow-x:auto; background:var(--bg); border:1px solid var(--line); border-radius:6px; padding:10px; font-size:12px; }
ol { margin:0; padding-left:20px; display:grid; gap:6px; }
code { font-size:13px; }
</style>
</head>
<body>
<main>
  <header>
    <h1>Set up {{.AppName}} for Slack</h1>
    <p class="muted">This page creates the Slack app for this deployment and saves its credentials in AWS Systems Manager Parameter Store. Nothing is stored in your browser.</p>
  </header>
  <div class="steps">
    <span class="{{if .Status.HasApp}}done{{else}}now{{end}}">1 · Create the app</span>
    <span class="{{if .Status.Installed}}done{{else if .Status.HasApp}}now{{end}}">2 · Install it</span>
    <span class="{{if .Status.Installed}}now{{end}}">3 · Invite it to channels</span>
  </div>
  {{if .Notice}}<div class="notice">{{.Notice}}</div>{{end}}
  {{if .Error}}<div class="error">{{.Error}}</div>{{end}}

  {{if not .Status.HasApp}}
  <section class="card">
    <h2>Create the app</h2>
    <p>Do this in the Slack workspace the bot should post in, with an account that may create apps there. Either way below makes the same app from this deployment's manifest.</p>
    <div class="option">
      <h3>Let this page create it <span class="tag">fastest</span></h3>
      <ol>
        <li>Open <a href="https://api.slack.com/apps" target="_blank" rel="noopener">api.slack.com/apps</a> in a new tab.</li>
        <li>Below your list of apps, find <b>Your App Configuration Tokens</b> and click <b>Generate Token</b>.</li>
        <li>Choose the workspace and generate the token.</li>
        <li>Copy the <b>access token</b> (it starts with <code>xoxe.xoxp-</code>), not the refresh token, and paste it here.</li>
      </ol>
      <form method="post" action="/setup">
        <input type="hidden" name="csrf" value="{{.CSRF}}">
        <input type="hidden" name="action" value="create">
        <label for="config_token">Access token
          <input id="config_token" name="config_token" type="password" autocomplete="off" required placeholder="xoxe.xoxp-…">
        </label>
        <p><button type="submit">Create the Slack app</button></p>
      </form>
      <p class="hint">The token is used once, right now, and isn't saved; it expires by itself after 12 hours. Generating it adds Slack's own <i>Slack Tooling Tokens Vendor</i> app to the workspace, which is expected.</p>
    </div>
    <details>
      <summary>Can't generate a token? Create the app yourself instead</summary>
      <ol>
        <li><a class="button secondary" href="{{.ManifestURL}}" target="_blank" rel="noopener">Open Slack with this app's manifest</a><br><span class="hint">Slack's <b>Create an app</b> page opens in a new tab with everything filled in.</span></li>
        <li>Choose the workspace, click <b>Next</b>, check the summary, and click <b>Create</b>.</li>
        <li>On the new app's <b>Basic Information</b> page, under <b>App Credentials</b>, copy the <b>Client ID</b>. Click <b>Show</b> next to <b>Client Secret</b> and <b>Signing Secret</b> and copy those too.</li>
        <li>Paste all three here and click <b>Save credentials</b>.</li>
      </ol>
      <form method="post" action="/setup">
        <input type="hidden" name="csrf" value="{{.CSRF}}">
        <input type="hidden" name="action" value="credentials">
        <label for="client_id">Client ID <input id="client_id" name="client_id" autocomplete="off" required placeholder="1234567890.1234567890"></label>
        <label for="client_secret">Client Secret <input id="client_secret" name="client_secret" type="password" autocomplete="off" required></label>
        <label for="signing_secret">Signing Secret <input id="signing_secret" name="signing_secret" type="password" autocomplete="off" required></label>
        <p><button type="submit">Save credentials</button></p>
      </form>
    </details>
    <details>
      <summary>See the manifest</summary>
      <pre>{{.ManifestJSON}}</pre>
    </details>
    <p class="hint">Next you'll install the app, which takes one click in Slack.</p>
  </section>
  {{else if not .Status.Installed}}
  <section class="card">
    <h2>Install it in your workspace</h2>
    <ol>
      <li>Click <b>Install to Slack</b>. Slack lists what the app may do: post and edit its own messages, add the <code>{{.Command}}</code> command, and read history in channels it's invited to.</li>
      <li>Check the workspace in the top right of Slack's page and click <b>Allow</b>.</li>
      <li>Slack sends you back here when it's done.</li>
    </ol>
    <p><a class="button" href="{{.InstallURL}}">Install to Slack</a></p>
    <p class="hint">If your workspace needs an admin to approve new apps, Slack offers to request approval instead. Once an admin approves, come back to this page and click <b>Install to Slack</b> again.</p>
  </section>
  {{else}}
  <section class="card">
    <h2>Installed{{if .Status.TeamName}} in {{.Status.TeamName}}{{end}}</h2>
    <ol>
      <li>Invite the bot to {{if .Channels}}{{range $i, $c := .Channels}}{{if $i}}, {{end}}{{if $c.URL}}<a href="{{$c.URL}}" target="_blank" rel="noopener">{{$c.ID}}</a>{{else}}{{$c.ID}}{{end}}{{end}}{{else}}your channels{{end}}: type <code>/invite @{{.AppName}}</code> there.</li>
      {{if .DryRun}}<li>Turn off <code>dry_run</code> when you're ready for real posts.</li>{{end}}
      <li>Turn setup off (<code>setup.enabled = false</code>) and redeploy, so this page goes away.</li>
    </ol>
    <p class="muted">Changed the app's scopes? <a href="{{.InstallURL}}">Reinstall</a>. It stays in this workspace.</p>
  </section>
  {{end}}
</main>
</body>
</html>
`))
