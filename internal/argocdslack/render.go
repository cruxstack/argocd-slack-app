package argocdslack

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Payload is a chat.postMessage / chat.update body.
type Payload struct {
	Channel     string       `json:"channel,omitempty"`
	TS          string       `json:"ts,omitempty"`
	ThreadTS    string       `json:"thread_ts,omitempty"`
	Text        string       `json:"text"`
	Blocks      []Block      `json:"blocks,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Metadata    *Metadata    `json:"metadata,omitempty"`
	UnfurlLinks bool         `json:"unfurl_links"`
	UnfurlMedia bool         `json:"unfurl_media"`
}

// Block is one Block Kit block.
type Block = map[string]any

// Attachment carries the status color bar around the blocks.
type Attachment struct {
	Color    string  `json:"color"`
	Fallback string  `json:"fallback,omitempty"`
	Blocks   []Block `json:"blocks"`
}

// Metadata lets the app find its own messages again.
type Metadata struct {
	EventType    string         `json:"event_type"`
	EventPayload map[string]any `json:"event_payload"`
}

// View holds what a render needs beyond the record itself.
type View struct {
	Cfg       Config
	Cluster   Cluster
	App       string
	Namespace string
	Mute      *Mute
	Rich      bool
	Now       time.Time
}

func mrkdwn(t string) Block { return Block{"type": "mrkdwn", "text": t} }
func plain(t string) Block  { return Block{"type": "plain_text", "text": trimTo(t, 75), "emoji": true} }

// section keeps line breaks, unlike trimTo, and stays under Slack's limit.
func section(t string) Block {
	if r := []rune(t); len(r) > 2900 {
		t = string(r[:2899]) + "…"
	}
	return Block{"type": "section", "text": mrkdwn(t)}
}

func contextBlock(parts ...string) Block {
	var els []Block
	for _, p := range parts {
		if strings.TrimSpace(p) != "" && len(els) < 10 {
			els = append(els, mrkdwn(trimTo(p, 1900)))
		}
	}
	if len(els) == 0 {
		return nil
	}
	return Block{"type": "context", "elements": els}
}

func button(text, actionID, value, style string) Block {
	b := Block{"type": "button", "text": plain(text), "action_id": actionID, "value": value}
	if style != "" {
		b["style"] = style
	}
	return b
}

func actions(els ...Block) Block {
	var keep []Block
	for _, e := range els {
		if e != nil && len(keep) < 25 {
			keep = append(keep, e)
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return Block{"type": "actions", "elements": keep}
}

func compact(blocks ...Block) []Block {
	var out []Block
	for _, b := range blocks {
		if b != nil {
			out = append(out, b)
		}
	}
	return out
}

func appLink(v View) string { return linkedApp(v.Cluster, v.App) }

func linkedApp(cl Cluster, app string) string {
	if u := cl.AppURL(app); u != "" {
		return fmt.Sprintf("*<%s|%s>*", u, app)
	}
	return "*" + app + "*"
}

func value(parts ...string) string { return strings.Join(parts, "|") }

func moreMenu(v View, kind string) Block {
	var opts []Block
	if u := v.Cluster.AppURL(v.App); u != "" {
		opts = append(opts, Block{"text": plain("Open in Argo CD"), "value": value("open", v.Cluster.Name, v.App), "url": u})
	}
	opts = append(opts, Block{"text": plain("Mute…"), "value": value("mute", v.Cluster.Name, v.App, kind)})
	return Block{"type": "overflow", "action_id": "more", "options": opts}
}

func headline(title string, accessory Block) Block {
	b := section(title)
	if accessory != nil {
		b["accessory"] = accessory
	}
	return b
}

// dot joins the non-empty parts with a middle dot.
func dot(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " · ")
}

// linkText is the app's links as quiet text rather than buttons.
func linkText(v View, runURL string) string {
	var out []string
	if u := v.Cluster.AppURL(v.App); u != "" {
		out = append(out, "<"+u+"|Argo CD>")
	}
	for _, l := range v.Cfg.ResolvedLinks(v.Cluster, v.App, v.Namespace) {
		out = append(out, "<"+l.URL+"|"+escape(l.Text)+">")
	}
	if runURL != "" {
		out = append(out, "<"+runURL+"|CI run>")
	}
	return strings.Join(out, " · ")
}

// message is a plain-text notification line plus the blocks people see. A
// mention goes in both, so it notifies and shows.
func message(title, mention string, blocks []Block, meta *Metadata) Payload {
	text := plainTitle(title)
	if mention != "" {
		text = mention + " " + text
		if t, ok := blocks[0]["text"].(Block); ok {
			t["text"] = mention + " " + t["text"].(string)
		}
	}
	return Payload{Text: text, Blocks: compact(blocks...), Metadata: meta}
}

// RenderDeploy draws one deploy message. A healthy deploy is three short
// lines; trouble adds what went wrong and why.
func RenderDeploy(v View, d *Deploy) Payload {
	cl, app := v.Cluster, appLink(v)
	var title string
	switch d.State {
	case DeploySyncing:
		title = fmt.Sprintf("%s is syncing to %s", app, cl.Name)
		if d.Attempts > 1 {
			title += fmt.Sprintf(", attempt %d", d.Attempts)
		}
	case DeployRolling:
		title = fmt.Sprintf("%s is rolling out on %s", app, cl.Name)
	case DeployStalled:
		title = fmt.Sprintf("%s is still rolling out on %s after %s", app, cl.Name, short(v.Now.Sub(d.StartedAt)))
	case DeployHealthy:
		title = fmt.Sprintf("%s deployed to %s in %s", app, cl.Name, short(d.FinishedAt.Sub(d.StartedAt)))
	case DeployFailed:
		title = fmt.Sprintf("%s failed to deploy to %s", app, cl.Name)
	case DeployUnhealthy:
		title = fmt.Sprintf("%s is %s after deploying to %s", app, strings.ToLower(orDefault(d.Reason.healthWord(), "unhealthy")), cl.Name)
	case DeployRecovered:
		title = fmt.Sprintf("%s recovered on %s, %s after the deploy", app, cl.Name, short(d.FinishedAt.Sub(d.StartedAt)))
	case DeploySuperseded:
		title = fmt.Sprintf("%s's deploy to %s was replaced by a newer sync", app, cl.Name)
	}
	kind := KindDeploys
	if d.State == DeployFailed || d.State == DeployUnhealthy {
		kind = KindFailed
	}
	// what went wrong reads as the headline's second line
	lines := []string{title}
	if d.State == DeployFailed {
		for i, f := range d.Failures {
			if i == 3 {
				lines = append(lines, fmt.Sprintf("and %d more", len(d.Failures)-3))
				break
			}
			lines = append(lines, escape(stripMrkdwn(f)))
		}
		if len(d.Failures) == 0 || d.Baseline {
			lines = append(lines[:1], escape(orDefault(firstLine(d.OpMessage), "The sync failed.")))
		}
	} else if d.State == DeployUnhealthy && d.Reason != nil && d.Reason.Summary != "" {
		lines = append(lines, escape(d.Reason.Summary))
	}
	blocks := []Block{headline(strings.Join(lines, "\n"), moreMenu(v, kind))}
	switch d.State {
	case DeploySyncing, DeployRolling, DeployStalled, DeployUnhealthy:
		blocks = append(blocks, deploySteps(v, d)...)
	}
	if r := d.Reason; r != nil && (d.State == DeployFailed || d.State == DeployUnhealthy) {
		blocks = append(blocks, reasonBlocks(v, r)...)
	}

	if line := commitLine(v.Cfg, d); line != "" {
		blocks = append(blocks, contextBlock(line))
	}
	meta := []string{whoText(v.Cfg, d)}
	if d.State == DeployFailed && !d.Baseline && len(d.Failures) > 0 {
		meta = append(meta, fmt.Sprintf("%d attempt%s, nothing new rolled out", d.Attempts, plural(d.Attempts)))
	}
	meta = append(meta, d.FixedBy)
	if v.Mute != nil {
		meta = append(meta, v.Mute.Note())
	}
	meta = append(meta, linkText(v, d.RunURL))
	blocks = append(blocks, contextBlock(dot(meta...)))
	return message(title, d.Mention, blocks,
		&Metadata{EventType: "argocd_deploy", EventPayload: map[string]any{"id": d.ID, "cluster": cl.Name, "app": v.App, "state": string(d.State)}})
}

func (r *Reason) healthWord() string {
	if r == nil {
		return ""
	}
	if r.Category == CatMissing {
		return "missing"
	}
	return "degraded"
}

func commitLine(cfg Config, d *Deploy) string {
	if d.Title == "" {
		return ""
	}
	t := trimTo(d.Title, 200)
	if u := cfg.PRURL(d.Repo, d.PR); u != "" {
		return fmt.Sprintf("<%s|%s #%s>", u, escape(t), d.PR)
	}
	if u := cfg.CommitURL(d.Repo, d.Commit); u != "" {
		return fmt.Sprintf("<%s|%s>", u, escape(t))
	}
	if d.PR != "" {
		return escape(t) + " #" + d.PR
	}
	return escape(t)
}

// whoText is the short commit and who shipped it, e.g. "4be81c0 by mara-lee".
func whoText(cfg Config, d *Deploy) string {
	sha := ""
	if c := d.Commit; c != "" {
		sha = shortSHA(c)
		if u := cfg.CommitURL(d.Repo, c); u != "" {
			sha = "<" + u + "|" + sha + ">"
		}
	}
	who := ""
	switch {
	case d.Actor != "":
		who = "by " + d.Actor
	case !d.Automated && d.Username != "":
		who = "synced by " + d.Username
	}
	return strings.TrimSpace(sha + " " + who)
}

func deploySteps(v View, d *Deploy) []Block {
	type task struct{ id, title, status, detail string }
	var tasks []task
	syncStatus := "in_progress"
	if !d.SyncedAt.IsZero() {
		syncStatus = "complete"
	}
	detail := ""
	if !d.SyncedAt.IsZero() {
		detail = short(d.SyncedAt.Sub(d.StartedAt))
	} else if d.Attempts > 1 {
		detail = fmt.Sprintf("attempt %d", d.Attempts)
	}
	tasks = append(tasks, task{"sync", "Sync manifests", syncStatus, detail})
	if !d.SyncedAt.IsZero() {
		roll := task{"rollout", "Roll out", "in_progress", ""}
		if d.Progress != "" {
			roll.title = "Roll out " + d.Progress + " pods"
		}
		if d.State == DeployUnhealthy {
			roll.status = "error"
		} else {
			roll.detail = short(v.Now.Sub(d.SyncedAt)) + " so far"
		}
		tasks = append(tasks, roll)
	}
	if v.Rich {
		var ts []Block
		for _, t := range tasks {
			tb := Block{"type": "task_card", "task_id": t.id, "title": t.title, "status": t.status}
			if t.detail != "" {
				tb["details"] = Block{"type": "rich_text", "elements": []Block{{
					"type": "rich_text_section", "elements": []Block{{"type": "text", "text": t.detail}},
				}}}
			}
			ts = append(ts, tb)
		}
		return []Block{{"type": "plan", "block_id": fmt.Sprintf("plan-%d", v.Now.UnixNano()), "title": "Deploy", "tasks": ts}}
	}
	var parts []string
	for _, t := range tasks {
		p := t.title
		switch t.status {
		case "complete":
			p += " done"
		case "error":
			p += " failed"
		}
		if t.detail != "" {
			p += ", " + t.detail
		}
		parts = append(parts, p)
	}
	return []Block{contextBlock(dot(parts...))}
}

// reasonBlocks backs up the headline's reason with one log line and folds
// the rest away.
func reasonBlocks(v View, r *Reason) []Block {
	var out []Block
	if r.Excerpt != "" {
		out = append(out, section("```"+strings.ReplaceAll(r.Excerpt, "```", "'''")+"```"))
	}
	var detail []string
	if r.Resource != "" {
		detail = append(detail, r.Resource)
	}
	if r.Ready != "" {
		detail = append(detail, r.Ready+" ready")
	}
	if r.Pods > 0 {
		detail = append(detail, fmt.Sprintf("%d pod%s", r.Pods, plural(r.Pods)))
	}
	detail = append(detail, r.Details...)
	if r.Image != "" {
		detail = append(detail, r.Image)
	}
	if len(detail) == 0 {
		return out
	}
	if v.Rich {
		return append(out, Block{
			"type":              "container",
			"title":             plain("Details"),
			"subtitle":          mrkdwn(escape(trimTo(dot(detail...), 140))),
			"is_collapsible":    true,
			"default_collapsed": true,
			"child_blocks":      []Block{contextBlock(escape(strings.Join(detail, "\n")))},
		})
	}
	return append(out, contextBlock(escape(dot(detail...))))
}

