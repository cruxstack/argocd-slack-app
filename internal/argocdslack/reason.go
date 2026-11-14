package argocdslack

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// Reader is the read-only cluster access enrichment needs.
type Reader interface {
	Get(ctx context.Context, path string, q url.Values, out any) (bool, error)
	Text(ctx context.Context, path string, q url.Values, limit int64) (string, error)
}

// Enricher explains failures from pods, events and controllers.
type Enricher struct {
	R        Reader
	LogLines bool
}

type pod struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type containerStatus struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting    *stateDetail `json:"waiting"`
		Terminated *stateDetail `json:"terminated"`
	} `json:"state"`
	LastState struct {
		Terminated *stateDetail `json:"terminated"`
	} `json:"lastState"`
}

type stateDetail struct {
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	ExitCode int    `json:"exitCode"`
}

type podList struct {
	Items []pod `json:"items"`
}

type workload struct {
	Spec struct {
		Replicas *int `json:"replicas"`
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
	} `json:"spec"`
	Status struct {
		Replicas          int `json:"replicas"`
		UpdatedReplicas   int `json:"updatedReplicas"`
		ReadyReplicas     int `json:"readyReplicas"`
		AvailableReplicas int `json:"availableReplicas"`
	} `json:"status"`
}

type hpa struct {
	Spec struct {
		ScaleTargetRef struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"scaleTargetRef"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

type eventList struct {
	Items []struct {
		Type    string `json:"type"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Count   int    `json:"count"`
		Series  *struct {
			Count int `json:"count"`
		} `json:"series"`
		LastTimestamp time.Time `json:"lastTimestamp"`
		EventTime     time.Time `json:"eventTime"`
	} `json:"items"`
}

func workloadPath(kind, ns, name string) string {
	plural := map[string]string{
		"Deployment": "deployments", "StatefulSet": "statefulsets", "DaemonSet": "daemonsets",
		"ReplicaSet": "replicasets", "Rollout": "rollouts",
	}[kind]
	group := "/apis/apps/v1"
	if kind == "Rollout" {
		group = "/apis/argoproj.io/v1alpha1"
	}
	return fmt.Sprintf("%s/namespaces/%s/%s/%s", group, url.PathEscape(ns), plural, url.PathEscape(name))
}

// ForHealth explains why an app is unhealthy.
func (e Enricher) ForHealth(ctx context.Context, s Snapshot) *Reason {
	c := s.Culprit
	if c == nil {
		if s.Health == "Missing" {
			return &Reason{Category: CatMissing, Summary: "Resources are missing from the cluster"}
		}
		return &Reason{Category: CatUnknown, Summary: orDefault(s.HealthMessage, "Argo CD reports "+s.Health)}
	}
	ns := orDefault(c.Namespace, s.Namespace)
	switch c.Kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Rollout":
		return e.forWorkload(ctx, c.Kind, ns, c.Name)
	case "HorizontalPodAutoscaler":
		return e.forHPA(ctx, ns, c.Name)
	case "Job":
		return e.forPods(ctx, ns, "job-name="+c.Name, "Job "+ns+"/"+c.Name, -1)
	case "Pod":
		var p pod
		if ok, err := e.R.Get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", url.PathEscape(ns), url.PathEscape(c.Name)), nil, &p); err == nil && ok {
			return e.analyze(ctx, []pod{p}, c.String(), -1)
		}
	}
	return &Reason{Category: CatUnknown, Resource: c.String(), Summary: orDefault(s.HealthMessage, c.String()+" is unhealthy")}
}

// ForDeploy explains a failed sync: failed hooks first, then the sync message.
func (e Enricher) ForDeploy(ctx context.Context, s Snapshot) *Reason {
	if s.Op != nil {
		for _, r := range s.Op.FailedResults() {
			if r.Kind == "Job" {
				ns := orDefault(r.Namespace, s.Namespace)
				reason := e.forPods(ctx, ns, "job-name="+r.Name, "Job "+ns+"/"+r.Name, -1)
				if reason.Category == CatUnknown {
					reason.Category, reason.Summary = CatHook, lowerFirst(trimSentence(r.Message))
				}
				return reason
			}
		}
		if failedPhase(s.Op.Phase) {
			return &Reason{Category: CatSync, Summary: firstLine(s.Op.Message)}
		}
	}
	return e.ForHealth(ctx, s)
}

// Progress is "ready/desired" across the app's workloads.
func (e Enricher) Progress(ctx context.Context, s Snapshot) string {
	ready, want := 0, 0
	for _, w := range s.Workloads() {
		var obj workload
		ok, err := e.R.Get(ctx, workloadPath(w.Kind, orDefault(w.Namespace, s.Namespace), w.Name), nil, &obj)
		if err != nil || !ok {
			continue
		}
		n := obj.Status.Replicas
		if obj.Spec.Replicas != nil {
			n = *obj.Spec.Replicas
		}
		want += n
		ready += min(obj.Status.UpdatedReplicas, obj.Status.AvailableReplicas)
	}
	if want == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d", ready, want)
}

func (e Enricher) forWorkload(ctx context.Context, kind, ns, name string) *Reason {
	res := kind + " " + ns + "/" + name
	var w workload
	ok, err := e.R.Get(ctx, workloadPath(kind, ns, name), nil, &w)
	if err != nil || !ok || len(w.Spec.Selector.MatchLabels) == 0 {
		return &Reason{Category: CatUnknown, Resource: res, Summary: res + " is unhealthy"}
	}
	want := w.Status.Replicas
	if w.Spec.Replicas != nil {
		want = *w.Spec.Replicas
	}
	return e.forPods(ctx, ns, selector(w.Spec.Selector.MatchLabels), res, want)
}

func (e Enricher) forHPA(ctx context.Context, ns, name string) *Reason {
	res := "HorizontalPodAutoscaler " + ns + "/" + name
	var h hpa
	ok, err := e.R.Get(ctx, fmt.Sprintf("/apis/autoscaling/v2/namespaces/%s/horizontalpodautoscalers/%s", url.PathEscape(ns), url.PathEscape(name)), nil, &h)
	if err != nil || !ok {
		return &Reason{Category: CatAutoscaler, Resource: res, Summary: "Autoscaler can't read metrics"}
	}
	// Failing pods behind the HPA are the real story (no pods, no metrics).
	if t := h.Spec.ScaleTargetRef; t.Name != "" && t.Kind != "" {
		if r := e.forWorkload(ctx, t.Kind, ns, t.Name); r.Category != CatUnknown && r.Category != "" && r.Summary != "" && r.Category != CatProbe || r.Ready != "" && !strings.HasPrefix(r.Ready, "0/0") && notAllReady(r.Ready) {
			if r.Category != CatUnknown {
				return r
			}
		}
	}
	for _, c := range h.Status.Conditions {
		if c.Status == "False" && (c.Type == "ScalingActive" || c.Type == "AbleToScale") {
			return &Reason{Category: CatAutoscaler, Resource: res, Summary: fmt.Sprintf("%s=False · %s", c.Type, c.Reason), Details: []string{trimTo(c.Message, 240)}}
		}
	}
	return &Reason{Category: CatAutoscaler, Resource: res, Summary: "Autoscaler reports a problem"}
}

func notAllReady(ready string) bool {
	var a, b int
	if _, err := fmt.Sscanf(ready, "%d/%d", &a, &b); err != nil {
		return false
	}
	return a < b
}

func selector(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ",")
}

func (e Enricher) forPods(ctx context.Context, ns, labelSelector, res string, want int) *Reason {
	var pl podList
	ok, err := e.R.Get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods", url.PathEscape(ns)), url.Values{"labelSelector": {labelSelector}}, &pl)
	if err != nil || !ok {
		return &Reason{Category: CatUnknown, Resource: res, Summary: res + " is unhealthy"}
	}
	return e.analyze(ctx, pl.Items, res, want)
}

type finding struct {
	pod       pod
	container string
	image     string
	cat       string
	summary   string
	restarts  int
	rank      int
	logs      bool
}

var catRank = map[string]int{CatImagePull: 6, CatConfig: 5, CatOOM: 4, CatCrashLoop: 3, CatPending: 2, CatProbe: 1}

func (e Enricher) analyze(ctx context.Context, pods []pod, res string, want int) *Reason {
	r := &Reason{Category: CatUnknown, Resource: res, Pods: len(pods)}
	ready := 0
	var best *finding
	for _, p := range pods {
		allReady := len(p.Status.ContainerStatuses) > 0
		for _, cs := range p.Status.ContainerStatuses {
			allReady = allReady && cs.Ready
		}
		if allReady {
			ready++
			continue
		}
		for _, f := range podFindings(p) {
			if best == nil || f.rank > best.rank || f.rank == best.rank && f.restarts > best.restarts {
				best = &f
			}
		}
	}
	if want < 0 {
		want = len(pods)
	}
	if want > 0 || len(pods) > 0 {
		r.Ready = fmt.Sprintf("%d/%d", ready, max(want, len(pods)))
	}
	if best == nil {
		if len(pods) == 0 {
			r.Category, r.Summary = CatMissing, "No pods are running"
		} else {
			r.Summary = "Pods are not ready"
		}
		return r
	}
	r.Category, r.Summary, r.Image = best.cat, best.summary, shortImage(best.image)
	r.Details = e.events(ctx, best.pod)
	if best.cat == CatProbe && len(r.Details) > 0 {
		r.Summary = r.Details[0]
		r.Details = r.Details[1:]
	}
	if e.LogLines && best.logs {
		r.Excerpt = e.excerpt(ctx, best.pod, best.container)
	}
	return r
}

func podFindings(p pod) []finding {
	var out []finding
	statuses := append(append([]containerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if cs.Ready {
			continue
		}
		f := finding{pod: p, container: cs.Name, image: cs.Image, restarts: cs.RestartCount}
		w, t, last := cs.State.Waiting, cs.State.Terminated, cs.LastState.Terminated
		lastExit := ""
		if last != nil {
			lastExit = fmt.Sprintf(" · last exit %d", last.ExitCode)
			if last.Reason != "" && last.Reason != "Error" {
				lastExit += " (" + last.Reason + ")"
			}
		}
		switch {
		case w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull" || w.Reason == "InvalidImageName"):
			f.cat, f.summary = CatImagePull, w.Reason+" · "+imagePullText(w.Message, cs.Image)
		case w != nil && (w.Reason == "CreateContainerConfigError" || w.Reason == "CreateContainerError"):
			f.cat, f.summary = CatConfig, w.Reason+" · "+trimTo(w.Message, 160)
		case last != nil && last.Reason == "OOMKilled" || t != nil && t.Reason == "OOMKilled":
			f.cat, f.summary, f.logs = CatOOM, fmt.Sprintf("OOMKilled · %d restarts", cs.RestartCount), true
		case w != nil && w.Reason == "CrashLoopBackOff":
			f.cat, f.summary, f.logs = CatCrashLoop, fmt.Sprintf("CrashLoopBackOff · %d restarts%s", cs.RestartCount, lastExit), true
		case t != nil && t.ExitCode != 0:
			f.cat, f.summary, f.logs = CatCrashLoop, fmt.Sprintf("Exited %d (%s)", t.ExitCode, orDefault(t.Reason, "Error")), true
		default:
			f.cat, f.summary = CatProbe, "Not ready"
			if cs.RestartCount > 0 {
				f.logs = true
				f.summary = fmt.Sprintf("Not ready · %d restarts%s", cs.RestartCount, lastExit)
			}
		}
		f.rank = catRank[f.cat]
		out = append(out, f)
	}
	if len(out) == 0 && p.Status.Phase == "Pending" {
		for _, c := range p.Status.Conditions {
			if c.Type == "PodScheduled" && c.Status == "False" {
				out = append(out, finding{pod: p, cat: CatPending, rank: catRank[CatPending], summary: "Pending · " + trimTo(orDefault(c.Message, c.Reason), 160)})
			}
		}
	}
	return out
}

func imagePullText(msg, image string) string {
	l := strings.ToLower(msg)
	if strings.Contains(l, "not found") || strings.Contains(l, "manifest unknown") {
		if strings.Contains(image, ".ecr.") {
			return "image not found in ECR"
		}
		return "image not found"
	}
	if strings.Contains(l, "unauthorized") || strings.Contains(l, "denied") {
		return "registry denied the pull"
	}
	return "can't pull " + shortImage(image)
}

func shortImage(image string) string {
	if image == "" {
		return ""
	}
	return path.Base(image)
}

// events lists the pod's warning events, newest first, as "message ×count".
func (e Enricher) events(ctx context.Context, p pod) []string {
	var el eventList
	q := url.Values{"fieldSelector": {"involvedObject.name=" + p.Metadata.Name}}
	if ok, err := e.R.Get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/events", url.PathEscape(p.Metadata.Namespace)), q, &el); err != nil || !ok {
		return nil
	}
	items := el.Items
	sort.SliceStable(items, func(i, j int) bool {
		ti, tj := items[i].LastTimestamp, items[j].LastTimestamp
		if ti.IsZero() {
			ti = items[i].EventTime
		}
		if tj.IsZero() {
			tj = items[j].EventTime
		}
		return ti.After(tj)
	})
	var out []string
	seen := map[string]bool{}
	for _, ev := range items {
		if ev.Type != "Warning" || seen[ev.Reason] {
			continue
		}
		seen[ev.Reason] = true
		n := ev.Count
		if ev.Series != nil && ev.Series.Count > n {
			n = ev.Series.Count
		}
		line := eventText(ev.Reason, ev.Message)
		if n > 1 {
			line += fmt.Sprintf(" ×%d", n)
		}
		out = append(out, line)
		if len(out) == 3 {
			break
		}
	}
	return out
}

func eventText(reason, msg string) string {
	switch reason {
	case "BackOff":
		if strings.Contains(msg, "pulling image") {
			return "Back-off pulling image"
		}
		return "Back-off restarting container"
	case "Unhealthy":
		if i := strings.Index(msg, ":"); i > 0 {
			return msg[:i]
		}
	case "FailedScheduling":
		return "Can't schedule: " + trimTo(msg, 120)
	case "FailedMount":
		return "Volume mount failed: " + trimTo(msg, 120)
	}
	return reason + ": " + trimTo(msg, 120)
}

func (e Enricher) excerpt(ctx context.Context, p pod, container string) string {
	base := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", url.PathEscape(p.Metadata.Namespace), url.PathEscape(p.Metadata.Name))
	q := url.Values{"container": {container}, "tailLines": {"60"}, "limitBytes": {"65536"}, "previous": {"true"}}
	logs, err := e.R.Text(ctx, base, q, 65536)
	if err != nil || strings.TrimSpace(logs) == "" {
		q.Del("previous")
		if logs, err = e.R.Text(ctx, base, q, 65536); err != nil {
			return ""
		}
	}
	return trimTo(Redact(ExtractErrorLine(logs)), 240)
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func trimTo(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
