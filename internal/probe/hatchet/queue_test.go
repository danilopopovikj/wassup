package hatchet

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

const newQueueMetrics = `{
  "queues": {"default": 7, "emails": 3, "reports": 0},
  "total": {"numPending": 2, "numQueued": 10, "numRunning": 4},
  "workflow": {"send-email": {"numPending": 1, "numQueued": 3, "numRunning": 2}}
}`

const workerRows = `{"rows": [
  {"metadata": {"id": "w1"}, "name": "api-worker-1", "status": "ACTIVE", "actions": ["send-email:render", "send-email:deliver"],
   "slotConfig": {"default": {"available": 2, "limit": 10}}},
  {"metadata": {"id": "w2"}, "name": "api-worker-2", "status": "INACTIVE", "actions": ["send-email:render"], "maxRuns": 5, "availableRuns": 5},
  {"metadata": {"id": "w3"}, "name": "cron-worker", "status": "ACTIVE", "actions": ["reports:build"], "maxRuns": 4, "availableRuns": 1}
]}`

func queuedRuns(now time.Time) http.HandlerFunc {
	return jsonOK(map[string]any{"rows": []map[string]any{
		{"status": "QUEUED", "displayName": "render", "createdAt": now.Add(-90 * time.Second).Format(time.RFC3339Nano), "taskExternalId": "t1"},
		{"status": "QUEUED", "displayName": "deliver", "createdAt": now.Add(-30 * time.Second).Format(time.RFC3339Nano), "taskExternalId": "t2"},
	}})
}

func TestQueueTotalNewShape(t *testing.T) {
	now := time.Now()
	var runQuery string
	srv := serve(t, routes{
		tenantPath("/queue-metrics"): rawJSON(newQueueMetrics),
		tenantPath("/worker"):        rawJSON(workerRows),
		stablePath("/workflow-runs"): func(w http.ResponseWriter, r *http.Request) {
			runQuery = r.URL.RawQuery
			queuedRuns(now)(w, r)
		},
	})
	p := &QueueProbe{}
	st, err := p.setup(specFor(t, srv, nil))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	want := map[string]float64{"depth": 12, "pending": 2, "running": 4, "active": 4, "consumers": 2}
	for k, v := range want {
		if o.Metrics[k] != v {
			t.Errorf("%s = %v, want %v", k, o.Metrics[k], v)
		}
	}
	if age := o.Metrics["oldest_age_s"]; age < 89 || age > 95 {
		t.Errorf("oldest_age_s = %v", age)
	}
	if _, ok := o.Metrics["growth_per_min"]; ok {
		t.Error("growth_per_min reported from a single reading")
	}
	for _, part := range []string{"only_tasks=true", "statuses=QUEUED", "limit=200", "since="} {
		if !strings.Contains(runQuery, part) {
			t.Errorf("runs query %q lacks %s", runQuery, part)
		}
	}
	queues, _ := o.Detail["queues"].([]map[string]any)
	if len(queues) != 3 || queues[0]["name"] != "default" {
		t.Errorf("queues = %v", queues)
	}
	if o.Detail["legacy"] != false {
		t.Errorf("legacy = %v", o.Detail["legacy"])
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}

	// A second reading a minute later yields growth.
	st.ring.push(now.Add(time.Minute), 24)
	if g, ok := st.ring.growth(); !ok || g < 11.9 || g > 12.1 {
		t.Fatalf("growth = %v, %v", g, ok)
	}
}

func TestQueueFilters(t *testing.T) {
	now := time.Now()
	srv := serve(t, routes{
		tenantPath("/queue-metrics"): rawJSON(newQueueMetrics),
		tenantPath("/worker"):        rawJSON(workerRows),
		tenantPath("/workflows"):     rawJSON(`{"rows":[{"name":"send-email","metadata":{"id":"wf-1"}}],"pagination":{}}`),
		stablePath("/workflow-runs"): queuedRuns(now),
	})

	p := &QueueProbe{}
	st, err := p.setup(specFor(t, srv, map[string]any{"queue": "emails"}))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Metrics["depth"] != 3 || o.Metrics["consumers"] != 0 {
		t.Fatalf("queue filter metrics = %v", o.Metrics)
	}
	if _, ok := o.Metrics["pending"]; ok {
		t.Fatal("pending fabricated for a queue filter")
	}
	if o.Detail["listed"] != true || o.Detail["queue"] != "emails" {
		t.Fatalf("detail = %v", o.Detail)
	}

	p = &QueueProbe{}
	st, err = p.setup(specFor(t, srv, map[string]any{"workflow": "send-email"}))
	if err != nil {
		t.Fatal(err)
	}
	o = p.poll(context.Background(), st)
	if o.Metrics["depth"] != 4 || o.Metrics["pending"] != 1 || o.Metrics["running"] != 2 || o.Metrics["consumers"] != 1 {
		t.Fatalf("workflow filter metrics = %v", o.Metrics)
	}
	if o.Detail["oldest_scope"] != "workflow" {
		t.Fatalf("oldest_scope = %v", o.Detail["oldest_scope"])
	}
}

func TestQueueStepRunFallback(t *testing.T) {
	srv := serve(t, routes{
		tenantPath("/step-run-queue-metrics"): rawJSON(`{"queues": {"default": 5, "emails": 2}}`),
		tenantPath("/worker"):                 rawJSON(workerRows),
		stablePath("/workflow-runs"):          rawJSON(`{"rows": []}`),
	})
	p := &QueueProbe{}
	st, err := p.setup(specFor(t, srv, nil))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if o.Metrics["depth"] != 7 || o.Metrics["consumers"] != 2 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	for _, k := range []string{"pending", "running", "active", "oldest_age_s"} {
		if _, ok := o.Metrics[k]; ok {
			t.Errorf("%s fabricated on a legacy server", k)
		}
	}
	if o.Detail["legacy"] != true {
		t.Fatalf("legacy = %v", o.Detail["legacy"])
	}

	// A workflow filter cannot be served by the legacy endpoint.
	p = &QueueProbe{}
	st, _ = p.setup(specFor(t, srv, map[string]any{"workflow": "send-email"}))
	o = p.poll(context.Background(), st)
	if o.Err == "" || p.Health().State != probe.HealthDegraded {
		t.Fatalf("legacy workflow filter: err=%q health=%+v", o.Err, p.Health())
	}
}

func TestQueueTokenRejected(t *testing.T) {
	srv := serve(t, routes{})
	spec := specFor(t, srv, map[string]any{"tenant": testTenant})
	t.Setenv("WASSUP_TEST_HATCHET_TOKEN", "wrong")
	p := &QueueProbe{}
	st, err := p.setup(spec)
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err == "" || o.Metrics != nil {
		t.Fatalf("observation = %+v", o)
	}
	if h := p.Health(); h.State != probe.HealthFailed || !strings.Contains(h.Message, "token rejected") {
		t.Fatalf("health = %+v", h)
	}
}

func TestQueueServerDown(t *testing.T) {
	srv := serve(t, routes{})
	spec := specFor(t, srv, nil)
	srv.Close()
	p := &QueueProbe{}
	st, err := p.setup(spec)
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err == "" {
		t.Fatal("expected an error observation")
	}
	if h := p.Health(); h.State != probe.HealthDegraded {
		t.Fatalf("health = %+v", h)
	}
}
