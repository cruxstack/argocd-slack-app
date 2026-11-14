package argocdslack

import (
	"context"
	"strings"
)

func isDry(m Message) bool { return strings.HasPrefix(m.TS, "dry.") }

// live drops dry-run timestamps once the app posts for real, so open items
// get a real message and finished ones are left alone.
func (w *worker) live(m *Message, open bool) bool {
	if !isDry(*m) || w.a.Cfg.DryRun {
		return true
	}
	if open {
		*m = Message{}
		return true
	}
	return false
}

func (w *worker) view(rec *AppRecord) View {
	return View{Cfg: w.a.Cfg, Cluster: w.cl, App: rec.App, Namespace: orDefault(rec.Namespace, rec.App), Rich: !w.a.richOff.Load(), Now: w.a.now()}
}

func (w *worker) mention(app string) string {
	if w.cl.Env != "prod" {
		return ""
	}
	id := w.a.Cfg.Owners[app]
	switch {
	case strings.HasPrefix(id, "S"):
		return "<!subteam^" + id + ">"
	case strings.HasPrefix(id, "U"), strings.HasPrefix(id, "W"):
		return "<@" + id + ">"
	}
	return ""
}

// post sends a new message, retrying once without the newer blocks if Slack
// refuses them.
func (w *worker) post(ctx context.Context, channel string, render func(rich bool) Payload) (Message, error) {
	rich := !w.a.richOff.Load()
	p := render(rich)
	p.Channel = channel
	msg, err := w.a.Slack.Post(ctx, p)
	if err != nil && rich && IsInvalidBlocks(err) {
		w.a.richOff.Store(true)
		w.a.Log.Warn("slack refused rich blocks; using basic blocks", "error", err)
		p = render(false)
		p.Channel = channel
		msg, err = w.a.Slack.Post(ctx, p)
	}
	return msg, err
}

func (w *worker) update(ctx context.Context, m Message, render func(rich bool) Payload) error {
	rich := !w.a.richOff.Load()
	p := render(rich)
	p.Channel, p.TS = m.Channel, m.TS
	err := w.a.Slack.Update(ctx, p)
	if err != nil && rich && IsInvalidBlocks(err) {
		w.a.richOff.Store(true)
		p = render(false)
		p.Channel, p.TS = m.Channel, m.TS
		err = w.a.Slack.Update(ctx, p)
	}
	return err
}

// thread posts unposted timeline entries as silent replies, newest five at most.
func (w *worker) thread(ctx context.Context, m Message, entries []Entry) {
	var pending []int
	for i := range entries {
		if !entries[i].Posted {
			pending = append(pending, i)
		}
	}
	if len(pending) > 5 {
		for _, i := range pending[:len(pending)-5] {
			entries[i].Posted = true
		}
		pending = pending[len(pending)-5:]
	}
	for _, i := range pending {
		_, err := w.a.Slack.Post(ctx, Payload{Channel: m.Channel, ThreadTS: m.TS, Text: entries[i].Text})
		if err != nil {
			w.a.Log.Warn("thread reply failed", "error", err)
			return
		}
		entries[i].Posted = true
	}
}

func noteOf(m *Mute) string {
	if m == nil {
		return ""
	}
	return m.Note()
}

func markPosted(entries []Entry) {
	for i := range entries {
		entries[i].Posted = true
	}
}

