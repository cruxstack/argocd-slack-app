package argocdslack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cruxstack/argocd-slack-app/internal/kube"
)

// ClusterAPI is what the poller needs from a cluster connection.
type ClusterAPI interface {
	Reader
	Watch(ctx context.Context, path, resourceVersion string, timeout time.Duration, fn func(kube.Event)) (string, error)
}

// App wires config, storage, Slack and cluster access together.
type App struct {
	Cfg      Config
	Store    Store
	Slack    Slack
	Log      *slog.Logger
	Connect  func(ctx context.Context, cl Cluster) (ClusterAPI, error)
	Invoke   func(ctx context.Context, job Job) error
	Now      func() time.Time
	Secrets  Secrets
	SetupAPI SetupAPI
	Sleep    func(ctx context.Context, d time.Duration) // demo pacing; tests skip it

	richOff atomic.Bool
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func appsPath(ns string) string {
	return "/apis/argoproj.io/v1alpha1/namespaces/" + ns + "/applications"
}

// Poll runs one scheduled cycle: list, then watch each enabled cluster until
// the configured deadline. A DynamoDB lease keeps cycles from overlapping.
func (a *App) Poll(ctx context.Context, owner string) error {
	start := a.now()
	ok, err := a.Store.AcquireLease(ctx, owner, time.Duration(a.Cfg.WatchSeconds+20)*time.Second, start)
	if err != nil {
		return fmt.Errorf("lease: %w", err)
	}
	if !ok {
		a.Log.Info("another poll holds the lease")
		return nil
	}
	defer func() { _ = a.Store.ReleaseLease(context.WithoutCancel(ctx), owner) }()

	mutes, err := a.Store.Mutes(ctx)
	if err != nil {
		return fmt.Errorf("mutes: %w", err)
	}
	mutes = a.expireMutes(ctx, mutes, start)

	deadline := start.Add(time.Duration(a.Cfg.WatchSeconds) * time.Second)
	var wg sync.WaitGroup
	var failed atomic.Int32
	for _, cl := range a.Cfg.Clusters {
		if !cl.Enabled {
			continue
		}
		wg.Add(1)
		go func(cl Cluster) {
			defer wg.Done()
			if err := a.pollCluster(ctx, cl, mutes, deadline); err != nil {
				failed.Add(1)
				a.Log.Error("poll failed", "cluster", cl.Name, "error", err)
			}
		}(cl)
	}
	wg.Wait()
	if failed.Load() == 0 {
		emitMetric("PollSucceeded", 1)
	}
	return nil
}

// metricOut receives embedded-metric lines; CloudWatch reads them from stdout.
var metricOut io.Writer = os.Stdout

// emitMetric writes a CloudWatch embedded-metric log line with no dimensions.
func emitMetric(name string, v float64) {
	line := map[string]any{
		"_aws": map[string]any{
			"Timestamp": time.Now().UnixMilli(),
			"CloudWatchMetrics": []any{map[string]any{
				"Namespace":  "ArgoCDSlack",
				"Dimensions": [][]string{{}},
				"Metrics":    []any{map[string]string{"Name": name, "Unit": "Count"}},
			}},
		},
		name: v,
	}
	b, _ := json.Marshal(line)
	fmt.Fprintln(metricOut, string(b))
}

type worker struct {
	a        *App
	cl       Cluster
	api      ClusterAPI
	recs     map[string]*AppRecord
	snaps    map[string]Snapshot
	saved    map[string][]byte
	meta     *ClusterMeta
	mutes    []Mute
	enricher Enricher
	lastProg map[string]time.Time
}

func (a *App) pollCluster(ctx context.Context, cl Cluster, mutes []Mute, deadline time.Time) error {
	cctx, cancel := context.WithDeadline(ctx, deadline.Add(4*time.Second))
	defer cancel()
	recs, meta, err := a.Store.LoadCluster(cctx, cl.Name)
	if err != nil {
		return err
	}
	if meta.BaselineDone && a.needsBaseline(cl, meta) {
		resetBaseline(meta)
	}
	w := &worker{a: a, cl: cl, recs: recs, meta: meta, mutes: append([]Mute(nil), mutes...), snaps: map[string]Snapshot{}, saved: map[string][]byte{}, lastProg: map[string]time.Time{}}
	for k, r := range recs {
		w.saved[k], _ = json.Marshal(r)
	}
	api, err := a.Connect(cctx, cl)
	if err == nil {
		w.api = api
		w.enricher = Enricher{R: api, LogLines: cl.LogLines}
		err = w.list(cctx)
	}
	if err != nil {
		w.unreachable(cctx, err)
		w.saveMeta(context.WithoutCancel(ctx))
		return err
	}
	w.reachable(cctx)

	rv := meta.ResourceVersion
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		remaining := deadline.Sub(a.now())
		if remaining < 3*time.Second {
			break
		}
		events := make(chan kube.Event, 64)
		done := make(chan error, 1)
		wctx, wcancel := context.WithDeadline(cctx, deadline)
		go func(from string) {
			var werr error
			rv, werr = w.api.Watch(wctx, appsPath(cl.Namespace), from, remaining, func(ev kube.Event) { events <- ev })
			done <- werr
		}(rv)
	loop:
		for {
			select {
			case ev := <-events:
				w.event(cctx, ev)
			case <-ticker.C:
				w.tick(cctx)
			case werr := <-done:
				for len(events) > 0 {
					w.event(cctx, <-events)
				}
				wcancel()
				if errors.Is(werr, kube.ErrGone) {
					if err := w.list(cctx); err != nil {
						return err
					}
					rv = w.meta.ResourceVersion
				} else if werr != nil && cctx.Err() == nil {
					a.Log.Warn("watch ended", "cluster", cl.Name, "error", werr)
					time.Sleep(time.Second)
				}
				break loop
			}
		}
	}
	w.tick(cctx)
	w.meta.LastPoll = a.now()
	w.saveMeta(context.WithoutCancel(ctx))
	return nil
}