// RenderIncident draws one incident message.
func RenderIncident(v View, inc *Incident) Payload {
	cl, app := v.Cluster, appLink(v)
	open := inc.State == IncidentOpen
	var title string
	switch inc.State {
	case IncidentResolved:
		title = fmt.Sprintf("%s recovered on %s after %s", app, cl.Name, short(inc.Downtime(v.Now)))
	case IncidentClosed:
		title = fmt.Sprintf("%s was removed from Argo CD on %s", app, cl.Name)
	default:
		word := "degraded"
		switch inc.Health {
		case "Missing":
			word = "missing resources"
		case "Unknown":
			word = "in an unknown state"
		}
		title = fmt.Sprintf("%s is %s on %s", app, word, cl.Name)
	}
	head := title
	if r := inc.Reason; open && r != nil && r.Summary != "" {
		head += "\n" + escape(r.Summary)
	}
	blocks := []Block{section(head)}
	var ctx []string
	if open {
		if r := inc.Reason; r != nil {
			blocks = append(blocks, reasonBlocks(v, r)...)
		}
		var btns []Block
		if inc.AckBy == "" {
			btns = append(btns, button("I'm on it", "ack", value(cl.Name, v.App, inc.ID), "primary"))
		}
		btns = append(btns, button("Mute…", "mute_open", value(cl.Name, v.App, KindIncidents), ""))
		blocks = append(blocks, actions(btns...))
		ctx = append(ctx, "Since "+slackTime(inc.FirstUnhealthyAt, "{date_short_pretty} {time}"))
	} else {
		if r := inc.Reason; r != nil && r.Summary != "" {
			was, _, _ := strings.Cut(r.Summary, " · ")
			ctx = append(ctx, "Was "+escape(was))
		}
		ctx = append(ctx, slackTime(inc.FirstUnhealthyAt, "{time}")+" to "+slackTime(inc.ResolvedAt, "{time}"))
	}
	if inc.Flaps > 0 {
		ctx = append(ctx, fmt.Sprintf("flapped %d×", inc.Flaps))
	}
	if open && inc.AckBy != "" {
		ctx = append(ctx, fmt.Sprintf("<@%s> is on it", inc.AckBy))
	}
	if open && v.Mute != nil {
		ctx = append(ctx, v.Mute.Note())
	}
	ctx = append(ctx, linkText(v, ""))
	blocks = append(blocks, contextBlock(dot(ctx...)))
	return message(title, inc.Mention, blocks,
		&Metadata{EventType: "argocd_incident", EventPayload: map[string]any{"id": inc.ID, "cluster": cl.Name, "app": v.App, "state": string(inc.State)}})
}

