package hatchet

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestWorkerSlots(t *testing.T) {
	newShape := worker{}
	newShape.SlotConfig = map[string]struct {
		Available float64 `json:"available"`
		Limit     float64 `json:"limit"`
	}{"default": {Available: 2, Limit: 10}, "heavy": {Available: 1, Limit: 2}}
	if used, max, ok := newShape.slots(); !ok || used != 9 || max != 12 {
		t.Fatalf("slotConfig: used=%v max=%v ok=%v", used, max, ok)
	}
	maxRuns, avail := 5.0, 3.0
	old := worker{MaxRuns: &maxRuns, AvailableRuns: &avail}
	if used, max, ok := old.slots(); !ok || used != 2 || max != 5 {
		t.Fatalf("maxRuns: used=%v max=%v ok=%v", used, max, ok)
	}
	if _, _, ok := (worker{}).slots(); ok {
		t.Fatal("empty worker reported slots")
	}
}

// runsByStatus answers workflow-runs with the rows matching the requested
// statuses.
func runsByStatus(rows map[string][]map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, s := range r.URL.Query()["statuses"] {
			out = append(out, rows[s]...)
		}
		if out == nil {
			out = []map[string]any{}
		}
		jsonOK(map[string]any{"rows": out})(w, r)
	}
}

func TestWorkersLongTaskAndPoolExhausted(t *testing.T) {
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	completed := make([]map[string]any, 0, 12)
	for i := 0; i < 12; i++ {
		completed = append(completed, map[string]any{
			"status": "COMPLETED", "startedAt": ts(time.Duration(i+2) * time.Minute), "finishedAt": ts(time.Duration(i+1) * time.Minute),
		})
	}
	// One task is far slower than the rest so the p95 is visible.
	completed[0]["startedAt"] = ts(30 * time.Minute)
	completed[0]["finishedAt"] = ts(0)
	srv := serve(t, routes{
		tenantPath("/worker"): rawJSON(`{"rows": [
		  {"metadata": {"id": "w1"}, "name": "api-worker-1", "status": "ACTIVE", "slotConfig": {"default": {"available": 0, "limit": 4}}},
		  {"metadata": {"id": "w2"}, "name": "api-worker-2", "status": "ACTIVE", "maxRuns": 2, "availableRuns": 0},
		  {"metadata": {"id": "w3"}, "name": "other-worker", "status": "ACTIVE", "maxRuns": 8, "availableRuns": 8},
		  {"metadata": {"id": "w4"}, "name": "api-worker-old", "status": "INACTIVE", "maxRuns": 2, "availableRuns": 2}
		]}`),
		tenantPath("/queue-metrics"): rawJSON(`{"queues": {"default": 3}, "total": {"numPending": 1, "numQueued": 3, "numRunning": 6}}`),
		stablePath("/workflow-runs"): runsByStatus(map[string][]map[string]any{
			"RUNNING": {
				{"status": "RUNNING", "displayName": "reindex", "taskExternalId": "t-long", "startedAt": ts(25 * time.Minute), "createdAt": ts(26 * time.Minute)},
				{"status": "RUNNING", "displayName": "send", "taskExternalId": "t-short", "startedAt": ts(10 * time.Second)},
			},
			"COMPLETED": completed,
		}),
	})
	p := &WorkersProbe{}
	st, err := p.setup(specFor(t, srv, map[string]any{"name": "api-worker", "long_task": "10m"}))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	want := map[string]float64{
		"workers_online": 2, "workers_total": 3, "pool_used": 6, "pool_max": 6, "active": 2, "waiters": 4,
	}
	for k, v := range want {
		if o.Metrics[k] != v {
			t.Errorf("%s = %v, want %v", k, o.Metrics[k], v)
		}
	}
	if rs := o.Metrics["running_s"]; rs < 1499 || rs > 1505 {
		t.Errorf("running_s = %v", rs)
	}
	if p95 := o.Metrics["p95_s"]; p95 < 1799 || p95 > 1801 {
		t.Errorf("p95_s = %v", p95)
	}
	tr, ok := model.HasCondition(o.Conditions, model.CondTaskRunning)
	if !ok || tr.Ref != "t-long" || tr.Detail != "reindex" || tr.Since.IsZero() {
		t.Fatalf("TaskRunning = %+v (%v)", tr, ok)
	}
	if n := countCond(o.Conditions, model.CondTaskRunning); n != 1 {
		t.Fatalf("%d TaskRunning conditions, want 1", n)
	}
	pe, ok := model.HasCondition(o.Conditions, model.CondPoolExhausted)
	if !ok || pe.Detail != "all 6 slots busy" {
		t.Fatalf("PoolExhausted = %+v (%v)", pe, ok)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondNotReady); ok {
		t.Fatal("NotReady with two active workers")
	}
	long, _ := o.Detail["long_tasks"].([]map[string]any)
	if len(long) != 1 || long[0]["task"] != "reindex" {
		t.Fatalf("long_tasks = %v", long)
	}
	workers, _ := o.Detail["workers"].([]map[string]any)
	if len(workers) != 3 || workers[0]["slots"] != "4/4" {
		t.Fatalf("workers = %v", workers)
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}

	// The PoolExhausted Since is stable across polls.
	since := pe.Since
	o = p.poll(context.Background(), st)
	pe, _ = model.HasCondition(o.Conditions, model.CondPoolExhausted)
	if !pe.Since.Equal(since) {
		t.Fatalf("Since moved from %s to %s", since, pe.Since)
	}
}