// list reads every Application, advances each, and handles apps that vanished.
func (w *worker) list(ctx context.Context) error {
	var l kube.List
	if _, err := w.api.Get(ctx, appsPath(w.cl.Namespace), nil, &l); err != nil {
		return fmt.Errorf("list applications: %w", err)
	}
	w.meta.ResourceVersion = l.Metadata.ResourceVersion
	baseline := !w.meta.BaselineDone
	seen := map[string]bool{}
	for _, raw := range l.Items {
		snap, err := ParseApplication(raw, w.a.Cfg.Annotations)
		if err != nil || snap.App == "" {
			continue
		}
		seen[snap.App] = true
		w.snaps[snap.App] = snap
		w.handle(ctx, snap, baseline)
	}
	for app, rec := range w.recs {
		if seen[app] {
			continue
		}
		rec.MissingPolls++
		if rec.MissingPolls >= 2 {
			w.remove(ctx, rec)
		} else {
			w.save(ctx, rec)
		}
	}
	if baseline {
		w.postBaseline(ctx, len(l.Items))
	}
	return nil
}

func (w *worker) event(ctx context.Context, ev kube.Event) {
	snap, err := ParseApplication(ev.Object, w.a.Cfg.Annotations)
	if err != nil || snap.App == "" {
		return
	}
	if ev.Type == "DELETED" {
		delete(w.snaps, snap.App)
		if rec := w.recs[snap.App]; rec != nil {
			w.remove(ctx, rec)
		}
		return
	}
	w.snaps[snap.App] = snap
	w.handle(ctx, snap, false)
}

// tick re-reads mutes and re-advances every app so time-based rules (grace,
// stall, mute changes) take effect.
func (w *worker) tick(ctx context.Context) {
	if mutes, err := w.a.Store.Mutes(ctx); err == nil {
		now := w.a.now()
		w.mutes = w.mutes[:0]
		for _, m := range mutes {
			if m.Active(now) {
				w.mutes = append(w.mutes, m)
			}
		}
	}
	names := make([]string, 0, len(w.snaps))
	for n := range w.snaps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w.handle(ctx, w.snaps[n], false)
	}
	if w.meta.BaselineDirty {
		w.updateBaseline(ctx)
	}
}

