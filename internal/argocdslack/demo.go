package argocdslack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// DemoCluster is the made-up cluster `/argocd demo` posts about.
const DemoCluster = "demo"

// The demo runs on a virtual clock that starts demoSpan before it begins, so
// every time it shows is in the past and the durations look real.
const (
	demoSpan  = 25 * time.Minute
	demoPause = 4 * time.Second
)

// demoStories maps each story to the apps that play it. Fillers only count
// toward the baseline's total.
var demoStories = map[string][]string{
	"baseline":    {"legacy-reports", "nightly-export"},
	"deploy":      {"checkout-api"},
	"failed":      {"payments-worker"},
	"unhealthy":   {"search-indexer"},
	"incident":    {"web-frontend"},
	"unreachable": nil,
	"digest":      nil,
}

var demoOrder = []string{"deploy", "failed", "unhealthy", "incident", "baseline", "unreachable", "digest"}

// demoUnreachable is a made-up error for the unreachable story.
const demoUnreachable = "describe cluster demo: operation error EKS: DescribeCluster, get identity: get credentials: failed to refresh cached credentials, " +
	"operation error STS: AssumeRole, https response error StatusCode: 403, RequestID: 5f1c2e9a-3b7d-4c11-9a0e-8d2f6b4e7c13, " +
	"api error AccessDenied: User: arn:aws:sts::111122223333:assumed-role/argocd-slack/argocd-slack is not authorized to perform: " +
	"sts:AssumeRole on resource: arn:aws:iam::444455556666:role/demo-eks-cluster-argocd-slack"

var demoFillers = []string{"admin-portal", "image-resizer", "ledger-sync", "notifications"}

// DemoState is the demo's progress, handed to the next invocation when one
// Lambda run isn't long enough.
type DemoState struct {
	Channel string                `json:"channel"`
	User    string                `json:"user"`
	Stories []string              `json:"stories,omitempty"`
	Start   time.Time             `json:"start"`
	Next    int                   `json:"next"`
	Intro   Message               `json:"intro"`
	Recs    map[string]*AppRecord `json:"recs,omitempty"`
	Meta    ClusterMeta           `json:"meta"`
	Basic   bool                  `json:"basic,omitempty"`
	Hops    int                   `json:"hops,omitempty"`
}

