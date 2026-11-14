package argocdslack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// Job is an async or scheduled unit of work for this same Lambda.
type Job struct {
	Job     string     `json:"job"`
	User    string     `json:"user,omitempty"`
	Channel string     `json:"channel,omitempty"`
	Cluster string     `json:"cluster,omitempty"`
	App     string     `json:"app,omitempty"`
	ID      string     `json:"id,omitempty"`
	Mute    *Mute      `json:"mute,omitempty"`
	Demo    *DemoState `json:"demo,omitempty"`
}

// Handle is the Lambda entry point: API Gateway requests, scheduler inputs
// and async jobs all arrive here.
func (a *App) Handle(ctx context.Context, raw json.RawMessage, requestID string) (any, error) {
	if !a.Cfg.Rich() {
		a.richOff.Store(true)
	}
	var probe struct {
		Job            string          `json:"job"`
		RawPath        string          `json:"rawPath"`
		RequestContext json.RawMessage `json:"requestContext"`
	}
	_ = json.Unmarshal(raw, &probe)
	if probe.RawPath != "" && probe.RequestContext != nil {
		return a.httpRaw(ctx, raw), nil
	}
	var job Job
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, err
	}
	return nil, a.RunJob(ctx, job, requestID)
}

func (a *App) httpRaw(ctx context.Context, raw json.RawMessage) events.APIGatewayV2HTTPResponse {
	var req events.APIGatewayV2HTTPRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		a.Log.WarnContext(ctx, "failed to decode api gateway request", "error", err)
		return respond(http.StatusBadRequest, "bad request")
	}
	return a.HTTP(ctx, req, a.now())
}

// RunJob runs a scheduled or async job.
func (a *App) RunJob(ctx context.Context, job Job, requestID string) error {
	switch job.Job {
	case "poll":
		return a.Poll(ctx, requestID)
	case "digest":
		return a.Digest(ctx)
	case "home":
		return a.Home(ctx, job.User)
	case "ack":
		return a.Ack(ctx, job)
	case "mute_created":
		return a.muteCreated(ctx, job)
	case "demo":
		return a.RunDemo(ctx, job.Demo)
	}
	return fmt.Errorf("unknown job %q", job.Job)
}

func respond(code int, body string) events.APIGatewayV2HTTPResponse {
	return events.APIGatewayV2HTTPResponse{StatusCode: code, Body: body, Headers: map[string]string{"Content-Type": "text/plain"}}
}

func respondJSON(v any) events.APIGatewayV2HTTPResponse {
	b, _ := json.Marshal(v)
	return events.APIGatewayV2HTTPResponse{StatusCode: http.StatusOK, Body: string(b), Headers: map[string]string{"Content-Type": "application/json"}}
}

// HTTP serves the installer pages and routes Slack's requests after checking
// their signature.
func (a *App) HTTP(ctx context.Context, req events.APIGatewayV2HTTPRequest, now time.Time) events.APIGatewayV2HTTPResponse {
	if req.RawPath == "/healthz" {
		return respond(http.StatusOK, "ok")
	}
	if isSetupPath(req.RawPath) {
		return a.setup(ctx, req)
	}
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		b, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return respond(http.StatusBadRequest, "bad body")
		}
		body = b
	}
	h := func(k string) string {
		for hk, v := range req.Headers {
			if strings.EqualFold(hk, k) {
				return v
			}
		}
		return ""
	}
	// Slack checks the events URL while the app is being created, before
	// this deployment has its signing secret; echoing the challenge is safe.
	if req.RawPath == "/slack/events" {
		var probe struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if json.Unmarshal(body, &probe) == nil && probe.Type == "url_verification" {
			return respondJSON(map[string]string{"challenge": probe.Challenge})
		}
	}
	secret := ""
	if a.Secrets != nil {
		secret = a.Secrets.Get(ctx, SecretSigningSecret)
	}
	if secret == "" {
		return respond(http.StatusServiceUnavailable, "the Slack app isn't set up yet")
	}
	if err := VerifySignature(secret, h("X-Slack-Request-Timestamp"), h("X-Slack-Signature"), body, now); err != nil {
		a.Log.Warn("rejected Slack request", "path", req.RawPath, "error", err)
		return respond(http.StatusUnauthorized, "invalid signature")
	}
	switch req.RawPath {
	case "/slack/events":
		return a.slackEvent(ctx, body)
	case "/slack/commands":
		form, _ := url.ParseQuery(string(body))
		return a.command(ctx, form, now)
	case "/slack/interactivity":
		form, _ := url.ParseQuery(string(body))
		return a.interaction(ctx, []byte(form.Get("payload")), now)
	}
	return respond(http.StatusNotFound, "not found")
}