// sync brings a record's messages up to date with its state.
func (w *worker) sync(ctx context.Context, rec *AppRecord) {
	now := w.a.now()
	for _, d := range []*Deploy{rec.LastDeploy, rec.Deploy} {
		if d == nil {
			continue
		}
		if d.Msg.Posted() && (!d.State.Final() || d.State == DeployUnhealthy) {
			kind := KindDeploys
			if d.State == DeployUnhealthy {
				kind = KindFailed
			}
			if note := noteOf(FindMute(w.mutes, w.cl, rec.App, kind, now)); note != d.MuteNote {
				d.MuteNote, d.Dirty = note, true
			}
		}
		if !d.Dirty {
			continue
		}
		d.Dirty = false
		if d.Baseline || d.Quiet && !d.Msg.Posted() {
			markPosted(d.Timeline)
			continue
		}
		open := !d.State.Final() || d.State == DeployUnhealthy
		if !w.live(&d.Msg, open) {
			continue
		}
		kind := KindDeploys
		if d.State == DeployFailed || d.State == DeployUnhealthy {
			kind = KindFailed
		}
		mute := FindMute(w.mutes, w.cl, rec.App, kind, now)
		v := w.view(rec)
		v.Mute = mute
		render := func(rich bool) Payload { vv := v; vv.Rich = rich; return RenderDeploy(vv, d) }
		if !d.Msg.Posted() {
			if mute != nil || d.State == DeploySuperseded {
				d.Muted = mute != nil
				markPosted(d.Timeline)
				continue
			}
			if kind == KindFailed && d.Mention == "" {
				d.Mention = w.mention(rec.App)
			}
			msg, err := w.post(ctx, w.a.Cfg.ChannelFor(w.cl), render)
			if err != nil {
				w.a.Log.Error("post deploy", "app", rec.App, "error", err)
				d.Dirty = true
				continue
			}
			d.Msg, d.Muted = msg, false
		} else if err := w.update(ctx, d.Msg, render); err != nil {
			w.a.Log.Error("update deploy", "app", rec.App, "error", err)
		}
		w.thread(ctx, d.Msg, d.Timeline)
	}

	for _, inc := range []*Incident{rec.LastIncident, rec.Incident} {
		if inc == nil || inc.State == IncidentPending {
			continue
		}
		if inc.State == IncidentOpen && inc.Msg.Posted() {
			if note := noteOf(FindMute(w.mutes, w.cl, rec.App, KindIncidents, now)); note != inc.MuteNote {
				inc.MuteNote, inc.Dirty = note, true
			}
		}
		// Open incidents held back by a mute post once the mute ends.
		waiting := inc.State == IncidentOpen && !inc.Msg.Posted() && !inc.Baseline
		if !inc.Dirty && !waiting {
			continue
		}
		if inc.Baseline {
			inc.Dirty = false
			markPosted(inc.Timeline)
			continue
		}
		if !w.live(&inc.Msg, inc.State == IncidentOpen) {
			inc.Dirty = false
			continue
		}
		mute := FindMute(w.mutes, w.cl, rec.App, KindIncidents, now)
		v := w.view(rec)
		v.Mute = mute
		render := func(rich bool) Payload { vv := v; vv.Rich = rich; return RenderIncident(vv, inc) }
		if !inc.Msg.Posted() {
			if inc.State != IncidentOpen {
				inc.Dirty = false
				markPosted(inc.Timeline)
				continue
			}
			if mute != nil {
				inc.Muted, inc.MuteNote, inc.Dirty = true, mute.Note(), false
				continue
			}
			if inc.Mention == "" {
				inc.Mention = w.mention(rec.App)
			}
			msg, err := w.post(ctx, w.a.Cfg.ChannelFor(w.cl), render)
			if err != nil {
				w.a.Log.Error("post incident", "app", rec.App, "error", err)
				continue
			}
			inc.Msg, inc.Muted = msg, false
		} else if err := w.update(ctx, inc.Msg, render); err != nil {
			w.a.Log.Error("update incident", "app", rec.App, "error", err)
		}
		inc.Dirty = false
		w.thread(ctx, inc.Msg, inc.Timeline)
	}
}

// Rerender repaints a record's open messages, e.g. after a mute changes.
func (a *App) Rerender(ctx context.Context, cl Cluster, rec *AppRecord, mutes []Mute) {
	w := &worker{a: a, cl: cl, mutes: mutes}
	for _, d := range []*Deploy{rec.Deploy, rec.LastDeploy} {
		if d != nil && d.Msg.Posted() && !isDry(d.Msg) {
			d.Dirty = true
		}
	}
	for _, inc := range []*Incident{rec.Incident, rec.LastIncident} {
		if inc != nil && (inc.Msg.Posted() || inc.State == IncidentOpen) {
			inc.Dirty = true
		}
	}
	w.sync(ctx, rec)
}