// RenderBaseline draws the cut-over summary of already-failing apps. Open
// rows get a line each; recovered ones shrink to a grey note.
func RenderBaseline(cl Cluster, total int, rows []BaselineRow) Payload {
	var open, done []BaselineRow
	for _, r := range rows {
		if r.Resolved {
			done = append(done, r)
		} else {
			open = append(open, r)
		}
	}
	apps := fmt.Sprintf("%d app%s", total, plural(total))
	var title string
	switch {
	case len(rows) == 0:
		title = fmt.Sprintf("Now watching *%s* · all %s are healthy", cl.Name, apps)
	case len(open) == 0:
		title = fmt.Sprintf("Now watching *%s* · all %s are healthy now", cl.Name, apps)
	default:
		verb := map[bool]string{true: "is", false: "are"}[len(open) == 1]
		if len(done) > 0 {
			verb += " still"
		}
		title = fmt.Sprintf("Now watching *%s* · %d of %s %s unhealthy", cl.Name, len(open), apps, verb)
	}

	blocks := []Block{section(title)}
	for i, r := range open {
		if i == 30 {
			blocks = append(blocks, contextBlock(fmt.Sprintf("and %d more", len(open)-30)))
			break
		}
		line := linkedApp(cl, r.App) + "  " + escape(trimTo(r.Summary, 200))
		if !r.Since.IsZero() {
			line += " · since " + slackTime(r.Since, "{date_short_pretty} {time}")
		}
		if r.Detail != "" {
			line += "\n`" + strings.ReplaceAll(trimTo(r.Detail, 300), "`", "'") + "`"
		}
		b := section(line)
		opts := []Block{{"text": plain("Mute…"), "value": value("mute", cl.Name, r.App, KindIncidents)}}
		if u := cl.AppURL(r.App); u != "" {
			opts = append([]Block{{"text": plain("Open in Argo CD"), "value": value("open", cl.Name, r.App), "url": u}}, opts...)
		}
		b["accessory"] = Block{"type": "overflow", "action_id": "more", "options": opts}
		blocks = append(blocks, b)
	}
	var notes []string
	for _, r := range done {
		n := r.App + " recovered"
		if r.Kind == "deploy" {
			n = r.App + " redeployed"
		}
		if !r.ResolvedAt.IsZero() {
			n += " at " + slackTime(r.ResolvedAt, "{time}")
			if !r.Since.IsZero() && r.ResolvedAt.After(r.Since) {
				n += " after " + short(r.ResolvedAt.Sub(r.Since))
			}
		}
		notes = append(notes, n)
	}
	for len(notes) > 0 && len(blocks) < 47 {
		n := min(len(notes), 10)
		blocks = append(blocks, contextBlock(notes[:n]...))
		notes = notes[n:]
	}
	if len(open) > 0 {
		blocks = append(blocks, contextBlock("Each line updates when its app recovers. New problems get their own message."))
	}
	return message(title, "", blocks, &Metadata{EventType: "argocd_baseline", EventPayload: map[string]any{"cluster": cl.Name}})
}

