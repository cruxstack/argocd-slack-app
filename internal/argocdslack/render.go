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

const (
	colorOK   = "#2EB67D"
	colorBad  = "#E01E5A"
	colorRun  = "#36C5F0"
	colorWarn = "#ECB22E"
	colorMute = "#9A9B9E"
)

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
func section(t string) Block {
	return Block{"type": "section", "text": mrkdwn(trimTo(t, 2900))}
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

func urlButton(text, url, actionID string) Block {
	if url == "" {
		return nil
	}
	return Block{"type": "button", "text": plain(text), "url": url, "action_id": actionID}
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

// headlined puts the title in the message's own blocks, where Slack shows it
// once, and the details in a colored attachment below it. With attachments
// alone Slack shows the text above them, which repeated the title.
func headlined(text, title, color string, details []Block, meta *Metadata) Payload {
	p := Payload{Text: text, Blocks: []Block{section(title)}, Metadata: meta}
	if details = compact(details...); len(details) > 0 {
		p.Attachments = []Attachment{{Color: color, Fallback: text, Blocks: details}}
	}
	return p
}

func value(parts ...string) string { return strings.Join(parts, "|") }

func envLine(cl Cluster) string { return cl.Env + " · " + shortRegion(cl.Region) }

// shortRegion turns us-east-1 into use1 and eu-central-1 into euc1.
func shortRegion(region string) string {
	parts := strings.Split(region, "-")
	if len(parts) != 3 {
		return region
	}
	dir := map[string]string{"east": "e", "west": "w", "north": "n", "south": "s", "central": "c",
		"northeast": "ne", "northwest": "nw", "southeast": "se", "southwest": "sw"}[parts[1]]
	if dir == "" {
		return region
	}
	return parts[0] + dir + parts[2]
}

func moreMenu(v View, kind string, extra ...Block) Block {
	opts := []Block{{"text": plain("Mute…"), "value": value("mute", v.Cluster.Name, v.App, kind)}}
	opts = append(opts, extra...)
	return Block{"type": "overflow", "action_id": "more", "options": opts}
}

// RenderDeploy draws one deploy message.
func RenderDeploy(v View, d *Deploy) Payload {
	cl := v.Cluster
	color, title := colorRun, ""
	switch d.State {
	case DeploySyncing:
		title = fmt.Sprintf("🔄 Syncing %s to %s", appLink(v), cl.Name)
		if d.Attempts > 1 {
			title += fmt.Sprintf(" · attempt %d", d.Attempts)
		}
	case DeployRolling:
		title = fmt.Sprintf("🔄 Rolling out %s to %s", appLink(v), cl.Name)
	case DeployStalled:
		color = colorWarn
		title = fmt.Sprintf("⏳ %s still rolling out on %s · %s", appLink(v), cl.Name, short(v.Now.Sub(d.StartedAt)))
	case DeployHealthy:
		color = colorOK
		title = fmt.Sprintf("✅ %s deployed to %s · %s", appLink(v), cl.Name, short(d.FinishedAt.Sub(d.StartedAt)))
	case DeployFailed:
		color = colorBad
		title = fmt.Sprintf("❌ %s deploy failed on %s", appLink(v), cl.Name)
	case DeployUnhealthy:
		color = colorBad
		title = fmt.Sprintf("🔴 %s is %s after deploying to %s", appLink(v), strings.ToLower(orDefault(d.Reason.healthWord(), "unhealthy")), cl.Name)
	case DeployRecovered:
		color = colorOK
		title = fmt.Sprintf("✅ %s recovered on %s · %s after the deploy", appLink(v), cl.Name, short(d.FinishedAt.Sub(d.StartedAt)))
	case DeploySuperseded:
		color = colorMute
		title = fmt.Sprintf("⏭ %s deploy on %s was superseded by a newer sync", appLink(v), cl.Name)
	}
	if d.Muted || v.Mute != nil {
		color = colorMute
	}

	var blocks []Block
	if line := commitLine(v.Cfg, d); line != "" {
		blocks = append(blocks, section(line))
	}
	if d.State != DeployFailed && d.State != DeploySuperseded {
		blocks = append(blocks, deploySteps(v, d)...)
	}
	if d.State == DeployFailed {
		if d.Baseline || len(d.Failures) == 0 {
			blocks = append(blocks, section(orDefault(firstLine(d.OpMessage), "The sync failed.")))
		} else {
			blocks = append(blocks, section(fmt.Sprintf("The sync failed after %d attempt%s. Nothing new was rolled out.", d.Attempts, plural(d.Attempts))))
		}
		for i, f := range d.Failures {
			if i == 4 {
				blocks = append(blocks, contextBlock(fmt.Sprintf("and %d more", len(d.Failures)-4)))
				break
			}
			blocks = append(blocks, section("> "+f))
		}
	}
	if d.Reason != nil && (d.State == DeployFailed || d.State == DeployUnhealthy) {
		blocks = append(blocks, reasonBlocks(v, d.Reason)...)
	}

	var fields []Block
	if c := d.Commit; c != "" {
		txt := "`" + shortSHA(c) + "`"
		if u := v.Cfg.CommitURL(d.Repo, c); u != "" {
			txt = fmt.Sprintf("<%s|`%s`>", u, shortSHA(c))
		}
		if d.Repo != "" {
			txt += " · " + d.Repo
		}
		fields = append(fields, mrkdwn("*Commit*\n"+txt))
	}
	who := "automated"
	switch {
	case d.Actor != "":
		who = d.Actor
	case !d.Automated && d.Username != "":
		who = d.Username + " (manual sync)"
	}
	fields = append(fields, mrkdwn("*By*\n"+who))
	if img := deployImage(d); img != "" {
		fields = append(fields, mrkdwn("*Image*\n"+img))
	}
	if took := tookText(d, v.Now); took != "" {
		fields = append(fields, mrkdwn("*Took*\n"+took))
	}
	blocks = append(blocks, Block{"type": "section", "fields": fields})

	var btns []Block
	btns = append(btns, urlButton("Argo CD", cl.AppURL(v.App), "open_argocd"))
	btns = append(btns, linkButtons(v)...)
	if d.PR != "" {
		btns = append(btns, urlButton("PR #"+d.PR, v.Cfg.PRURL(d.Repo, d.PR), "open_pr"))
	} else if d.RunURL != "" {
		btns = append(btns, urlButton("CI run", d.RunURL, "open_run"))
	}
	if d.State == DeployFailed || d.State == DeployUnhealthy {
		btns = append(btns, button("Mute…", "mute_open", value(v.Cluster.Name, v.App, KindFailed), ""))
	} else {
		btns = append(btns, moreMenu(v, KindDeploys))
	}
	blocks = append(blocks, actions(btns...))

	ctx := []string{envLine(cl), "Started " + slackTime(d.StartedAt, "{date_short_pretty} {time}")}
	switch {
	case d.State == DeployHealthy:
		ctx = append(ctx, "healthy "+slackTime(d.FinishedAt, "{time}"))
	case d.State == DeployFailed && !d.FinishedAt.IsZero():
		ctx = append(ctx, "failed "+slackTime(d.FinishedAt, "{time}"))
		if d.Health != "" && d.Health != "Healthy" {
			ctx = append(ctx, "app is "+strings.ToLower(d.Health))
		}
	}
	if d.FixedBy != "" {
		ctx = append(ctx, "✅ "+d.FixedBy)
	}
	if v.Mute != nil {
		ctx = append(ctx, v.Mute.Note())
	}
	blocks = append(blocks, contextBlock(strings.Join(ctx, " · ")))

	text := plainTitle(title)
	if d.Mention != "" {
		text, title = d.Mention+" "+text, d.Mention+" "+title
	}
	return headlined(text, title, color, blocks,
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
		return fmt.Sprintf("<%s|%s> #%s", u, escape(t), d.PR)
	}
	if u := cfg.CommitURL(d.Repo, d.Commit); u != "" {
		return fmt.Sprintf("<%s|%s>", u, escape(t))
	}
	if d.PR != "" {
		return escape(t) + " #" + d.PR
	}
	return escape(t)
}

func tookText(d *Deploy, now time.Time) string {
	switch d.State {
	case DeployHealthy, DeployRecovered:
		s := short(d.FinishedAt.Sub(d.StartedAt))
		if !d.SyncedAt.IsZero() && d.State == DeployHealthy {
			s += fmt.Sprintf(" · sync %s, rollout %s", short(d.SyncedAt.Sub(d.StartedAt)), short(d.FinishedAt.Sub(d.SyncedAt)))
		}
		return s
	case DeployFailed:
		if d.FinishedAt.IsZero() {
			return ""
		}
		return fmt.Sprintf("%s · %d attempt%s", short(d.FinishedAt.Sub(d.StartedAt)), d.Attempts, plural(d.Attempts))
	case DeploySuperseded:
		return ""
	default:
		if now.Sub(d.StartedAt) < time.Second {
			return ""
		}
		return short(now.Sub(d.StartedAt)) + " so far"
	}
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
		switch d.State {
		case DeployHealthy, DeployRecovered:
			roll.status, roll.title = "complete", "Roll out"
			roll.detail = short(d.FinishedAt.Sub(d.SyncedAt))
		case DeployUnhealthy:
			roll.status = "error"
		default:
			roll.detail = short(v.Now.Sub(d.SyncedAt)) + " so far"
		}
		tasks = append(tasks, roll)
	}
	if d.State == DeployHealthy || d.State == DeployRecovered {
		tasks = append(tasks, task{"healthy", "Healthy", "complete", ""})
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
		mark := map[string]string{"complete": "✓", "in_progress": "◐", "error": "✗"}[t.status]
		p := mark + " " + t.title
		if t.detail != "" {
			p += " " + t.detail
		}
		parts = append(parts, p)
	}
	return []Block{contextBlock(strings.Join(parts, "  ·  "))}
}

func reasonBlocks(v View, r *Reason) []Block {
	head := []string{}
	if r.Resource != "" {
		kind, rest, _ := strings.Cut(r.Resource, " ")
		head = append(head, fmt.Sprintf("*%s* `%s`", kind, rest))
	}
	if r.Ready != "" {
		head = append(head, r.Ready+" ready")
	}
	if r.Summary != "" {
		head = append(head, escape(r.Summary))
	}
	out := []Block{section(strings.Join(head, " · "))}
	if r.Excerpt != "" {
		out = append(out, section("```"+strings.ReplaceAll(r.Excerpt, "```", "'''")+"```"))
	}
	var detail []string
	detail = append(detail, r.Details...)
	if r.Image != "" {
		detail = append(detail, "image `"+r.Image+"`")
	}
	if r.Pods > 0 {
		detail = append([]string{fmt.Sprintf("%d pod%s", r.Pods, plural(r.Pods))}, detail...)
	}
	if len(detail) == 0 {
		return out
	}
	if v.Rich {
		out = append(out, Block{
			"type":              "container",
			"title":             plain("Details"),
			"subtitle":          mrkdwn(trimTo(strings.Join(detail, " · "), 140)),
			"is_collapsible":    true,
			"default_collapsed": true,
			"child_blocks":      []Block{contextBlock(detail...)},
		})
		return out
	}
	return append(out, contextBlock(strings.Join(detail, " · ")))
}

// RenderIncident draws one incident message.
func RenderIncident(v View, inc *Incident) Payload {
	cl := v.Cluster
	color := colorBad
	var title string
	switch inc.State {
	case IncidentResolved:
		color = colorOK
		title = fmt.Sprintf("✅ %s recovered on %s · down %s", appLink(v), cl.Name, short(inc.Downtime(v.Now)))
	case IncidentClosed:
		color = colorMute
		title = fmt.Sprintf("⚪ %s was removed from Argo CD on %s", appLink(v), cl.Name)
	default:
		word := "degraded"
		switch inc.Health {
		case "Missing":
			word = "missing resources"
		case "Unknown":
			word = "in an unknown state"
		}
		title = fmt.Sprintf("🔴 %s is %s on %s · %s", appLink(v), word, cl.Name, short(inc.Downtime(v.Now)))
		if inc.Flaps > 0 {
			title += fmt.Sprintf(" · flapped %d×", inc.Flaps)
		}
	}
	if v.Mute != nil && inc.State == IncidentOpen {
		color = colorMute
	}
	var blocks []Block
	if r := inc.Reason; r != nil {
		if inc.State == IncidentOpen {
			blocks = append(blocks, reasonBlocks(v, r)...)
		} else if r.Summary != "" {
			blocks = append(blocks, contextBlock("Was "+escape(r.Summary)))
		}
	}
	btns := append([]Block{urlButton("Argo CD", cl.AppURL(v.App), "open_argocd")}, linkButtons(v)...)
	if inc.State == IncidentOpen {
		if inc.AckBy == "" {
			btns = append(btns, button("I'm on it", "ack", value(cl.Name, v.App, inc.ID), "primary"))
		}
		btns = append(btns, button("Mute…", "mute_open", value(cl.Name, v.App, KindIncidents), ""))
	}
	blocks = append(blocks, actions(btns...))
	var ctx []string
	ctx = append(ctx, envLine(cl))
	switch inc.State {
	case IncidentOpen:
		ctx = append(ctx, "Since "+slackTime(inc.FirstUnhealthyAt, "{date_short_pretty} {time}"))
	default:
		ctx = append(ctx, fmt.Sprintf("Unhealthy %s → %s %s", slackTime(inc.FirstUnhealthyAt, "{date_short_pretty} {time}"),
			map[IncidentState]string{IncidentResolved: "healthy", IncidentClosed: "removed"}[inc.State], slackTime(inc.ResolvedAt, "{time}")))
	}
	if inc.AckBy != "" && inc.State == IncidentOpen {
		ctx = append(ctx, fmt.Sprintf("👀 <@%s> is on it", inc.AckBy))
	}
	if v.Mute != nil && inc.State == IncidentOpen {
		ctx = append(ctx, v.Mute.Note())
	}
	blocks = append(blocks, contextBlock(strings.Join(ctx, " · ")))
	text := plainTitle(title)
	if inc.Mention != "" {
		text, title = inc.Mention+" "+text, inc.Mention+" "+title
	}
	return headlined(text, title, color, blocks,
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
		title = fmt.Sprintf("👋 Now watching *%s* · all %s are healthy", cl.Name, apps)
	case len(open) == 0:
		title = fmt.Sprintf("👋 Now watching *%s* · all %s are healthy now", cl.Name, apps)
	default:
		verb := map[bool]string{true: "is", false: "are"}[len(open) == 1]
		still := "already"
		if len(done) > 0 {
			still = "still"
		}
		title = fmt.Sprintf("👋 Now watching *%s* · %d of %d apps %s %s unhealthy", cl.Name, len(open), total, verb, still)
	}

	var blocks []Block
	for i, r := range open {
		if i == 30 {
			blocks = append(blocks, contextBlock(fmt.Sprintf("and %d more", len(open)-30)))
			break
		}
		icon, summary := "🔴", r.Summary
		if r.Kind == "deploy" {
			icon = "❌"
		}
		line := icon + " " + linkedApp(cl, r.App) + "  " + escape(trimTo(summary, 200))
		if !r.Since.IsZero() {
			line += " · since " + slackTime(r.Since, "{date_short_pretty} {time}")
		}
		if r.Detail != "" {
			line += "\n`" + strings.ReplaceAll(trimTo(r.Detail, 300), "`", "'") + "`"
		}
		b := Block{"type": "section", "text": mrkdwn(line)}
		opts := []Block{{"text": plain("Mute…"), "value": value("mute", cl.Name, r.App, KindIncidents)}}
		if u := cl.AppURL(r.App); u != "" {
			opts = append([]Block{{"text": plain("Open in Argo CD"), "value": value("open", cl.Name, r.App), "url": u}}, opts...)
		}
		b["accessory"] = Block{"type": "overflow", "action_id": "more", "options": opts}
		blocks = append(blocks, b)
	}
	var notes []string
	for _, r := range done {
		n := "✅ *" + r.App + "* recovered"
		if r.Kind == "deploy" {
			n = "✅ *" + r.App + "* redeployed"
		}
		if !r.ResolvedAt.IsZero() {
			n += " " + slackTime(r.ResolvedAt, "{time}")
			if !r.Since.IsZero() && r.ResolvedAt.After(r.Since) {
				n += " after " + short(r.ResolvedAt.Sub(r.Since))
			}
		}
		notes = append(notes, n)
	}
	for len(notes) > 0 && len(blocks) < 46 {
		n := min(len(notes), 10)
		blocks = append(blocks, contextBlock(notes[:n]...))
		notes = notes[n:]
	}
	ctx := envLine(cl)
	if len(open) > 0 {
		ctx += " · lines turn ✅ as apps recover; new problems get their own message"
	}
	blocks = append(blocks, contextBlock(ctx))
	color := colorWarn
	if len(open) == 0 {
		color = colorOK
	}
	return headlined(plainTitle(title), title, color, blocks,
		&Metadata{EventType: "argocd_baseline", EventPayload: map[string]any{"cluster": cl.Name}})
}

// DigestRow is one long-running problem in the daily summary.
type DigestRow struct {
	App, Clusters, For, Reason, Note string
	URL                              string
}

// RenderDigest draws the weekday summary for one channel.
func RenderDigest(title string, rows []DigestRow, stats Stats, rich bool) Payload {
	blocks := []Block{section("*" + title + "*"), section("*Unhealthy for more than a day*")}
	if rich {
		header := []Block{rawCell("App"), rawCell("Cluster"), rawCell("For"), rawCell("Reason"), rawCell("")}
		table := [][]Block{header}
		for _, r := range rows {
			table = append(table, []Block{rawCell(r.App), rawCell(r.Clusters), rawCell(r.For), rawCell(r.Reason), rawCell(r.Note)})
		}
		blocks = append(blocks, Block{"type": "table", "rows": table})
	} else {
		for _, r := range rows {
			name := "*" + r.App + "*"
			if r.URL != "" {
				name = fmt.Sprintf("*<%s|%s>*", r.URL, r.App)
			}
			line := fmt.Sprintf("• %s · %s · %s · %s", name, r.Clusters, r.For, escape(r.Reason))
			if r.Note != "" {
				line += " · _" + r.Note + "_"
			}
			blocks = append(blocks, section(line))
		}
	}
	blocks = append(blocks, contextBlock(fmt.Sprintf("Last 24 h: %d deploys · %d failed · %d incidents resolved · %d blips suppressed",
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
			status += " · ⚠️ last read " + slackTime(c.LastPoll, "{time}")
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

func linkButtons(v View) []Block {
	var out []Block
	for i, l := range v.Cfg.ResolvedLinks(v.Cluster, v.App, v.Namespace) {
		out = append(out, urlButton(l.Text, l.URL, fmt.Sprintf("open_link_%d", i)))
	}
	return out
}

// deployImage is the first image Argo CD reports for the app, or the image
// of the failing container.
func deployImage(d *Deploy) string {
	switch {
	case len(d.Images) == 1:
		return "`" + shortImage(d.Images[0]) + "`"
	case len(d.Images) > 1:
		return fmt.Sprintf("`%s` +%d", shortImage(d.Images[0]), len(d.Images)-1)
	case d.Reason != nil && d.Reason.Image != "":
		return "`" + d.Reason.Image + "`"
	}
	return ""
}
