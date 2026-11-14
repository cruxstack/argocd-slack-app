package argocdslack

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Digest posts the weekday summary to each channel that has something
// unhealthy for longer than Grace.LongRunning. Nothing is posted otherwise.
func (a *App) Digest(ctx context.Context) error {
	now := a.now()
	local := now.In(a.Cfg.Location())
	today, yesterday := local.Format("2006-01-02"), local.AddDate(0, 0, -1).Format("2006-01-02")
	mutes, _ := a.Store.Mutes(ctx)

	type item struct {
		row      DigestRow
		clusters []string
		age      time.Duration
	}
	byChannel := map[string]map[string]*item{}
	stats := map[string]*Stats{}
	envs := map[string]map[string]bool{}
	for _, cl := range a.Cfg.Clusters {
		if !cl.Enabled {
			continue
		}
		ch := a.Cfg.ChannelFor(cl)
		if byChannel[ch] == nil {
			byChannel[ch], stats[ch], envs[ch] = map[string]*item{}, &Stats{}, map[string]bool{}
		}
		envs[ch][cl.Env] = true
		recs, meta, err := a.Store.LoadCluster(ctx, cl.Name)
		if err != nil {
			return err
		}
		for _, day := range []string{today, yesterday} {
			if s := meta.Stats[day]; s != nil {
				st := stats[ch]
				st.Deploys += s.Deploys
				st.Failed += s.Failed
				st.Resolved += s.Resolved
				st.Blips += s.Blips
			}
		}
		for _, rec := range recs {
			var since time.Time
			reason := ""
			switch {
			case rec.Incident != nil && rec.Incident.State == IncidentOpen:
				since = rec.Incident.FirstUnhealthyAt
				reason = strings.ToLower(rec.Incident.Health)
				if r := rec.Incident.Reason; r != nil && r.Summary != "" {
					reason = digestReason(r)
				}
			case rec.Deploy != nil && rec.Deploy.State == DeployFailed:
				since = rec.Deploy.FinishedAt
				reason = "deploy failed"
				if r := rec.Deploy.Reason; r != nil && r.Summary != "" {
					reason += ": " + digestReason(r)
				}
			default:
				continue
			}
			age := now.Sub(since)
			if since.IsZero() || age < a.Cfg.Grace.LongRunning.Duration {
				continue
			}
			k := rec.App + "|" + reason
			it := byChannel[ch][k]
			if it == nil {
				it = &item{row: DigestRow{App: rec.App, Reason: reason, URL: cl.AppURL(rec.App)}}
				byChannel[ch][k] = it
			}
			it.clusters = append(it.clusters, cl.Name)
			if age > it.age {
				it.age = age
			}
			if m := FindMute(mutes, cl, rec.App, KindIncidents, now); m != nil {
				it.row.Note = "muted " + m.UntilText()
			} else if a.Cfg.IsPreview(rec.App) && age >= a.Cfg.Grace.PreviewCleanup.Duration {
				it.row.Note = "cleanup?"
			}
		}
	}

	for ch, items := range byChannel {
		if len(items) == 0 {
			continue
		}
		var rows []DigestRow
		var list []*item
		for _, it := range items {
			list = append(list, it)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].age > list[j].age })
		for _, it := range list {
			sort.Strings(it.clusters)
			it.row.Clusters = strings.Join(it.clusters, ", ")
			it.row.For, it.row.Age = short(it.age), it.age
			rows = append(rows, it.row)
		}
		first, err := a.Store.MarkOnce(ctx, "digest#"+today+"#"+ch, 3*24*time.Hour, now)
		if err != nil || !first {
			continue
		}
		var envList []string
		for e := range envs[ch] {
			envList = append(envList, e)
		}
		sort.Strings(envList)
		title := fmt.Sprintf("Daily summary · %s · %s", strings.Join(envList, " and "), local.Format("Mon Jan 2"))
		p := RenderDigest(title, rows, *stats[ch], !a.richOff.Load())
		p.Channel = ch
		_, err = a.Slack.Post(ctx, p)
		if err != nil && IsInvalidBlocks(err) {
			p = RenderDigest(title, rows, *stats[ch], false)
			p.Channel = ch
			_, err = a.Slack.Post(ctx, p)
		}
		if err != nil {
			a.Log.Error("post digest", "channel", ch, "error", err)
		}
	}
	return nil
}

func digestReason(r *Reason) string {
	switch r.Category {
	case CatImagePull:
		if strings.Contains(r.Summary, "not found") {
			return "image not found"
		}
		return "can't pull image"
	case CatCrashLoop:
		if r.Excerpt != "" {
			return "crash looping · " + trimTo(r.Excerpt, 60)
		}
		return "crash looping"
	}
	return trimTo(r.Summary, 80)
}

// Home publishes a user's App Home tab.
func (a *App) Home(ctx context.Context, user string) error {
	if user == "" {
		return nil
	}
	now := a.now()
	mutes, err := a.Store.Mutes(ctx)
	if err != nil {
		return err
	}
	var clusters []HomeCluster
	for _, cl := range a.Cfg.Clusters {
		hc := HomeCluster{Name: cl.Name, Enabled: cl.Enabled}
		if cl.Enabled {
			recs, meta, err := a.Store.LoadCluster(ctx, cl.Name)
			if err != nil {
				return err
			}
			hc.LastPoll = meta.LastPoll
			for _, r := range recs {
				switch {
				case r.Incident != nil && r.Incident.State == IncidentOpen:
					hc.Unhealthy++
					if FindMute(mutes, cl, r.App, KindIncidents, now) != nil {
						hc.Muted++
					}
				case r.Deploy != nil && r.Deploy.State == DeployFailed:
					hc.Unhealthy++
				default:
					hc.Healthy++
				}
			}
		}
		clusters = append(clusters, hc)
	}
	return a.Slack.PublishHome(ctx, user, RenderHome(clusters, mutes, now))
}