// DigestRow is one long-running problem in the daily summary.
type DigestRow struct {
	App, Clusters, For, Reason, Note string
	URL                              string
}

// RenderDigest draws the weekday summary for one channel.
func RenderDigest(title string, rows []DigestRow, stats Stats, rich bool) Payload {
	blocks := []Block{section("*" + title + "*\nUnhealthy for more than a day")}
	if rich {
		header := []Block{rawCell("App"), rawCell("Cluster"), rawCell("For"), rawCell("Reason"), rawCell("")}
		table := [][]Block{header}
		for _, r := range rows {
			table = append(table, []Block{rawCell(r.App), rawCell(r.Clusters), rawCell(r.For), rawCell(r.Reason), rawCell(r.Note)})
		}
		blocks = append(blocks, Block{"type": "table", "rows": table})
	} else {
		var lines []string
		for _, r := range rows {
			name := "*" + r.App + "*"
			if r.URL != "" {
				name = fmt.Sprintf("*<%s|%s>*", r.URL, r.App)
			}
			lines = append(lines, dot(name, r.Clusters, r.For, escape(r.Reason), r.Note))
		}
		blocks = append(blocks, section(strings.Join(lines, "\n")))
	}
	blocks = append(blocks, contextBlock(fmt.Sprintf("Last 24 hours: %d deploys, %d failed, %d incidents resolved, %d blips ignored",
		stats.Deploys, stats.Failed, stats.Resolved, stats.Blips)))
	return Payload{Text: title, Blocks: blocks, Metadata: &Metadata{EventType: "argocd_digest", EventPayload: map[string]any{"title": title}}}
}