func TestWorkersNoneActive(t *testing.T) {
	srv := serve(t, routes{
		tenantPath("/worker"):        rawJSON(`{"rows": [{"metadata": {"id": "w1"}, "name": "w", "status": "INACTIVE", "maxRuns": 2, "availableRuns": 2}]}`),
		tenantPath("/queue-metrics"): rawJSON(`{"queues": {}, "total": {"numPending": 0, "numQueued": 0, "numRunning": 0}}`),
		stablePath("/workflow-runs"): rawJSON(`{"rows": []}`),
	})
	p := &WorkersProbe{}
	st, err := p.setup(specFor(t, srv, nil))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Metrics["workers_online"] != 0 || o.Metrics["workers_total"] != 1 || o.Metrics["active"] != 0 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if _, ok := o.Metrics["pool_max"]; ok {
		t.Fatal("pool_max reported from an inactive worker")
	}
	if _, ok := o.Metrics["p95_s"]; ok {
		t.Fatal("p95_s reported without samples")
	}
	nr, ok := model.HasCondition(o.Conditions, model.CondNotReady)
	if !ok || nr.Detail != "no active Hatchet workers" {
		t.Fatalf("NotReady = %+v (%v)", nr, ok)
	}
}

func TestWorkersPartialRead(t *testing.T) {
	srv := serve(t, routes{
		tenantPath("/worker"): rawJSON(`{"rows": [{"metadata": {"id": "w1"}, "name": "w", "status": "ACTIVE", "maxRuns": 2, "availableRuns": 1}]}`),
		stablePath("/workflow-runs"): func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})
	p := &WorkersProbe{}
	st, _ := p.setup(specFor(t, srv, nil))
	o := p.poll(context.Background(), st)
	if o.Err != "" || o.Metrics["workers_online"] != 1 || o.Metrics["pool_used"] != 1 {
		t.Fatalf("observation = %+v", o)
	}
	for _, k := range []string{"active", "waiters", "running_s", "p95_s"} {
		if _, ok := o.Metrics[k]; ok {
			t.Errorf("%s reported although its read failed", k)
		}
	}
	if h := p.Health(); h.State != probe.HealthDegraded {
		t.Fatalf("health = %+v", h)
	}
}

func countCond(conds []model.Condition, kind string) int {
	n := 0
	for _, c := range conds {
		if c.Kind == kind {
			n++
		}
	}
	return n
}

// The backlog comes from task-stats on a server that has it; the retired
// queue-metrics is not asked.
func TestWorkersBacklogFromTaskStats(t *testing.T) {
	queueMetricsAsked := false
	srv := serve(t, routes{
		tenantPath("/worker"):     rawJSON(`{"rows": [{"metadata": {"id": "w1"}, "name": "w", "status": "ACTIVE", "maxRuns": 2, "availableRuns": 1}]}`),
		tenantPath("/task-stats"): taskStats(time.Now()),
		tenantPath("/queue-metrics"): func(w http.ResponseWriter, _ *http.Request) {
			queueMetricsAsked = true
			http.Error(w, `{"errors":[{"description":"TenantGetQueueMetrics is deprecated"}]}`, http.StatusBadRequest)
		},
		stablePath("/workflow-runs"): rawJSON(`{"rows": []}`),
	})
	p := &WorkersProbe{}
	st, err := p.setup(specFor(t, srv, nil))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err != "" || o.Metrics["waiters"] != 10 || o.Detail["queue_source"] != sourceTaskStats {
		t.Fatalf("metrics = %v, detail = %v", o.Metrics, o.Detail)
	}
	if queueMetricsAsked {
		t.Error("queue-metrics was asked although task-stats answered")
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}
}

// A worker list with nobody in it says nothing about the fleet: the token
// may be another tenant's. It never reads as not ready.
func TestWorkersEmptyListConcludesNothing(t *testing.T) {
	srv := serve(t, routes{
		tenantPath("/worker"):        rawJSON(`{"rows": []}`),
		tenantPath("/task-stats"):    rawJSON(`{}`),
		stablePath("/workflow-runs"): rawJSON(`{"rows": []}`),
	})
	for name, extra := range map[string]map[string]any{"no filter": nil, "a name filter": {"name": "api-worker"}} {
		p := &WorkersProbe{}
		st, err := p.setup(specFor(t, srv, extra))
		if err != nil {
			t.Fatal(err)
		}
		o := p.poll(context.Background(), st)
		if o.Err != "" {
			t.Fatal(o.Err)
		}
		if _, ok := model.HasCondition(o.Conditions, model.CondNotReady); ok {
			t.Errorf("%s: NotReady from an empty worker list", name)
		}
		for _, k := range []string{"workers_online", "workers_total"} {
			if _, ok := o.Metrics[k]; ok {
				t.Errorf("%s: %s reported from an empty worker list", name, k)
			}
		}
		note, _ := o.Detail["workers_note"].(string)
		if !strings.Contains(note, "Hatchet lists no worker") || !strings.Contains(note, "another tenant than the workers'") {
			t.Errorf("%s: workers_note = %q", name, note)
		}
		if o.Metrics["waiters"] != 0 || o.Metrics["active"] != 0 {
			t.Errorf("%s: metrics = %v", name, o.Metrics)
		}
	}
}
