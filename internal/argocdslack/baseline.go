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
		summary := "sync failed"
		if len(d.Failures) > 0 {
			summary = "sync failed: " + stripMrkdwn(d.Failures[0])
		} else if d.OpMessage != "" {
			summary = "sync failed: " + firstLine(d.OpMessage)
		}
		w.addRow(BaselineRow{App: rec.App, Kind: "deploy", Summary: trimTo(summary, 140), Since: d.FinishedAt})
		return
	}
	if inc := rec.Incident; inc != nil && inc.State == IncidentOpen {
		inc.Baseline = true
		summary := strings.ToLower(inc.Health)
		if inc.Reason != nil && inc.Reason.Summary != "" {
			summary = inc.Reason.Summary
			if inc.Reason.Excerpt != "" {
				summary += " · " + trimTo(inc.Reason.Excerpt, 60)
			}
		}
		w.addRow(BaselineRow{App: rec.App, Kind: "incident", Summary: trimTo(summary, 140), Since: inc.FirstUnhealthyAt})
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
			r.Resolved, w.meta.BaselineDirty = true, true
		}
	}
}

func (w *worker) postBaseline(ctx context.Context, total int) {
	channel := w.a.Cfg.ChannelFor(w.cl)
	render := func(bool) Payload { return RenderBaseline(w.cl, total, w.meta.BaselineRows, w.a.now()) }
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
	render := func(bool) Payload { return RenderBaseline(w.cl, total, w.meta.BaselineRows, w.a.now()) }
	if err := w.update(ctx, w.meta.Baseline, render); err != nil {
		w.a.Log.Error("update baseline", "cluster", w.cl.Name, "error", err)
		return
	}
	w.meta.BaselineDirty = false
}