func rawCell(t string) Block { return Block{"type": "raw_text", "text": orDefault(t, " ")} }

// HomeCluster is one cluster row on App Home.
type HomeCluster struct {
	Name                      string
	Healthy, Unhealthy, Muted int
	Enabled                   bool
	LastPoll                  time.Time
}

// RenderHome draws a user's App Home tab.
func RenderHome(clusters []HomeCluster, mutes []Mute, now time.Time) map[string]any {
	blocks := []Block{
		{"type": "header", "text": plain("Argo CD")},
		section("Deploys and incidents from Argo CD, one message each. Mute anything here or with `/argocd mute`."),
		{"type": "divider"},
		section("*Clusters*"),
	}
	for _, c := range clusters {
		status := fmt.Sprintf("%d healthy · %d unhealthy", c.Healthy, c.Unhealthy)
		if c.Muted > 0 {
			status += fmt.Sprintf(" · %d muted", c.Muted)
		}
		if !c.Enabled {
			status = "not watched yet"
		} else if !c.LastPoll.IsZero() && now.Sub(c.LastPoll) > 5*time.Minute {
			status += " · not read since " + slackTime(c.LastPoll, "{time}")
		}
		b := section(fmt.Sprintf("*%s*\n%s", c.Name, status))
		b["accessory"] = button("Mute…", "mute_open", value(c.Name, "", KindIncidents), "")
		blocks = append(blocks, b)
	}
	blocks = append(blocks, Block{"type": "divider"}, section("*Active mutes*"))
	active := 0
	for _, m := range mutes {
		if !m.Active(now) {
			continue
		}
		active++
		line := fmt.Sprintf("*%s* · %s · %s\n<@%s>", m.Scope(), strings.Join(m.Kinds, ", "), m.UntilText(), m.By)
		if m.Reason != "" {
			line += " · " + escape(m.Reason)
		}
		b := section(line)
		b["accessory"] = button("Unmute", "unmute", m.ID, "")
		blocks = append(blocks, b)
		if len(blocks) > 95 {
			break
		}
	}
	if active == 0 {
		blocks = append(blocks, contextBlock("Nothing is muted."))
	}
	return map[string]any{"type": "home", "blocks": blocks}
}

