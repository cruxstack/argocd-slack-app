package argocdslack

import (
	"context"
	"strings"
)

// needsBaseline is true on a cluster's first poll, after a channel move, and
// when the app starts posting for real after a dry run.
func (a *App) needsBaseline(cl Cluster, meta *ClusterMeta) bool {
	if !meta.BaselineDone {
		return true
	}
	return meta.Baseline.Channel != "" && meta.Baseline.Channel != a.Cfg.ChannelFor(cl) ||
		isDry(meta.Baseline) && !a.Cfg.DryRun
}

func resetBaseline(meta *ClusterMeta) {
	meta.BaselineDone, meta.BaselineRows, meta.Baseline, meta.BaselineDirty = false, nil, Message{}, false
}

// collectBaseline turns an already-failing app into a summary row instead of
// its own alert.
func (w *worker) collectBaseline(rec *AppRecord) {
	if d := rec.Deploy; d != nil && d.State == DeployFailed {
		d.Baseline = true
		detail := firstLine(d.OpMessage)
		if len(d.Failures) > 0 {
			detail = stripMrkdwn(d.Failures[0])
		}
		w.addRow(BaselineRow{App: rec.App, Kind: "deploy", Summary: "sync failed", Detail: trimTo(detail, 120), Since: d.FinishedAt})
		return
	}
	if inc := rec.Incident; inc != nil && inc.State == IncidentOpen {
		inc.Baseline = true
		summary, detail := strings.ToLower(inc.Health), ""
		if r := inc.Reason; r != nil {
			summary, detail = orDefault(r.Summary, summary), r.Excerpt
		}
		w.addRow(BaselineRow{App: rec.App, Kind: "incident", Summary: trimTo(summary, 100), Detail: trimTo(detail, 120), Since: inc.FirstUnhealthyAt})
	}
}

func (w *worker) addRow(r BaselineRow) {
	for _, existing := range w.meta.BaselineRows {
		if existing.App == r.App {
			return
		}
	}
	w.meta.BaselineRows = append(w.meta.BaselineRows, r)
}

func stripMrkdwn(s string) string { return strings.NewReplacer("*", "", "`", "").Replace(s) }

// baselineProgress marks a row resolved once its app recovers or redeploys.
func (w *worker) baselineProgress(rec *AppRecord) {
	for i := range w.meta.BaselineRows {
		r := &w.meta.BaselineRows[i]
		if r.App != rec.App || r.Resolved {
			continue
		}
		done := false
		switch r.Kind {
		case "deploy":
			done = rec.Deploy == nil || !rec.Deploy.Baseline
		case "incident":
			done = rec.Incident == nil || !rec.Incident.Baseline
		}
		if done {
			r.Resolved, r.ResolvedAt, w.meta.BaselineDirty = true, w.a.now(), true
		}
	}
}

func (w *worker) postBaseline(ctx context.Context, total int) {
	channel := w.a.Cfg.ChannelFor(w.cl)
	render := func(rich bool) Payload { return RenderBaseline(w.a.Cfg, w.cl, total, w.meta.BaselineRows, rich) }
	msg, err := w.post(ctx, channel, render)
	if err != nil {
		w.a.Log.Error("post baseline", "cluster", w.cl.Name, "error", err)
		return
	}
	w.meta.Baseline, w.meta.BaselineDone, w.meta.BaselineDirty = msg, true, false
	w.saveMeta(context.WithoutCancel(ctx))
}

func (w *worker) updateBaseline(ctx context.Context) {
	if !w.meta.Baseline.Posted() {
		w.meta.BaselineDirty = false
		return
	}
	total := len(w.snaps)
	render := func(rich bool) Payload { return RenderBaseline(w.a.Cfg, w.cl, total, w.meta.BaselineRows, rich) }
	if err := w.update(ctx, w.meta.Baseline, render); err != nil {
		w.a.Log.Error("update baseline", "cluster", w.cl.Name, "error", err)
		return
	}
	w.meta.BaselineDirty = false
}