func (w *worker) stats() *Stats {
	day := w.a.now().In(w.a.Cfg.Location()).Format("2006-01-02")
	if w.meta.Stats == nil {
		w.meta.Stats = map[string]*Stats{}
	}
	if w.meta.Stats[day] == nil {
		w.meta.Stats[day] = &Stats{}
		for k := range w.meta.Stats {
			if k < w.a.now().AddDate(0, 0, -7).Format("2006-01-02") {
				delete(w.meta.Stats, k)
			}
		}
	}
	return w.meta.Stats[day]
}

func (w *worker) handle(ctx context.Context, snap Snapshot, baseline bool) {
	now := w.a.now()
	rec := w.recs[snap.App]
	if rec == nil {
		rec = &AppRecord{Cluster: w.cl.Name, App: snap.App}
		w.recs[snap.App] = rec
	}
	st := Advance(w.a.Cfg, rec, snap, now, baseline)

	if st.NewDeploy && rec.Deploy != nil && !rec.Deploy.Quiet {
		w.stats().Deploys++
	}
	if st.DeployFailed {
		w.stats().Failed++
	}
	if st.IncidentResolved {
		w.stats().Resolved++
		w.clearUntilHealthy(ctx, snap.App)
	}
	if st.Blip {
		w.stats().Blips++
	}
	if d := rec.Deploy; d != nil && !st.NewDeploy && (d.State == DeployRecovered || d.State == DeployHealthy) && healthyish(snap.Health) {
		w.clearUntilHealthy(ctx, snap.App)
	}
	if d := rec.Deploy; st.NeedProgress && d != nil && w.api != nil {
		if now.Sub(w.lastProg[snap.App]) >= 10*time.Second {
			w.lastProg[snap.App] = now
			if p := w.enricher.Progress(ctx, snap); p != "" && p != d.Progress {
				d.Progress, d.Dirty = p, true
			}
		}
	}
	if d := rec.Deploy; st.NeedDeployReason && d != nil && w.api != nil {
		if d.State == DeployFailed {
			d.Reason = w.enricher.ForDeploy(ctx, snap)
		} else {
			d.Reason = w.enricher.ForHealth(ctx, snap)
		}
		d.Dirty = true
	}
	if inc := rec.Incident; st.NeedIncidentReason && inc != nil && w.api != nil {
		r := w.enricher.ForHealth(ctx, snap)
		inc.ReasonAt = now
		if inc.Reason == nil || inc.Reason.Summary != r.Summary || inc.Reason.Ready != r.Ready || inc.Reason.Excerpt != r.Excerpt {
			inc.Reason, inc.Dirty = r, true
		}
	}

	if baseline {
		w.collectBaseline(rec)
	} else {
		w.baselineProgress(rec)
		w.sync(ctx, rec)
	}
	w.save(ctx, rec)
}

func (w *worker) save(ctx context.Context, rec *AppRecord) {
	b, _ := json.Marshal(rec)
	if string(b) == string(w.saved[rec.App]) {
		return
	}
	err := w.a.Store.SaveRecord(ctx, rec)
	if errors.Is(err, ErrConflict) {
		// A Slack action (only "I'm on it" writes records) got there first.
		fresh, ferr := w.a.Store.GetRecord(ctx, rec.Cluster, rec.App)
		if ferr == nil && fresh != nil {
			if fresh.Incident != nil && rec.Incident != nil && fresh.Incident.ID == rec.Incident.ID && fresh.Incident.AckBy != rec.Incident.AckBy {
				rec.Incident.AckBy, rec.Incident.Dirty = fresh.Incident.AckBy, true
			}
			rec.Version = fresh.Version
			err = w.a.Store.SaveRecord(ctx, rec)
		}
	}
	if err != nil {
		w.a.Log.Error("save record", "cluster", rec.Cluster, "app", rec.App, "error", err)
		return
	}
	w.saved[rec.App], _ = json.Marshal(rec)
}

