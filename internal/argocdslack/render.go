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

// codeBlock shows text as a preformatted block, line breaks and all.
func codeBlock(t string) Block {
	if r := []rune(t); len(r) > 2400 {
		t = string(r[:2399]) + "…"
	}
	return section("```" + strings.ReplaceAll(escape(t), "```", "'''") + "```")
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

func linkedApp(cl Cluster, app string) string {
	if u := cl.AppURL(app); u != "" {
		return fmt.Sprintf("*<%s|%s>*", u, app)
	}
	return "*" + app + "*"
}

func value(parts ...string) string { return strings.Join(parts, "|") }

// moreMenu is the quiet ⋯ menu beside a headline in basic layouts.
func moreMenu(v View, kind string) Block {
	var opts []Block
	if u := v.Cluster.AppURL(v.App); u != "" {
		opts = append(opts, Block{"text": plain("Open in Argo CD"), "value": value("open", v.Cluster.Name, v.App), "url": u})
	}
	opts = append(opts, Block{"text": plain("Mute…"), "value": value("mute", v.Cluster.Name, v.App, kind)})
	return Block{"type": "overflow", "action_id": "more", "options": opts}
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

// linkText is the app's links as quiet text rather than buttons, starting
// with the deploy's own run when there is one.
func linkText(v View, runURL string) string {
	var out []string
	if runURL != "" {
		out = append(out, "<"+runURL+"|Deployment>")
	}
	if u := v.Cluster.AppURL(v.App); u != "" {
		out = append(out, "<"+u+"|Argo CD>")
	}
	for _, l := range v.Cfg.ResolvedLinks(v.Cluster, v.App, v.Namespace) {
		out = append(out, "<"+l.URL+"|"+escape(l.Text)+">")
	}
	return strings.Join(out, " · ")
}

// Status icons, served from IconBaseURL.
const (
	iconOK    = "ok"
	iconFail  = "fail"
	iconWarn  = "warn"
	iconRun   = "run"
	iconMuted = "muted"
)

// heading is a status line: a bold lead (the app, linked when it can be),
// the version it deploys if known, and the rest in plain words.
type heading struct {
	Lead, LeadURL, Version, VersionURL, Rest string
}

func (h heading) mrkdwn() string {
	var parts []string
	switch {
	case h.Lead != "" && h.LeadURL != "":
		parts = append(parts, fmt.Sprintf("*<%s|%s>*", h.LeadURL, h.Lead))
	case h.Lead != "":
		parts = append(parts, "*"+h.Lead+"*")
	}
	switch {
	case h.Version != "" && h.VersionURL != "":
		parts = append(parts, fmt.Sprintf("<%s|%s>", h.VersionURL, escape(h.Version)))
	case h.Version != "":
		parts = append(parts, escape(h.Version))
	}
	return strings.TrimSpace(strings.Join(append(parts, escape(h.Rest)), " "))
}

func (h heading) plain() string {
	return strings.Join(strings.Fields(h.Lead+" "+h.Version+" "+h.Rest), " ")
}

// mentionElement turns <@U1> or <!subteam^S1> into a rich text element.
func mentionElement(m string) Block {
	switch {
	case strings.HasPrefix(m, "<@"):
		return Block{"type": "user", "user_id": strings.Trim(m, "<@>")}
	case strings.HasPrefix(m, "<!subteam^"):
		return Block{"type": "usergroup", "usergroup_id": strings.TrimSuffix(strings.TrimPrefix(m, "<!subteam^"), ">")}
	}
	return nil
}

func (h heading) richText(mention string) Block {
	var els []Block
	if m := mentionElement(mention); m != nil {
		els = append(els, m, Block{"type": "text", "text": " "})
	}
	if h.Lead != "" {
		lead := Block{"type": "text", "text": h.Lead, "style": Block{"bold": true}}
		if h.LeadURL != "" {
			lead = Block{"type": "link", "url": h.LeadURL, "text": h.Lead, "style": Block{"bold": true}}
		}
		els = append(els, lead)
		if h.Version != "" {
			ver := Block{"type": "text", "text": h.Version}
			if h.VersionURL != "" {
				ver = Block{"type": "link", "url": h.VersionURL, "text": h.Version}
			}
			els = append(els, Block{"type": "text", "text": " "}, ver)
		}
		els = append(els, Block{"type": "text", "text": " " + h.Rest})
	} else {
		els = append(els, Block{"type": "text", "text": h.Rest})
	}
	return Block{"type": "rich_text", "elements": []Block{{"type": "rich_text_section", "elements": els}}}
}

// statusMessage frames a status in a container whose icon carries the color.
// Basic layouts, for clients or workspaces that refuse containers, put the
// heading in a section and the children below it. Slack refuses a container
// without children, so a bare heading uses the basic layout too.
func statusMessage(v View, h heading, icon, mention string, children []Block, accessory Block, meta *Metadata) Payload {
	text := h.plain()
	if mention != "" {
		text = mention + " " + text
	}
	children = compact(children...)
	if v.Rich && len(children) > 0 {
		if len(children) > 10 {
			children = children[:10]
		}
		c := Block{
			"type":            "container",
			"width":           "full",
			"title":           Block{"type": "plain_text", "text": trimTo(h.plain(), 150)},
			"rich_text_title": h.richText(mention),
			"child_blocks":    children,
		}
		if u := v.Cfg.IconURL(icon); u != "" {
			c["icon"] = Block{"type": "image", "image_url": u, "alt_text": iconAlt[icon]}
		}
		return Payload{Text: text, Blocks: []Block{c}, Metadata: meta}
	}
	head := section(strings.TrimSpace(mention + " " + h.mrkdwn()))
	if accessory != nil {
		head["accessory"] = accessory
	}
	return Payload{Text: text, Blocks: append([]Block{head}, children...), Metadata: meta}
}

var iconAlt = map[string]string{iconOK: "healthy", iconFail: "failing", iconWarn: "needs attention", iconRun: "in progress", iconMuted: "muted"}

// RenderDeploy draws one deploy message: a heading with the version, a line
// of what changed, who shipped it and how it went, and a grey line of links.
// Trouble adds what went wrong and why above that line.
func RenderDeploy(v View, d *Deploy) Payload {
	cl := v.Cluster
	h := heading{Lead: v.App, LeadURL: cl.AppURL(v.App), Version: d.Version, VersionURL: v.Cfg.VersionURL(d.Repo, d.Version)}
	icon := iconRun
	switch d.State {
	case DeploySyncing:
		h.Rest = "is syncing to " + cl.Name
		if d.Attempts > 1 {
			h.Rest += fmt.Sprintf(", attempt %d", d.Attempts)
		}
	case DeployRolling:
		h.Rest = "is rolling out on " + cl.Name
	case DeployStalled:
		h.Rest, icon = fmt.Sprintf("is still rolling out on %s after %s", cl.Name, short(v.Now.Sub(d.StartedAt))), iconWarn
	case DeployHealthy:
		h.Rest, icon = "deployed to "+cl.Name, iconOK
	case DeployFailed:
		h.Rest, icon = "failed to deploy to "+cl.Name, iconFail
	case DeployUnhealthy:
		h.Rest, icon = fmt.Sprintf("is %s after deploying to %s", strings.ToLower(orDefault(d.Reason.healthWord(), "unhealthy")), cl.Name), iconFail
	case DeployRecovered:
		h.Rest, icon = fmt.Sprintf("recovered on %s, %s after the deploy", cl.Name, short(d.FinishedAt.Sub(d.StartedAt))), iconOK
	case DeploySuperseded:
		h.Rest, icon = "deploy to "+cl.Name+" was replaced by a newer sync", iconMuted
		if h.Version == "" {
			h.Lead += "'s"
		}
	}
	trouble := d.State == DeployFailed || d.State == DeployUnhealthy
	if v.Mute != nil && trouble {
		icon = iconMuted
	}
	kind := KindDeploys
	if trouble {
		kind = KindFailed
	}

	var children []Block
	switch {
	case d.State == DeployFailed:
		var lines []string
		for i, f := range d.Failures {
			if i == 3 {
				lines = append(lines, fmt.Sprintf("and %d more", len(d.Failures)-3))
				break
			}
			lines = append(lines, escape(stripMrkdwn(f)))
		}
		if len(d.Failures) == 0 || d.Baseline {
			lines = []string{escape(orDefault(firstLine(d.OpMessage), "The sync failed."))}
		}
		children = append(children, section(strings.Join(lines, "\n")))
	case d.State == DeployUnhealthy && d.Reason != nil && d.Reason.Summary != "":
		children = append(children, section(reasonSummary(d.Reason)))
	}
	if trouble && d.Reason != nil {
		children = append(children, reasonBlocks(d.Reason)...)
	}
	children = append(children, deployMiddle(v, d))
	if trouble {
		children = append(children, actions(button("Mute…", "mute_open", value(cl.Name, v.App, KindFailed), "")))
	}
	children = append(children, contextBlock(linkText(v, d.RunURL)))
	return statusMessage(v, h, icon, d.Mention, children, moreMenu(v, kind),
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

// deployMiddle is the line under the heading, in normal text: what changed,
// who shipped it and how it went, e.g. "v1.4.0 → v1.5.0 · started and
// approved by mara-lee · 1m 15s". A single change's title goes on a second
// line; several go in a thread reply instead.
func deployMiddle(v View, d *Deploy) Block {
	parts := []string{changeRange(v.Cfg, d), whoText(d)}
	switch d.State {
	case DeployHealthy:
		parts = append(parts, short(d.FinishedAt.Sub(d.StartedAt)))
	case DeploySyncing, DeployRolling, DeployStalled:
		parts = append(parts, progressText(v, d))
	case DeployFailed:
		if !d.Baseline && len(d.Failures) > 0 {
			parts = append(parts, fmt.Sprintf("%d attempt%s, nothing new rolled out", d.Attempts, plural(d.Attempts)))
		}
	}
	parts = append(parts, d.FixedBy)
	if v.Mute != nil {
		parts = append(parts, lowerFirst(v.Mute.Note()))
	}
	lines := []string{dot(parts...)}
	if len(d.Changes) == 0 {
		lines = append(lines, titleLine(v.Cfg, d))
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if text == "" {
		return nil
	}
	return section(text)
}

// changeRange is the version or commit before this deploy and after it,
// linked to the comparison. Without a previous one it is the short commit.
func changeRange(cfg Config, d *Deploy) string {
	text, u := rangeOf(cfg, d)
	if u != "" {
		return "<" + u + "|" + text + ">"
	}
	return text
}

// rangeOf is changeRange's text and link.
func rangeOf(cfg Config, d *Deploy) (text, link string) {
	from, to, label := d.From.Version, d.Version, escape
	if from == "" || to == "" || from == to {
		from, to, label = d.From.Commit, d.Commit, shortSHA
	}
	if d.From.Repo != "" && d.Repo != "" && d.From.Repo != d.Repo {
		from = ""
	}
	if from == "" || from == to {
		return shortSHA(d.Commit), cfg.CommitURL(d.Repo, d.Commit)
	}
	return label(from) + " → " + label(to), cfg.CompareURL(d.Repo, from, to)
}

// compareURL links what changed in the deploy, or "".
func compareURL(cfg Config, d *Deploy) string {
	if text, u := rangeOf(cfg, d); strings.Contains(text, " → ") {
		return u
	}
	return ""
}

// whoText is who shipped the deploy, e.g. "started and approved by mara-lee".
func whoText(d *Deploy) string {
	switch {
	case d.Actor != "" && d.ApprovedBy != "" && strings.EqualFold(d.Actor, d.ApprovedBy):
		return "started and approved by " + escape(d.Actor)
	case d.Actor != "" && d.ApprovedBy != "":
		return "by " + escape(d.Actor) + ", approved by " + escape(d.ApprovedBy)
	case d.Actor != "":
		return "by " + escape(d.Actor)
	case d.ApprovedBy != "":
		return "approved by " + escape(d.ApprovedBy)
	case !d.Automated && d.Username != "":
		return "synced by " + escape(d.Username)
	}
	return ""
}

// titleLine is the commit or pull request title, e.g. "#482 feat: one-tap
// checkout".
func titleLine(cfg Config, d *Deploy) string {
	if d.Title == "" {
		return ""
	}
	t := escape(trimTo(d.Title, 200))
	if u := cfg.PRURL(d.Repo, d.PR); u != "" {
		return fmt.Sprintf("<%s|#%s> %s", u, d.PR, t)
	}
	if u := cfg.CommitURL(d.Repo, d.Commit); u != "" {
		return fmt.Sprintf("<%s|%s>", u, t)
	}
	if d.PR != "" {
		return "#" + d.PR + " " + t
	}
	return t
}

// maxChanges is how many changes the thread reply lists.
const maxChanges = 10

// RenderChanges draws the thread reply listing a deploy's changes, e.g.
// "6 PRs in this deploy" and a line per pull request.
func RenderChanges(v View, d *Deploy) Payload {
	n := len(d.Changes)
	noun := "change"
	if allPRs(d.Changes) {
		noun = "PR"
	}
	lead := fmt.Sprintf("%d %s%s", n, noun, plural(n))
	text := lead + " in this deploy"
	more := ""
	if n > maxChanges {
		more = fmt.Sprintf("and %d more", n-maxChanges)
	}
	moreURL := compareURL(v.Cfg, d)
	shown := d.Changes[:min(n, maxChanges)]
	if !v.Rich {
		lines := []string{"*" + lead + "* in this deploy"}
		for _, c := range shown {
			id := escape(c.ID)
			if c.URL != "" && id != "" {
				id = "<" + c.URL + "|" + id + ">"
			}
			line := strings.TrimSpace("• *" + id + "* " + escape(c.Title))
			if id == "" {
				line = "• " + escape(c.Title)
			}
			if c.Author != "" {
				line += "  _" + escape(c.Author) + "_"
			}
			lines = append(lines, line)
		}
		if more != "" {
			if moreURL != "" {
				more = "<" + moreURL + "|" + more + ">"
			}
			lines = append(lines, more)
		}
		return Payload{Text: text, Blocks: []Block{section(strings.Join(lines, "\n"))}}
	}
	var items []Block
	for _, c := range shown {
		var els []Block
		switch {
		case c.ID != "" && c.URL != "":
			els = append(els, Block{"type": "link", "url": c.URL, "text": c.ID, "style": Block{"bold": true}}, Block{"type": "text", "text": "  "})
		case c.ID != "":
			els = append(els, Block{"type": "text", "text": c.ID, "style": Block{"bold": true}}, Block{"type": "text", "text": "  "})
		}
		els = append(els, codeSpans(trimTo(c.Title, 200))...)
		if c.Author != "" {
			els = append(els, Block{"type": "text", "text": "  " + c.Author, "style": Block{"italic": true}})
		}
		items = append(items, Block{"type": "rich_text_section", "elements": els})
	}
	elements := []Block{
		{"type": "rich_text_section", "elements": []Block{
			{"type": "text", "text": lead, "style": Block{"bold": true}},
			{"type": "text", "text": " in this deploy\n"}}},
		{"type": "rich_text_list", "style": "bullet", "indent": 0, "elements": items},
	}
	if more != "" {
		m := Block{"type": "text", "text": more}
		if moreURL != "" {
			m = Block{"type": "link", "url": moreURL, "text": more}
		}
		elements = append(elements, Block{"type": "rich_text_section", "elements": []Block{m}})
	}
	return Payload{Text: text, Blocks: []Block{{"type": "rich_text", "elements": elements}}}
}

func allPRs(cs []Change) bool {
	for _, c := range cs {
		if !strings.HasPrefix(c.ID, "#") {
			return false
		}
	}
	return len(cs) > 0
}

// codeSpans turns `code` in a title into rich text code style.
func codeSpans(t string) []Block {
	if strings.Count(t, "`")%2 != 0 {
		return []Block{{"type": "text", "text": t}}
	}
	var out []Block
	for i, part := range strings.Split(t, "`") {
		if part == "" {
			continue
		}
		el := Block{"type": "text", "text": part}
		if i%2 == 1 {
			el["style"] = Block{"code": true}
		}
		out = append(out, el)
	}
	return out
}

// RenderResync is the one thread reply that counts later syncs at the same
// commit, edited as more arrive.
func RenderResync(d *Deploy) string {
	at := slackTime(d.ResyncAt, "{time}")
	if d.Resyncs == 1 {
		return fmt.Sprintf("Resynced at %s with no new commit; healthy.", at)
	}
	return fmt.Sprintf("Resynced %d times with no new commit, last at %s; healthy.", d.Resyncs, at)
}

// progressText is where a running deploy is, e.g. "synced in 25s, rolling
// out 2/3 pods, 1m 05s so far".
func progressText(v View, d *Deploy) string {
	if d.SyncedAt.IsZero() {
		s := "syncing manifests"
		if d.Attempts > 1 {
			s += fmt.Sprintf(", attempt %d", d.Attempts)
		}
		if el := v.Now.Sub(d.StartedAt); el >= time.Second {
			s += ", " + short(el) + " so far"
		}
		return s
	}
	roll := "rolling out"
	if d.Progress != "" {
		roll += " " + d.Progress + " pods"
	}
	if d.State == DeployUnhealthy {
		roll = "rollout failed"
		if d.Progress != "" {
			roll += " at " + d.Progress + " pods"
		}
	} else {
		roll += ", " + short(v.Now.Sub(d.SyncedAt)) + " so far"
	}
	return "synced in " + short(d.SyncedAt.Sub(d.StartedAt)) + ", " + roll
}

// reasonSummary leads with the cause in bold and how many pods are ready,
// e.g. "*CrashLoopBackOff*, 1 of 3 pods ready · 6 restarts · last exit 1".
func reasonSummary(r *Reason) string {
	head, rest, _ := strings.Cut(r.Summary, " · ")
	s := "*" + escape(stripMrkdwn(head)) + "*"
	if ready, total, ok := strings.Cut(r.Ready, "/"); ok {
		s += fmt.Sprintf(", %s of %s pods ready", ready, total)
	}
	if rest != "" {
		s += " · " + escape(rest)
	}
	return s
}

// reasonBlocks backs up the heading's reason with one log line and a grey
// line of details.
func reasonBlocks(r *Reason) []Block {
	var out []Block
	if r.Excerpt != "" {
		out = append(out, section("```"+strings.ReplaceAll(r.Excerpt, "```", "'''")+"```"))
	}
	var detail []string
	if r.Resource != "" {
		detail = append(detail, r.Resource)
	}
	if r.Image != "" {
		detail = append(detail, r.Image)
	}
	detail = append(detail, r.Details...)
	if len(detail) > 0 {
		out = append(out, contextBlock(escape(dot(detail...))))
	}
	return out
}

// RenderIncident draws one incident message.
func RenderIncident(v View, inc *Incident) Payload {
	cl := v.Cluster
	h := heading{Lead: v.App, LeadURL: cl.AppURL(v.App)}
	open := inc.State == IncidentOpen
	icon := iconFail
	switch inc.State {
	case IncidentResolved:
		h.Rest, icon = fmt.Sprintf("recovered on %s after %s", cl.Name, short(inc.Downtime(v.Now))), iconOK
	case IncidentClosed:
		h.Rest, icon = "was removed from Argo CD on "+cl.Name, iconMuted
	default:
		word := "degraded"
		switch inc.Health {
		case "Missing":
			word = "missing resources"
		case "Unknown":
			word, icon = "in an unknown state", iconWarn
		}
		h.Rest = fmt.Sprintf("is %s on %s", word, cl.Name)
		if v.Mute != nil {
			icon = iconMuted
		}
	}
	var children []Block
	var ctx []string
	if open {
		if r := inc.Reason; r != nil {
			if r.Summary != "" {
				children = append(children, section(reasonSummary(r)))
			}
			children = append(children, reasonBlocks(r)...)
		}
		var btns []Block
		if inc.AckBy == "" {
			btns = append(btns, button("I'm on it", "ack", value(cl.Name, v.App, inc.ID), "primary"))
		}
		btns = append(btns, button("Mute…", "mute_open", value(cl.Name, v.App, KindIncidents), ""))
		children = append(children, actions(btns...))
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
	children = append(children, contextBlock(dot(ctx...)))
	return statusMessage(v, h, icon, inc.Mention, children, nil,
		&Metadata{EventType: "argocd_incident", EventPayload: map[string]any{"id": inc.ID, "cluster": cl.Name, "app": v.App, "state": string(inc.State)}})
}

// RenderBaseline draws the cut-over summary of already-failing apps in the
// same frame as other statuses: a line per open app, a grey note for the
// ones that recovered.
func RenderBaseline(cfg Config, cl Cluster, total int, rows []BaselineRow, rich bool) Payload {
	var open, done []BaselineRow
	for _, r := range rows {
		if r.Resolved {
			done = append(done, r)
		} else {
			open = append(open, r)
		}
	}
	apps := fmt.Sprintf("%d app%s", total, plural(total))
	h := heading{Rest: fmt.Sprintf("Now watching %s · ", cl.Name)}
	icon := iconWarn
	switch {
	case len(rows) == 0:
		h.Rest, icon = h.Rest+"all "+apps+" are healthy", iconOK
	case len(open) == 0:
		h.Rest, icon = h.Rest+"all "+apps+" are healthy now", iconOK
	default:
		verb := map[bool]string{true: "is", false: "are"}[len(open) == 1]
		if len(done) > 0 {
			verb += " still"
		}
		h.Rest += fmt.Sprintf("%d of %s %s unhealthy", len(open), apps, verb)
	}

	// a container holds ten blocks: up to seven rows, then a count
	var children []Block
	for i, r := range open {
		if i == 7 {
			children = append(children, contextBlock(fmt.Sprintf("and %d more; see `%s status`", len(open)-7, cfg.Setup.Command)))
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
		children = append(children, b)
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
	if len(notes) > 10 {
		notes = append(notes[:9], fmt.Sprintf("and %d more", len(notes)-9))
	}
	if len(notes) > 0 {
		children = append(children, contextBlock(notes...))
	}
	switch {
	case len(open) > 0:
		children = append(children, contextBlock("Each line updates when its app recovers. New problems get their own message."))
	case len(rows) == 0:
		children = append(children, contextBlock("Deploys and new problems get their own messages."))
	}
	v := View{Cfg: cfg, Cluster: cl, Rich: rich}
	return statusMessage(v, h, icon, "", children, nil, &Metadata{EventType: "argocd_baseline", EventPayload: map[string]any{"cluster": cl.Name}})
}

// RenderUnreachable says a cluster's notifications are paused, with the
// redacted error in full.
func RenderUnreachable(v View, since time.Time, cause string) Payload {
	cl := v.Cluster
	children := []Block{
		section(fmt.Sprintf("Notifications for %s have been paused since %s. They resume on their own once the cluster can be read again.",
			escape(cl.Name), slackTime(since, "{date_short_pretty} {time}"))),
		codeBlock(Redact(cause)),
	}
	return statusMessage(v, heading{Rest: "Can't read Argo CD on " + cl.Name}, iconWarn, "", children, nil,
		&Metadata{EventType: "argocd_cluster", EventPayload: map[string]any{"cluster": cl.Name, "state": "unreachable"}})
}

// RenderReachable replaces the unreachable notice once the cluster is back.
func RenderReachable(v View, since time.Time) Payload {
	cl := v.Cluster
	children := []Block{contextBlock(fmt.Sprintf("Notifications were paused for %s, from %s.", short(v.Now.Sub(since)), slackTime(since, "{date_short_pretty} {time}")))}
	return statusMessage(v, heading{Rest: "Reading Argo CD on " + cl.Name + " again"}, iconOK, "", children, nil,
		&Metadata{EventType: "argocd_cluster", EventPayload: map[string]any{"cluster": cl.Name, "state": "reachable"}})
}

// DigestRow is one long-running problem in the daily summary.
type DigestRow struct {
	App, Clusters, For, Reason, Note string
	URL                              string
	Age                              time.Duration
}

// RenderDigest draws the weekday summary for one channel as a sortable table
// with a mute button on each row.
func RenderDigest(title string, rows []DigestRow, stats Stats, rich bool) Payload {
	blocks := []Block{section("*" + title + "*")}
	footer := contextBlock(fmt.Sprintf("Last 24 hours: %d deploys, %d failed, %d incidents resolved, %d blips ignored",
		stats.Deploys, stats.Failed, stats.Resolved, stats.Blips))
	if rich {
		table := [][]Block{{rawCell("App"), rawCell("Cluster"), rawCell("For"), rawCell("Reason"), rawCell(" ")}}
		for _, r := range rows {
			app := rawCell(r.App)
			if r.URL != "" {
				app = Block{"type": "rich_text", "elements": []Block{{"type": "rich_text_section", "elements": []Block{
					{"type": "link", "url": r.URL, "text": r.App},
				}}}}
			}
			reason := r.Reason
			if r.Note != "" {
				reason += " (" + r.Note + ")"
			}
			mute := Block{"type": "action_cell", "fallback": rawCell("-"),
				"element": button("Mute…", "mute_open", value("", r.App, KindIncidents), "")}
			table = append(table, []Block{app, rawCell(r.Clusters),
				{"type": "raw_number", "value": int(r.Age.Hours()), "text": orDefault(r.For, " ")}, rawCell(trimTo(reason, 300)), mute})
		}
		blocks = append(blocks, Block{"type": "data_table", "caption": "Unhealthy for more than a day", "page_size": 10, "rows": table}, footer)
		return Payload{Text: title, Blocks: blocks, Metadata: &Metadata{EventType: "argocd_digest", EventPayload: map[string]any{"title": title}}}
	}
	var lines []string
	for _, r := range rows {
		name := "*" + r.App + "*"
		if r.URL != "" {
			name = fmt.Sprintf("*<%s|%s>*", r.URL, r.App)
		}
		lines = append(lines, dot(name, r.Clusters, r.For, escape(r.Reason), r.Note))
	}
	blocks = append(blocks, section("Unhealthy for more than a day\n"+strings.Join(lines, "\n")), footer)
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

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