// MuteContext prefills the mute modal.
type MuteContext struct {
	Cluster string `json:"cluster,omitempty"`
	App     string `json:"app,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Channel string `json:"channel,omitempty"`
}

// RenderMuteModal draws the mute form.
func RenderMuteModal(cfg Config, mc MuteContext) map[string]any {
	type opt struct{ text, value string }
	var scopes []opt
	cl, haveCluster := cfg.ClusterByName(mc.Cluster)
	if mc.App != "" && haveCluster {
		scopes = append(scopes, opt{mc.App + " on " + cl.Name, "app@" + mc.App + "@" + cl.Name})
	}
	if mc.App != "" {
		scopes = append(scopes, opt{mc.App + " everywhere", "app@" + mc.App + "@"})
	}
	if haveCluster {
		scopes = append(scopes, opt{"All of " + cl.Name, "cluster@" + cl.Name})
		scopes = append(scopes, opt{"All of " + cl.Env, "env@" + cl.Env})
	}
	if len(scopes) == 0 {
		for _, c := range cfg.Clusters {
			scopes = append(scopes, opt{"All of " + c.Name, "cluster@" + c.Name})
		}
	}
	options := func(os []opt) []Block {
		var out []Block
		for _, o := range os {
			out = append(out, Block{"text": plain(o.text), "value": o.value})
		}
		return out
	}
	scopeOpts := options(scopes)
	durations := options([]opt{{"1 hour", "1h"}, {"4 hours", "4h"}, {"1 day", "1d"}, {"1 week", "1w"}, {"Until it's healthy", "healthy"}, {"Until the time below", "custom"}})
	initialDur := durations[1]
	if mc.App != "" && mc.Kind == KindIncidents {
		initialDur = durations[4]
	}
	kinds := options([]opt{{"Incidents", KindIncidents}, {"Successful deploys", KindDeploys}, {"Failed deploys", KindFailed}})
	initialKinds := []Block{kinds[0]}
	switch mc.Kind {
	case KindDeploys:
		initialKinds = []Block{kinds[1]}
	case KindFailed:
		initialKinds = []Block{kinds[2]}
	}
	meta, _ := json.Marshal(mc)
	return map[string]any{
		"type":             "modal",
		"callback_id":      "mute_submit",
		"private_metadata": string(meta),
		"title":            plain("Mute notifications"),
		"submit":           plain("Mute"),
		"close":            plain("Cancel"),
		"blocks": []Block{
			{"type": "input", "block_id": "scope", "label": plain("What"), "element": Block{
				"type": "radio_buttons", "action_id": "v", "options": scopeOpts, "initial_option": scopeOpts[0]}},
			{"type": "input", "block_id": "duration", "label": plain("For"), "element": Block{
				"type": "static_select", "action_id": "v", "options": durations, "initial_option": initialDur}},
			{"type": "input", "block_id": "until", "optional": true, "label": plain("End time (for “until the time below”)"), "element": Block{
				"type": "datetimepicker", "action_id": "v"}},
			{"type": "input", "block_id": "kinds", "label": plain("Mute"), "element": Block{
				"type": "checkboxes", "action_id": "v", "options": kinds, "initial_options": initialKinds}},
			{"type": "input", "block_id": "reason", "optional": true, "label": plain("Why (shown on the message)"), "element": Block{
				"type": "plain_text_input", "action_id": "v", "max_length": 140,
				"placeholder": plain("e.g. waiting on a database fix")}},
		},
	}
}

// plainTitle drops mrkdwn links and emphasis for notification text.
func plainTitle(s string) string {
	for {
		i := strings.Index(s, "<")
		j := strings.Index(s, ">")
		if i < 0 || j < i {
			break
		}
		inner := s[i+1 : j]
		if k := strings.LastIndex(inner, "|"); k >= 0 {
			inner = inner[k+1:]
		}
		s = s[:i] + inner + s[j+1:]
	}
	return strings.NewReplacer("*", "", "~", "", "`", "").Replace(s)
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