// ParseDemoStories reads `/argocd demo [story...]`; none or "all" plays all.
func ParseDemoStories(args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		a = strings.ToLower(strings.Trim(a, ","))
		if a == "all" {
			return nil, nil
		}
		if _, ok := demoStories[a]; !ok {
			return nil, fmt.Errorf("there's no %q demo; pick from %s, or leave it out for all of them", a, strings.Join(demoOrder, ", "))
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

// startDemo posts the intro right away, so a channel the bot isn't in fails
// while the person is still looking, then plays the rest in the background.
func (a *App) startDemo(ctx context.Context, channel, user string, args []string, now time.Time) events.APIGatewayV2HTTPResponse {
	stories, err := ParseDemoStories(args)
	if err != nil {
		msg := err.Error()
		return ephemeral(strings.ToUpper(msg[:1]) + msg[1:] + ".")
	}
	if a.Cfg.DryRun {
		return ephemeral("Dry run is on, so the demo can't post. Turn off `dry_run` first.")
	}
	st := &DemoState{Channel: channel, User: user, Stories: stories, Start: now.Add(-demoSpan)}
	intro, err := a.Slack.Post(ctx, Payload{Channel: channel, Text: DemoIntro(a.Cfg, st, false)})
	if err != nil {
		var se *SlackError
		if errors.As(err, &se) && (se.Code == "not_in_channel" || se.Code == "channel_not_found") {
			return ephemeral(fmt.Sprintf("Invite me to this channel first with `/invite @%s`, then run the demo again.", a.Cfg.Setup.AppName))
		}
		return ephemeral("Couldn't start the demo: " + err.Error())
	}
	st.Intro = intro
	a.async(ctx, Job{Job: "demo", Demo: st})
	return respond(http.StatusOK, "")
}

// demoAction reports a button on a demo message.
func demoAction(actionID string, parts []string) bool {
	switch {
	case (actionID == "ack" || actionID == "mute_open") && len(parts) == 3:
		return parts[0] == DemoCluster
	case actionID == "more" && len(parts) >= 3 && parts[0] == "mute":
		return parts[1] == DemoCluster
	}
	return false
}

func (st *DemoState) plays(story string) bool {
	return len(st.Stories) == 0 || slices.Contains(st.Stories, story)
}

func (st *DemoState) playsApp(app string) bool {
	if slices.Contains(demoFillers, app) {
		return st.plays("baseline")
	}
	for story, apps := range demoStories {
		if slices.Contains(apps, app) {
			return st.plays(story)
		}
	}
	return false
}

// DemoIntro is the first message, and the last one the demo edits.
func DemoIntro(cfg Config, st *DemoState, done bool) string {
	what := "made-up apps on a `" + DemoCluster + "` cluster deploy, fail, flap and recover"
	if len(st.Stories) > 0 {
		what = "the " + strings.Join(st.Stories, ", ") + " demo plays out with made-up apps on a `" + DemoCluster + "` cluster"
	}
	if done {
		return fmt.Sprintf("*Argo CD demo* by <@%s> is done. Play it again with `%s demo`, or one story with `%s demo %s`.",
			st.User, cfg.Setup.Command, cfg.Setup.Command, strings.Join(demoOrder, "|"))
	}
	return fmt.Sprintf("*Argo CD demo* started by <@%s>. Over the next couple of minutes %s in the messages below, each one updating in place. Buttons and links don't act on anything real.",
		st.User, what)
}

// demoFrame is the world at one virtual moment.
type demoFrame struct {
	At       time.Duration
	Snaps    map[string]Snapshot
	Reasons  map[string]*Reason
	Progress map[string]string
	Mutes    []Mute
	Acks     []string
	Baseline bool
	Digest   bool
	Down     string // the cluster can't be read, with this error
	Up       bool   // the cluster can be read again
}

type demoBuilder struct {
	start    time.Time
	snaps    map[string]Snapshot
	reasons  map[string]*Reason
	progress map[string]string
	mutes    []Mute
	frames   []demoFrame
}

func (b *demoBuilder) t(d time.Duration) time.Time { return b.start.Add(d) }

func (b *demoBuilder) set(app string, fn func(s *Snapshot)) {
	s := b.snaps[app]
	fn(&s)
	b.snaps[app] = s
}

func (b *demoBuilder) health(app, health string, since time.Duration, msg string) {
	b.set(app, func(s *Snapshot) { s.Health, s.HealthSince, s.HealthMessage = health, b.t(since), msg })
}

func (b *demoBuilder) emit(at time.Duration, extra func(f *demoFrame)) {
	f := demoFrame{At: at, Snaps: map[string]Snapshot{}, Reasons: map[string]*Reason{}, Progress: map[string]string{}, Mutes: slices.Clone(b.mutes)}
	for k, v := range b.snaps {
		f.Snaps[k] = v
	}
	for k, v := range b.reasons {
		f.Reasons[k] = v
	}
	for k, v := range b.progress {
		f.Progress[k] = v
	}
	if extra != nil {
		extra(&f)
	}
	b.frames = append(b.frames, f)
}

// deploy starts a sync of a new commit.
func (b *demoBuilder) deploy(app string, at time.Duration, commit, actor, pr, title, image string) {
	b.set(app, func(s *Snapshot) {
		s.Op = &Operation{Phase: "Running", StartedAt: b.t(at), Revision: commit, Automated: true}
		s.Commit, s.Revision, s.Actor, s.PR, s.CommitTitle, s.Images = commit, commit, actor, pr, title, []string{image}
		s.Sync = "OutOfSync"
	})
}

func (b *demoBuilder) synced(app string, at time.Duration, health string) {
	b.set(app, func(s *Snapshot) {
		op := *s.Op
		op.Phase, op.FinishedAt = "Succeeded", b.t(at)
		s.Op, s.Sync, s.Health, s.HealthSince = &op, "Synced", health, b.t(at)
	})
}

func (b *demoBuilder) retry(app string, n int) {
	b.set(app, func(s *Snapshot) { op := *s.Op; op.RetryCount = n; s.Op = &op })
}

// demoFrames is the script. Each story runs alongside the others; the times
// are offsets on the virtual clock.
func demoFrames(start time.Time, user string) []demoFrame {
	m := time.Minute
	b := &demoBuilder{start: start, snaps: map[string]Snapshot{}, reasons: map[string]*Reason{}, progress: map[string]string{}}
	cast := map[string]string{
		"checkout-api": "shop", "payments-worker": "payments", "search-indexer": "search", "web-frontend": "web",
		"legacy-reports": "reports", "nightly-export": "data",
	}
	for _, f := range demoFillers {
		cast[f] = f
	}
	for app, ns := range cast {
		b.snaps[app] = Snapshot{App: app, Namespace: ns, Health: "Healthy", Sync: "Synced", Repo: "acme/" + strings.SplitN(app, "-", 2)[0]}
	}

	// Already broken when the app starts watching: one crash-looping app and
	// one whose last sync failed two days ago.
	b.health("legacy-reports", "Degraded", -3*time.Hour-12*m, "Deployment reports/legacy-reports is degraded")
	b.reasons["legacy-reports"] = &Reason{Category: CatCrashLoop, Resource: "Deployment reports/legacy-reports", Ready: "0/1",
		Summary: "CrashLoopBackOff · 41 restarts · last exit 1", Excerpt: "panic: open /data/reports.db: read-only file system", Pods: 1}
	b.set("nightly-export", func(s *Snapshot) {
		s.Commit, s.Sync = "9f2c4e1a7b", "OutOfSync"
		s.Op = &Operation{Phase: "Failed", StartedAt: b.t(-49 * time.Hour), FinishedAt: b.t(-49*time.Hour + 3*m), Revision: "9f2c4e1a7b", Automated: true, RetryCount: 4,
			Message: "one or more synchronization tasks completed unsuccessfully",
			Results: []ResourceResult{{Kind: "Job", Namespace: "data", Name: "export-migrate", Status: "SyncFailed", HookType: "PreSync", HookPhase: "Failed",
				Message: "Job has reached the specified backoff limit"}}}
	})
	b.emit(0, func(f *demoFrame) { f.Baseline = true })

	// A deploy that rolls out pod by pod, and one whose migration fails.
	b.deploy("checkout-api", 1*m, "4be81c07d2", "mara-lee", "482", "feat: one-tap checkout for returning customers", "checkout-api:2.14.0")
	b.deploy("payments-worker", 1*m, "c71d09ae35", "dev-okafor", "219", "chore: add an index on ledger_entries", "payments-worker:5.3.0")
	b.emit(1*m, nil)

	b.synced("checkout-api", 1*m+25*time.Second, "Progressing")
	b.progress["checkout-api"] = "1/3"
	b.retry("payments-worker", 1)
	b.health("web-frontend", "Degraded", 1*m+40*time.Second, "Deployment web/web-frontend is degraded")
	b.reasons["web-frontend"] = &Reason{Category: CatCrashLoop, Resource: "Deployment web/web-frontend", Ready: "1/3",
		Summary: "CrashLoopBackOff · 6 restarts · last exit 1", Excerpt: "Error: connect ECONNREFUSED 10.0.12.7:6379 (session store)",
		Details: []string{"Back-off restarting failed container ×6", "Readiness probe failed ×18"}, Image: "web-frontend:8.1.3", Pods: 3}
	b.emit(1*m+40*time.Second, nil)

	b.progress["checkout-api"] = "2/3"
	b.retry("payments-worker", 2)
	b.emit(2*m+30*time.Second, nil)

	b.health("checkout-api", "Healthy", 3*m+20*time.Second, "")
	delete(b.progress, "checkout-api")
	b.set("payments-worker", func(s *Snapshot) {
		op := *s.Op
		op.Phase, op.FinishedAt, op.Message = "Failed", b.t(3*m+20*time.Second), "one or more synchronization tasks completed unsuccessfully"
		op.Results = []ResourceResult{{Kind: "Job", Namespace: "payments", Name: "payments-migrate", Status: "SyncFailed", HookType: "PreSync", HookPhase: "Failed",
			Message: "Job has reached the specified backoff limit"}}
		s.Op = &op
	})
	b.reasons["payments-worker"] = &Reason{Category: CatHook, Resource: "Job payments/payments-migrate", Summary: "PreSync hook failed · exit 1",
		Excerpt: `ERROR: relation "ledger_entries_account_idx" already exists`, Pods: 1}
	b.deploy("search-indexer", 3*m+30*time.Second, "e09a6f3b18", "jun-park", "77", "feat: index product reviews", "search-indexer:1.9.0")
	b.emit(3*m+30*time.Second, nil)

	// A deploy that syncs, then runs out of memory.
	b.synced("search-indexer", 4*m, "Progressing")
	b.progress["search-indexer"] = "0/2"
	b.emit(4*m+10*time.Second, nil)

	b.health("search-indexer", "Degraded", 5*m, "Deployment search/search-indexer is degraded")
	b.reasons["search-indexer"] = &Reason{Category: CatOOM, Resource: "Deployment search/search-indexer", Ready: "0/2",
		Summary: "OOMKilled · 3 restarts · limit 512Mi", Details: []string{"Container indexer was OOMKilled ×3"}, Image: "search-indexer:1.9.0", Pods: 2}
	b.emit(5*m, nil)

	// web-frontend has been degraded for five minutes, so its incident opens.
	b.emit(7*m, nil)

	b.deploy("payments-worker", 8*m, "d4a2b7c901", "dev-okafor", "221", "fix: make the ledger_entries migration idempotent", "payments-worker:5.3.1")
	delete(b.reasons, "payments-worker")
	b.emit(8*m, func(f *demoFrame) { f.Acks = []string{"web-frontend"} })

	b.synced("payments-worker", 9*m, "Healthy")
	b.emit(9*m+30*time.Second, nil)

	b.health("legacy-reports", "Healthy", 10*m, "")
	b.emit(10*m+10*time.Second, nil)

	b.health("web-frontend", "Healthy", 12*m, "")
	b.emit(12*m, nil)

	b.health("search-indexer", "Healthy", 13*m, "")
	b.emit(13*m, nil)

	// The session store drops again within the flap window, so the same
	// message reopens, then someone mutes it until it's healthy.
	b.health("web-frontend", "Degraded", 14*m+30*time.Second, "Deployment web/web-frontend is degraded")
	b.reasons["web-frontend"] = &Reason{Category: CatCrashLoop, Resource: "Deployment web/web-frontend", Ready: "2/3",
		Summary: "CrashLoopBackOff · 2 restarts · last exit 1", Excerpt: "Error: connect ETIMEDOUT 10.0.12.7:6379 (session store)", Image: "web-frontend:8.1.3", Pods: 3}
	b.emit(14*m+30*time.Second, nil)

	b.mutes = []Mute{{ID: "demo", App: "web-frontend", Cluster: DemoCluster, Kinds: []string{KindIncidents}, Until: b.t(24 * time.Hour),
		UntilHealthy: true, By: user, Reason: "session store failover in progress", CreatedAt: b.t(15*m + 30*time.Second)}}
	b.emit(15*m+30*time.Second, nil)

	b.health("web-frontend", "Healthy", 17*m, "")
	b.mutes = nil
	b.emit(17*m, nil)

	// The app loses access to the cluster for a few minutes. The notice posts
	// once reads have failed for five.
	b.emit(17*m+10*time.Second, func(f *demoFrame) { f.Down = demoUnreachable })
	b.emit(22*m+10*time.Second, func(f *demoFrame) { f.Down = demoUnreachable })
	b.emit(23*m+40*time.Second, func(f *demoFrame) { f.Up = true })

	b.emit(24*m, func(f *demoFrame) { f.Digest = true })
	return b.frames
}

// demoConfig posts to one channel about the demo cluster, which borrows the
// first enabled cluster's links so the buttons look like the real ones.
func (a *App) demoConfig(channel string) Config {
	cfg := a.Cfg
	cfg.DryRun, cfg.ChannelOverride, cfg.Owners = false, channel, nil
	cfg.SCM = SCM{} // the demo repos don't exist
	cl := Cluster{Name: DemoCluster, Env: "dev", Region: "us-east-1", Cluster: DemoCluster, Namespace: "argocd", Enabled: true}
	for _, c := range a.Cfg.Clusters {
		if c.Enabled {
			cl.Env, cl.Region, cl.ArgoCDURL, cl.Links, cl.Vars = c.Env, c.Region, c.ArgoCDURL, c.Links, c.Vars
			break
		}
	}
	cfg.Clusters = []Cluster{cl}
	return cfg
}

type countingSlack struct {
	Slack
	n *int
}

func (c countingSlack) Post(ctx context.Context, p Payload) (Message, error) {
	*c.n++
	return c.Slack.Post(ctx, p)
}

func (c countingSlack) Update(ctx context.Context, p Payload) error {
	*c.n++
	return c.Slack.Update(ctx, p)
}

func (a *App) sleep(ctx context.Context, d time.Duration) {
	if a.Sleep != nil {
		a.Sleep(ctx, d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func timeLeft(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		return time.Until(d)
	}
	return time.Hour
}

// RunDemo plays the script through the real state machine and renderers,
// pausing between frames so people can watch messages change. When the Lambda
// is about to time out it hands the rest to a fresh invocation.
func (a *App) RunDemo(ctx context.Context, st *DemoState) error {
	if st == nil || st.Channel == "" {
		return errors.New("demo job without a channel")
	}
	frames := demoFrames(st.Start, st.User)
	now := st.Start
	calls := 0
	da := &App{Cfg: a.demoConfig(st.Channel), Slack: countingSlack{Slack: a.Slack, n: &calls}, Log: a.Log.With("demo", st.Channel), Now: func() time.Time { return now }}
	if st.Basic || a.richOff.Load() {
		da.richOff.Store(true)
	}
	if st.Recs == nil {
		st.Recs = map[string]*AppRecord{}
	}
	cl := da.Cfg.Clusters[0]
	st.Meta.Cluster = cl.Name
	w := &worker{a: da, cl: cl, recs: st.Recs, meta: &st.Meta}

	first := st.Next
	for ; st.Next < len(frames); st.Next++ {
		if st.Next > first && calls > 0 {
			if timeLeft(ctx) < 25*time.Second {
				return a.handOffDemo(ctx, st, da)
			}
			a.sleep(ctx, demoPause)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		calls = 0
		f := frames[st.Next]
		now = st.Start.Add(f.At)
		w.demoFrame(ctx, st, f)
	}
	_ = a.Slack.Update(ctx, Payload{Channel: st.Intro.Channel, TS: st.Intro.TS, Text: DemoIntro(a.Cfg, st, true)})
	a.Log.Info("demo finished", "channel", st.Channel, "user", st.User)
	return nil
}

func (a *App) handOffDemo(ctx context.Context, st *DemoState, da *App) error {
	if st.Hops >= 8 {
		return errors.New("demo stopped after 8 hand-offs")
	}
	st.Hops++
	st.Basic = da.richOff.Load()
	a.async(ctx, Job{Job: "demo", Demo: st})
	return nil
}

func (w *worker) demoFrame(ctx context.Context, st *DemoState, f demoFrame) {
	if f.Digest {
		if st.plays("digest") {
			w.demoDigest(ctx)
		}
		return
	}
	if f.Down != "" || f.Up {
		switch {
		case !st.plays("unreachable"):
		case f.Up:
			w.reachable(ctx)
		default:
			w.unreachable(ctx, errors.New(f.Down))
		}
		return
	}
	if f.Baseline && !st.plays("baseline") {
		return
	}
	now := w.a.now()
	w.mutes = f.Mutes
	w.snaps = map[string]Snapshot{}
	var apps []string
	for app, s := range f.Snaps {
		if st.playsApp(app) {
			w.snaps[app] = s
			apps = append(apps, app)
		}
	}
	sort.Strings(apps)
	for _, app := range apps {
		rec := w.recs[app]
		if rec == nil {
			rec = &AppRecord{Cluster: w.cl.Name, App: app}
			w.recs[app] = rec
		}
		step := Advance(w.a.Cfg, rec, f.Snaps[app], now, f.Baseline)
		reason := f.Reasons[app]
		if d := rec.Deploy; d != nil {
			if p := f.Progress[app]; step.NeedProgress && p != "" && p != d.Progress {
				d.Progress, d.Dirty = p, true
			}
			if step.NeedDeployReason && reason != nil {
				d.Reason, d.Dirty = reason, true
			}
		}
		if inc := rec.Incident; inc != nil && step.NeedIncidentReason && reason != nil {
			inc.ReasonAt = now
			if inc.Reason == nil || inc.Reason.Summary != reason.Summary || inc.Reason.Excerpt != reason.Excerpt {
				inc.Reason, inc.Dirty = reason, true
			}
		}
		if inc := rec.Incident; inc != nil && inc.State == IncidentOpen && inc.AckBy == "" && slices.Contains(f.Acks, app) {
			inc.AckBy, inc.Dirty = st.User, true
			inc.Timeline = append(inc.Timeline, quietEntry(now, "<@%s> is on it.", st.User))
		}
		if f.Baseline {
			w.collectBaseline(rec)
			continue
		}
		w.baselineProgress(rec)
		w.sync(ctx, rec)
	}
	switch {
	case f.Baseline:
		render := func(rich bool) Payload { return RenderBaseline(w.a.Cfg, w.cl, len(w.snaps), w.meta.BaselineRows, rich) }
		msg, err := w.post(ctx, w.a.Cfg.ChannelFor(w.cl), render)
		if err != nil {
			w.a.Log.Error("post demo baseline", "error", err)
			return
		}
		w.meta.Baseline, w.meta.BaselineDone = msg, true
	case w.meta.BaselineDirty:
		w.updateBaseline(ctx)
	}
}

func (w *worker) demoDigest(ctx context.Context) {
	now := w.a.now()
	title := fmt.Sprintf("Daily summary · %s · %s", w.cl.Env, now.In(w.a.Cfg.Location()).Format("Mon Jan 2"))
	rows := []DigestRow{
		{App: "nightly-export", Clusters: DemoCluster, For: "2d", Reason: "sync failed: PreSync job export-migrate hit its backoff limit", URL: w.cl.AppURL("nightly-export")},
		{App: "preview-pr-311", Clusters: DemoCluster, For: "4d", Reason: "image not found", Note: "preview cleanup?", URL: w.cl.AppURL("preview-pr-311")},
	}
	stats := Stats{Deploys: 4, Failed: 1, Resolved: 3, Blips: 2}
	if _, err := w.post(ctx, w.a.Cfg.ChannelFor(w.cl), func(rich bool) Payload { return RenderDigest(title, rows, stats, rich) }); err != nil {
		w.a.Log.Error("post demo digest", "error", err)
	}
}
