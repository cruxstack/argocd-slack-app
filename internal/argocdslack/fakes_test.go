package argocdslack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cruxstack/argocd-slack-app/internal/kube"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := LoadConfig(`{
		"channel_override": "CTEST",
		"owners": {"api": "S123"},
		"annotations": {"repo": "example.com/repo", "commit": "example.com/commit", "title": "example.com/title"},
		"links": [
			{"text": "Logs", "url": "https://grafana.example.com/explore?cluster={telemetry_cluster}&ns={namespace}"},
			{"text": "Metrics", "url": "https://grafana.example.com/d/workloads?var-namespace={namespace}"}
		],
		"clusters": [
			{"name": "dev-use1", "env": "dev", "cluster": "dev-cluster", "role_arn": "arn:aws:iam::111122223333:role/reader", "argocd_url": "https://argocd.dev.example.com", "channel": "CDEV", "enabled": true, "log_lines": true, "vars": {"telemetry_cluster": "dev-main"}},
			{"name": "prod-use1", "env": "prod", "cluster": "prod-cluster", "role_arn": "arn:aws:iam::444455556666:role/reader", "argocd_url": "https://argocd.prod.example.com", "channel": "CPROD", "enabled": false}
		]}`)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// memStore is an in-memory Store with the same versioning rules as Dynamo.
type memStore struct {
	mu    sync.Mutex
	recs  map[string][]byte
	vers  map[string]int64
	metas map[string][]byte
	mvers map[string]int64
	mutes map[string]Mute
	once  map[string]bool
	locks map[string]bool
	lease string
}

func newMemStore() *memStore {
	return &memStore{recs: map[string][]byte{}, vers: map[string]int64{}, metas: map[string][]byte{}, mvers: map[string]int64{}, mutes: map[string]Mute{}, once: map[string]bool{}}
}

func (m *memStore) LoadCluster(_ context.Context, cluster string) (map[string]*AppRecord, *ClusterMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]*AppRecord{}
	for k, b := range m.recs {
		if strings.HasPrefix(k, cluster+"/") {
			r := &AppRecord{}
			_ = json.Unmarshal(b, r)
			r.Version = m.vers[k]
			out[r.App] = r
		}
	}
	meta := &ClusterMeta{Cluster: cluster}
	if b, ok := m.metas[cluster]; ok {
		_ = json.Unmarshal(b, meta)
		meta.Version = m.mvers[cluster]
	}
	return out, meta, nil
}

func (m *memStore) GetRecord(_ context.Context, cluster, app string) (*AppRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.recs[cluster+"/"+app]
	if !ok {
		return nil, nil
	}
	r := &AppRecord{}
	_ = json.Unmarshal(b, r)
	r.Version = m.vers[cluster+"/"+app]
	return r, nil
}

func (m *memStore) SaveRecord(_ context.Context, rec *AppRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := rec.Cluster + "/" + rec.App
	if m.vers[k] != rec.Version {
		return ErrConflict
	}
	m.recs[k], _ = json.Marshal(rec)
	rec.Version++
	m.vers[k] = rec.Version
	return nil
}

func (m *memStore) DeleteRecord(_ context.Context, cluster, app string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, cluster+"/"+app)
	delete(m.vers, cluster+"/"+app)
	return nil
}

func (m *memStore) SaveMeta(_ context.Context, meta *ClusterMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mvers[meta.Cluster] != meta.Version {
		return ErrConflict
	}
	m.metas[meta.Cluster], _ = json.Marshal(meta)
	meta.Version++
	m.mvers[meta.Cluster] = meta.Version
	return nil
}

func (m *memStore) Mutes(context.Context) ([]Mute, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Mute
	for _, x := range m.mutes {
		out = append(out, x)
	}
	return out, nil
}

func (m *memStore) PutMute(_ context.Context, x Mute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mutes[x.ID] = x
	return nil
}

func (m *memStore) DeleteMute(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mutes, id)
	return nil
}

func (m *memStore) AcquireLease(_ context.Context, owner string, _ time.Duration, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lease != "" && m.lease != owner {
		return false, nil
	}
	m.lease = owner
	return true, nil
}

func (m *memStore) ReleaseLease(_ context.Context, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lease == owner {
		m.lease = ""
	}
	return nil
}

func (m *memStore) TryLock(_ context.Context, name string, _ time.Duration, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = map[string]bool{}
	}
	if m.locks[name] {
		return false, nil
	}
	m.locks[name] = true
	return true, nil
}

func (m *memStore) Unlock(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.locks, name)
	return nil
}

func (m *memStore) MarkOnce(_ context.Context, k string, _ time.Duration, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.once[k] {
		return false, nil
	}
	m.once[k] = true
	return true, nil
}

// fakeSlack records calls and hands out sequential timestamps.
type fakeSlack struct {
	mu         sync.Mutex
	posts      []Payload
	updates    []Payload
	ephem      []string
	n          int
	rejectRich bool
}

func (f *fakeSlack) Post(_ context.Context, p Payload) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rejectRich && strings.Contains(mustJSON(p), `"plan"`) {
		return Message{}, &SlackError{Method: "chat.postMessage", Code: "invalid_blocks"}
	}
	f.n++
	f.posts = append(f.posts, p)
	return Message{Channel: p.Channel, TS: fmt.Sprintf("100.%d", f.n)}, nil
}

func (f *fakeSlack) Update(_ context.Context, p Payload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, p)
	return nil
}

func (f *fakeSlack) Ephemeral(_ context.Context, _, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ephem = append(f.ephem, text)
	return nil
}

func (f *fakeSlack) PublishHome(context.Context, string, any) error { return nil }
func (f *fakeSlack) OpenView(context.Context, string, any) error    { return nil }

func (f *fakeSlack) topLevel() []Payload {
	var out []Payload
	for _, p := range f.posts {
		if p.ThreadTS == "" {
			out = append(out, p)
		}
	}
	return out
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// fakeCluster serves a mutable set of Applications plus static objects.
type fakeCluster struct {
	mu      sync.Mutex
	apps    map[string]json.RawMessage
	objects map[string]string // path -> JSON
	logs    string
}

func (c *fakeCluster) Get(_ context.Context, path string, q url.Values, out any) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.HasSuffix(path, "/applications") {
		var items []json.RawMessage
		for _, a := range c.apps {
			items = append(items, a)
		}
		b, _ := json.Marshal(map[string]any{"metadata": map[string]string{"resourceVersion": "1"}, "items": items})
		return true, json.Unmarshal(b, out)
	}
	key := path
	if sel := q.Get("labelSelector"); sel != "" {
		key += "?" + sel
	}
	if strings.HasSuffix(path, "/events") {
		key = path
	}
	body, ok := c.objects[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal([]byte(body), out)
}

func (c *fakeCluster) Text(context.Context, string, url.Values, int64) (string, error) {
	return c.logs, nil
}

func (c *fakeCluster) Watch(ctx context.Context, _, rv string, _ time.Duration, _ func(kube.Event)) (string, error) {
	<-ctx.Done()
	return rv, nil
}

func (c *fakeCluster) set(t *testing.T, raw []byte, edit func(map[string]any)) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(m)
	}
	b, _ := json.Marshal(m)
	c.mu.Lock()
	defer c.mu.Unlock()
	name := m["metadata"].(map[string]any)["name"].(string)
	c.apps[name] = b
}

func quietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func init() { metricOut = io.Discard }

func obj(m map[string]any, keys ...string) map[string]any {
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	return cur
}