func (w *worker) saveMeta(ctx context.Context) {
	err := w.a.Store.SaveMeta(ctx, w.meta)
	if errors.Is(err, ErrConflict) {
		if _, fresh, ferr := w.a.Store.LoadCluster(ctx, w.cl.Name); ferr == nil {
			w.meta.Version = fresh.Version
			err = w.a.Store.SaveMeta(ctx, w.meta)
		}
	}
	if err != nil {
		w.a.Log.Error("save meta", "cluster", w.cl.Name, "error", err)
	}
}

func (w *worker) remove(ctx context.Context, rec *AppRecord) {
	Remove(rec, w.a.now())
	w.sync(ctx, rec)
	for i := range w.meta.BaselineRows {
		if w.meta.BaselineRows[i].App == rec.App && !w.meta.BaselineRows[i].Resolved {
			w.meta.BaselineRows[i].Resolved, w.meta.BaselineRows[i].ResolvedAt, w.meta.BaselineDirty = true, w.a.now(), true
		}
	}
	if err := w.a.Store.DeleteRecord(ctx, rec.Cluster, rec.App); err != nil {
		w.a.Log.Error("delete record", "app", rec.App, "error", err)
	}
	delete(w.recs, rec.App)
	delete(w.saved, rec.App)
}

func (w *worker) clusterView(rich bool) View {
	return View{Cfg: w.a.Cfg, Cluster: w.cl, Rich: rich, Now: w.a.now()}
}

// unreachable posts once the cluster has failed for five minutes.
func (w *worker) unreachable(ctx context.Context, cause error) {
	now := w.a.now()
	if w.meta.FailingSince.IsZero() {
		w.meta.FailingSince = now
	}
	if now.Sub(w.meta.FailingSince) < 5*time.Minute || w.meta.Unreachable.Posted() {
		return
	}
	msg, err := w.post(ctx, w.a.Cfg.ChannelFor(w.cl), func(rich bool) Payload {
		return RenderUnreachable(w.clusterView(rich), w.meta.FailingSince, cause.Error())
	})
	if err != nil {
		w.a.Log.Error("post unreachable notice", "cluster", w.cl.Name, "error", err)
		return
	}
	w.meta.Unreachable = msg
}

func (w *worker) reachable(ctx context.Context) {
	if w.meta.Unreachable.Posted() {
		if err := w.update(ctx, w.meta.Unreachable, func(rich bool) Payload {
			return RenderReachable(w.clusterView(rich), w.meta.FailingSince)
		}); err != nil {
			w.a.Log.Warn("update unreachable notice", "cluster", w.cl.Name, "error", err)
		}
		w.meta.Unreachable = Message{}
	}
	w.meta.FailingSince = time.Time{}
}

func (w *worker) clearUntilHealthy(ctx context.Context, app string) {
	keep := w.mutes[:0]
	for _, m := range w.mutes {
		if m.UntilHealthy && m.App == app && (m.Cluster == "" || m.Cluster == w.cl.Name) {
			if err := w.a.Store.DeleteMute(ctx, m.ID); err == nil {
				w.a.Log.Info("mute cleared on recovery", "mute", m.ID, "app", app)
				continue
			}
		}
		keep = append(keep, m)
	}
	w.mutes = keep
}

// expireMutes drops ended mutes and says so for wide ones.
func (a *App) expireMutes(ctx context.Context, mutes []Mute, now time.Time) []Mute {
	var active []Mute
	for _, m := range mutes {
		if m.Active(now) {
			active = append(active, m)
			continue
		}
		if err := a.Store.DeleteMute(ctx, m.ID); err != nil {
			active = append(active, m)
			continue
		}
		if m.Wide() && m.Announced.Posted() {
			_, _ = a.Slack.Post(ctx, Payload{Channel: m.Announced.Channel, ThreadTS: m.Announced.TS, Text: fmt.Sprintf("Mute on %s ended.", m.Scope())})
		}
	}
	return active
}