func (a *App) async(ctx context.Context, job Job) {
	if a.Invoke == nil {
		go func() { _ = a.RunJob(context.WithoutCancel(ctx), job, "local") }()
		return
	}
	if err := a.Invoke(ctx, job); err != nil {
		a.Log.Error("enqueue job", "job", job.Job, "error", err)
	}
}

func (a *App) slackEvent(ctx context.Context, body []byte) events.APIGatewayV2HTTPResponse {
	var ev struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Event     struct {
			Type string `json:"type"`
			User string `json:"user"`
			Tab  string `json:"tab"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return respond(http.StatusBadRequest, "bad event")
	}
	switch ev.Type {
	case "url_verification":
		return respondJSON(map[string]string{"challenge": ev.Challenge})
	case "event_callback":
		if ev.Event.Type == "app_home_opened" && (ev.Event.Tab == "" || ev.Event.Tab == "home") {
			a.async(ctx, Job{Job: "home", User: ev.Event.User})
		}
	}
	return respond(http.StatusOK, "")
}

func ephemeral(text string) events.APIGatewayV2HTTPResponse {
	return respondJSON(map[string]string{"response_type": "ephemeral", "text": text})
}

const helpText = "*/argocd* commands:\n" +
	"• `/argocd mute` opens the mute form\n" +
	"• `/argocd mute api@prod-use1 4h waiting on a fix` mutes one app on one cluster (also a cluster such as `prod-use1`, an environment such as `prod`, or an app everywhere; durations like `1h`, `1d`, `1w`, or `until-healthy`)\n" +
	"• `/argocd mutes` lists active mutes\n" +
	"• `/argocd unmute <id or scope>` ends a mute\n" +
	"• `/argocd status [app]` shows what's unhealthy\n" +
	"• `/argocd demo` plays every message type in this channel with made-up apps; `/argocd demo incident` plays one story (deploy, failed, unhealthy, incident, baseline, digest)"

func (a *App) command(ctx context.Context, form url.Values, now time.Time) events.APIGatewayV2HTTPResponse {
	user, channel := form.Get("user_id"), form.Get("channel_id")
	args := strings.Fields(form.Get("text"))
	if len(args) == 0 {
		return ephemeral(helpText)
	}
	switch strings.ToLower(args[0]) {
	case "mute":
		if len(args) == 1 {
			if err := a.Slack.OpenView(ctx, form.Get("trigger_id"), RenderMuteModal(a.Cfg, MuteContext{Channel: channel})); err != nil {
				return ephemeral("Couldn't open the mute form: " + err.Error())
			}
			return respond(http.StatusOK, "")
		}
		m, err := ParseMuteArgs(a.Cfg, args[1:], now)
		if err != nil {
			return ephemeral(err.Error())
		}
		m.ID, m.By = NewMuteID(), user
		if err := a.Store.PutMute(ctx, m); err != nil {
			return ephemeral("Couldn't save the mute: " + err.Error())
		}
		a.async(ctx, Job{Job: "mute_created", Mute: &m, Channel: channel, User: user})
		return ephemeral(fmt.Sprintf("🔕 Muted %s (%s) %s. Undo with `/argocd unmute %s`.", m.Scope(), strings.Join(m.Kinds, ", "), m.UntilText(), m.ID))
	case "mutes":
		mutes, err := a.Store.Mutes(ctx)
		if err != nil {
			return ephemeral("Couldn't read mutes: " + err.Error())
		}
		var lines []string
		for _, m := range mutes {
			if m.Active(now) {
				lines = append(lines, fmt.Sprintf("• `%s` %s · %s · <@%s> %s", m.ID, m.Scope(), m.UntilText(), m.By, m.Reason))
			}
		}
		if len(lines) == 0 {
			return ephemeral("Nothing is muted.")
		}
		return ephemeral(strings.Join(lines, "\n"))
	case "unmute":
		if len(args) < 2 {
			return ephemeral("Say which mute, e.g. `/argocd unmute api@prod-use1` or its ID from `/argocd mutes`.")
		}
		mutes, err := a.Store.Mutes(ctx)
		if err != nil {
			return ephemeral("Couldn't read mutes: " + err.Error())
		}
		target := strings.ToLower(strings.Join(args[1:], " "))
		var ended []string
		for _, m := range mutes {
			scope := strings.ToLower(m.Scope())
			if m.ID == target || scope == target || strings.TrimPrefix(scope, "all of ") == target || strings.TrimSuffix(scope, " everywhere") == target {
				if err := a.Store.DeleteMute(ctx, m.ID); err == nil {
					ended = append(ended, m.Scope())
				}
			}
		}
		if len(ended) == 0 {
			return ephemeral("No active mute matches `" + target + "`. See `/argocd mutes`.")
		}
		return ephemeral("🔔 Unmuted " + strings.Join(ended, ", ") + ".")
	case "status":
		return ephemeral(a.statusText(ctx, args[1:]))
	case "demo":
		return a.startDemo(ctx, channel, user, args[1:], now)
	}
	return ephemeral(helpText)
}

func (a *App) statusText(ctx context.Context, args []string) string {
	filter := ""
	if len(args) > 0 {
		filter = strings.ToLower(args[0])
	}
	var lines []string
	for _, cl := range a.Cfg.Clusters {
		if !cl.Enabled {
			continue
		}
		recs, meta, err := a.Store.LoadCluster(ctx, cl.Name)
		if err != nil {
			lines = append(lines, fmt.Sprintf("*%s*: couldn't read state", cl.Name))
			continue
		}
		var bad []string
		healthy := 0
		for _, r := range recs {
			if filter != "" && r.App != filter {
				continue
			}
			if healthyish(r.Health) || r.Health == "Progressing" {
				healthy++
				continue
			}
			bad = append(bad, fmt.Sprintf("<%s|%s> %s", cl.AppURL(r.App), r.App, strings.ToLower(r.Health)))
		}
		sort.Strings(bad)
		line := fmt.Sprintf("*%s*: %d healthy", cl.Name, healthy)
		if len(bad) > 0 {
			line += " · " + strings.Join(bad, ", ")
		}
		if !meta.LastPoll.IsZero() {
			line += " · read " + slackTime(meta.LastPoll, "{time}")
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "No clusters are watched yet."
	}
	return strings.Join(lines, "\n")
}

type interactionPayload struct {
	Type      string `json:"type"`
	TriggerID string `json:"trigger_id"`
	User      struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Actions []struct {
		ActionID       string `json:"action_id"`
		Value          string `json:"value"`
		SelectedOption *struct {
			Value string `json:"value"`
		} `json:"selected_option"`
	} `json:"actions"`
	View struct {
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		State           struct {
			Values map[string]map[string]json.RawMessage `json:"values"`
		} `json:"state"`
	} `json:"view"`
}

func (a *App) interaction(ctx context.Context, raw []byte, now time.Time) events.APIGatewayV2HTTPResponse {
	var p interactionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return respond(http.StatusBadRequest, "bad payload")
	}
	switch p.Type {
	case "block_actions":
		if len(p.Actions) == 0 {
			return respond(http.StatusOK, "")
		}
		act := p.Actions[0]
		v := act.Value
		if act.SelectedOption != nil {
			v = act.SelectedOption.Value
		}
		parts := strings.Split(v, "|")
		if demoAction(act.ActionID, parts) {
			if err := a.Slack.Ephemeral(ctx, p.Channel.ID, p.User.ID, "That's a demo message, so its buttons don't act on anything."); err != nil {
				a.Log.WarnContext(ctx, "failed to answer a demo button", "error", err)
			}
			return respond(http.StatusOK, "")
		}
		switch {
		case act.ActionID == "ack" && len(parts) == 3:
			a.async(ctx, Job{Job: "ack", Cluster: parts[0], App: parts[1], ID: parts[2], User: p.User.ID, Channel: p.Channel.ID})
		case act.ActionID == "mute_open" && len(parts) == 3:
			a.openMute(ctx, p.TriggerID, MuteContext{Cluster: parts[0], App: parts[1], Kind: parts[2], Channel: p.Channel.ID})
		case act.ActionID == "more" && len(parts) >= 3 && parts[0] == "mute":
			kind := KindIncidents
			if len(parts) > 3 {
				kind = parts[3]
			}
			a.openMute(ctx, p.TriggerID, MuteContext{Cluster: parts[1], App: parts[2], Kind: kind, Channel: p.Channel.ID})
		case act.ActionID == "unmute":
			mutes, _ := a.Store.Mutes(ctx)
			for _, m := range mutes {
				if m.ID == v {
					_ = a.Store.DeleteMute(ctx, m.ID)
				}
			}
			a.async(ctx, Job{Job: "home", User: p.User.ID})
		}
		return respond(http.StatusOK, "")
	case "view_submission":
		if p.View.CallbackID != "mute_submit" {
			return respond(http.StatusOK, "")
		}
		var mc MuteContext
		_ = json.Unmarshal([]byte(p.View.PrivateMetadata), &mc)
		m, field, err := muteFromForm(p.View.State.Values, now)
		if err != nil {
			return respondJSON(map[string]any{"response_action": "errors", "errors": map[string]string{field: err.Error()}})
		}
		m.ID, m.By = NewMuteID(), p.User.ID
		if err := a.Store.PutMute(ctx, m); err != nil {
			return respondJSON(map[string]any{"response_action": "errors", "errors": map[string]string{"reason": "Couldn't save the mute: " + err.Error()}})
		}
		a.async(ctx, Job{Job: "mute_created", Mute: &m, Channel: mc.Channel, User: p.User.ID})
		return respond(http.StatusOK, "")
	}
	return respond(http.StatusOK, "")
}

func (a *App) openMute(ctx context.Context, trigger string, mc MuteContext) {
	if err := a.Slack.OpenView(ctx, trigger, RenderMuteModal(a.Cfg, mc)); err != nil {
		a.Log.Error("open mute modal", "error", err)
	}
}

func muteFromForm(values map[string]map[string]json.RawMessage, now time.Time) (Mute, string, error) {
	get := func(block string) json.RawMessage { return values[block]["v"] }
	var scope struct {
		SelectedOption struct {
			Value string `json:"value"`
		} `json:"selected_option"`
	}
	_ = json.Unmarshal(get("scope"), &scope)
	var dur struct {
		SelectedOption struct {
			Value string `json:"value"`
		} `json:"selected_option"`
	}
	_ = json.Unmarshal(get("duration"), &dur)
	var until struct {
		SelectedDateTime int64 `json:"selected_date_time"`
	}
	_ = json.Unmarshal(get("until"), &until)
	var kinds struct {
		SelectedOptions []struct {
			Value string `json:"value"`
		} `json:"selected_options"`
	}
	_ = json.Unmarshal(get("kinds"), &kinds)
	var reason struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(get("reason"), &reason)

	m := Mute{CreatedAt: now, Reason: trimTo(reason.Value, 140)}
	sv := scope.SelectedOption.Value
	switch {
	case strings.HasPrefix(sv, "app@"):
		rest := strings.SplitN(strings.TrimPrefix(sv, "app@"), "@", 2)
		m.App = rest[0]
		if len(rest) == 2 {
			m.Cluster = rest[1]
		}
	case strings.HasPrefix(sv, "cluster@"):
		m.Cluster = strings.TrimPrefix(sv, "cluster@")
	case strings.HasPrefix(sv, "env@"):
		m.Env = strings.TrimPrefix(sv, "env@")
	default:
		return m, "scope", fmt.Errorf("pick what to mute")
	}
	for _, k := range kinds.SelectedOptions {
		m.Kinds = append(m.Kinds, k.Value)
	}
	if len(m.Kinds) == 0 {
		return m, "kinds", fmt.Errorf("pick at least one kind")
	}
	switch dur.SelectedOption.Value {
	case "healthy":
		if m.App == "" {
			return m, "duration", fmt.Errorf("“until healthy” works for one app; pick a time instead")
		}
		m.UntilHealthy, m.Until = true, now.Add(MaxMute)
	case "custom":
		if until.SelectedDateTime == 0 {
			return m, "until", fmt.Errorf("pick an end time")
		}
		m.Until = time.Unix(until.SelectedDateTime, 0)
	default:
		d, err := parseDuration(orDefault(dur.SelectedOption.Value, "4h"))
		if err != nil {
			return m, "duration", err
		}
		m.Until = now.Add(d)
	}
	if !m.Until.After(now) {
		return m, "until", fmt.Errorf("pick a time in the future")
	}
	if m.Until.Sub(now) > MaxMute+time.Minute {
		return m, "until", fmt.Errorf("mutes last a week at most")
	}
	return m, "", nil
}

// Ack records who took an incident and repaints its message.
func (a *App) Ack(ctx context.Context, job Job) error {
	cl, ok := a.Cfg.ClusterByName(job.Cluster)
	if !ok {
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		rec, err := a.Store.GetRecord(ctx, job.Cluster, job.App)
		if err != nil || rec == nil || rec.Incident == nil || rec.Incident.ID != job.ID {
			if job.Channel != "" {
				_ = a.Slack.Ephemeral(ctx, job.Channel, job.User, "That incident is no longer open.")
			}
			return err
		}
		rec.Incident.AckBy = job.User
		rec.Incident.Timeline = append(rec.Incident.Timeline, entry(a.now(), "👀 <@%s> is on it.", job.User))
		mutes, _ := a.Store.Mutes(ctx)
		a.Rerender(ctx, cl, rec, mutes)
		err = a.Store.SaveRecord(ctx, rec)
		if errors.Is(err, ErrConflict) {
			continue
		}
		return err
	}
	return nil
}

func (a *App) muteCreated(ctx context.Context, job Job) error {
	m := job.Mute
	if m == nil {
		return nil
	}
	if job.Channel != "" {
		_ = a.Slack.Ephemeral(ctx, job.Channel, job.User, fmt.Sprintf("🔕 Muted %s (%s) %s. Undo with `/argocd unmute %s`.", m.Scope(), strings.Join(m.Kinds, ", "), m.UntilText(), m.ID))
	}
	if m.Wide() {
		channels := map[string]bool{}
		for _, cl := range a.Cfg.Clusters {
			if cl.Enabled && (m.Cluster == "" || m.Cluster == cl.Name) && (m.Env == "" || m.Env == cl.Env) {
				channels[a.Cfg.ChannelFor(cl)] = true
			}
		}
		for ch := range channels {
			text := fmt.Sprintf("🔕 <@%s> muted %s (%s) %s.", m.By, m.Scope(), strings.Join(m.Kinds, ", "), m.UntilText())
			if m.Reason != "" {
				text += " " + m.Reason
			}
			if msg, err := a.Slack.Post(ctx, Payload{Channel: ch, Text: text}); err == nil && !m.Announced.Posted() {
				m.Announced = msg
				_ = a.Store.PutMute(ctx, *m)
			}
		}
	}
	return nil
}
