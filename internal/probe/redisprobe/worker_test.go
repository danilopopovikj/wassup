package redisprobe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// fakeFlower serves /api/workers (with and without status=true) and
// /api/tasks from the given maps.
type fakeFlower struct {
	workers map[string]any
	status  map[string]bool
	tasks   map[string]any
}

func (f *fakeFlower) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("status") == "true" {
			_ = json.NewEncoder(w).Encode(f.status)
			return
		}
		_ = json.NewEncoder(w).Encode(f.workers)
	})
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.tasks)
	})
	return mux
}

var workerClock = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func flowerWorker(concurrency int, queues []string, active ...map[string]any) map[string]any {
	qs := make([]map[string]any, 0, len(queues))
	for _, q := range queues {
		qs = append(qs, map[string]any{"name": q})
	}
	if active == nil {
		active = []map[string]any{}
	}
	return map[string]any{
		"active":        active,
		"active_queues": qs,
		"stats": map[string]any{
			"pool":  map[string]any{"max-concurrency": concurrency, "processes": []int{1, 2}},
			"total": map[string]any{"mail.send": 42},
		},
	}
}

func flowerActive(id, name string, started time.Time) map[string]any {
	return map[string]any{"id": id, "name": name, "time_start": float64(started.Unix()), "args": "()"}
}

func newWorkerProbe() *WorkerProbe {
	return &WorkerProbe{now: func() time.Time { return workerClock }, client: &http.Client{Timeout: 2 * time.Second}}
}

func workerCfg(flowerURL, name string) workerConfig {
	return workerConfig{target: "workers", flower: flowerURL, name: name, longTask: 10 * time.Minute, tick: time.Second, interval: 5 * time.Second, timeout: 2 * time.Second}
}

func TestWorkerTwoWorkersOneOffline(t *testing.T) {
	tasks := map[string]any{}
	for i := 0; i < 20; i++ {
		tasks[string(rune('a'+i))] = map[string]any{"name": "mail.send", "runtime": float64(i + 1), "succeeded": 1790000000.0, "state": "SUCCESS"}
	}
	f := &fakeFlower{
		workers: map[string]any{
			"celery@worker-1": flowerWorker(4, []string{"default", "exports"},
				flowerActive("t1", "mail.send", workerClock.Add(-30*time.Second)),
				flowerActive("t2", "reports.tasks.export_csv", workerClock.Add(-2*time.Minute))),
			"celery@worker-2": flowerWorker(8, []string{"default"}, flowerActive("t3", "mail.send", workerClock.Add(-time.Hour))),
		},
		status: map[string]bool{"celery@worker-1": true, "celery@worker-2": false},
		tasks:  tasks,
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newWorkerProbe()
	o := p.round(context.Background(), &flowerClient{base: srv.URL, http: p.client}, workerCfg(srv.URL, ""), firstSeen{})
	if o.Err != "" {
		t.Fatalf("err = %q", o.Err)
	}
	if o.Target != "workers" || o.Probe != "celery.worker" || !o.At.Equal(workerClock) {
		t.Fatalf("target/probe/at = %q/%q/%v", o.Target, o.Probe, o.At)
	}
	want := map[string]float64{
		"workers_online": 1, "workers_total": 2, "active": 2, "pool_used": 2, "pool_max": 4,
		"running_s": 120, "queues": 2, "p95_s": 19,
	}
	for k, v := range want {
		if o.Metrics[k] != v {
			t.Errorf("metric %s = %v, want %v (all: %v)", k, o.Metrics[k], v, o.Metrics)
		}
	}
	// The offline worker's hour-old task is not counted and raises nothing.
	if len(o.Conditions) != 0 {
		t.Fatalf("unexpected conditions %+v", o.Conditions)
	}
	ws, _ := o.Detail["workers"].([]map[string]any)
	if len(ws) != 2 || ws[0]["name"] != "celery@worker-1" || ws[0]["online"] != true || ws[0]["concurrency"] != 4 || ws[0]["active"] != 2 || ws[1]["online"] != false {
		t.Fatalf("workers detail = %v", o.Detail["workers"])
	}
	qs, _ := o.Detail["queues"].([]string)
	if len(qs) != 2 || qs[0] != "default" || qs[1] != "exports" {
		t.Fatalf("queues detail = %v", o.Detail["queues"])
	}
	if p.Health().State != probe.HealthOK {
		t.Fatalf("health = %+v", p.Health())
	}
}

func TestWorkerLongTask(t *testing.T) {
	started := workerClock.Add(-25 * time.Minute)
	f := &fakeFlower{
		workers: map[string]any{
			"celery@worker-1": flowerWorker(4, []string{"default"},
				flowerActive("long-1", "reports.tasks.export_csv", started),
				flowerActive("short-1", "mail.send", workerClock.Add(-5*time.Second))),
		},
		status: map[string]bool{"celery@worker-1": true},
		tasks:  map[string]any{"x": map[string]any{"name": "mail.send", "runtime": 1.0}},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newWorkerProbe()
	seen := firstSeen{}
	o := p.round(context.Background(), &flowerClient{base: srv.URL, http: p.client}, workerCfg(srv.URL, ""), seen)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if len(o.Conditions) != 1 {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
	c := o.Conditions[0]
	if c.Kind != model.CondTaskRunning || c.Ref != "long-1" || c.Detail != "reports.tasks.export_csv" || !c.Since.Equal(started) {
		t.Fatalf("condition = %+v", c)
	}
	if o.Metrics["running_s"] != 1500 || o.Metrics["active"] != 2 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if _, ok := o.Metrics["p95_s"]; ok {
		t.Fatalf("p95_s needs at least %d runtimes: %v", workerP95Min, o.Metrics)
	}
	lt, _ := o.Detail["long_tasks"].([]map[string]any)
	if len(lt) != 1 || lt[0]["id"] != "long-1" || lt[0]["worker"] != "celery@worker-1" || lt[0]["running_s"] != 1500.0 {
		t.Fatalf("long_tasks detail = %v", o.Detail["long_tasks"])
	}

	// More than workerMaxLongTasks long tasks: conditions are capped, the
	// oldest first, and Detail lists them all.
	var many []map[string]any
	for i := 0; i < 8; i++ {
		many = append(many, flowerActive(string(rune('a'+i)), "slow", workerClock.Add(-time.Duration(11+i)*time.Minute)))
	}
	f.workers["celery@worker-1"] = flowerWorker(16, []string{"default"}, many...)
	o = p.round(context.Background(), &flowerClient{base: srv.URL, http: p.client}, workerCfg(srv.URL, ""), seen)
	if len(o.Conditions) != workerMaxLongTasks {
		t.Fatalf("conditions = %d, want %d", len(o.Conditions), workerMaxLongTasks)
	}
	if o.Conditions[0].Ref != "h" || o.Conditions[workerMaxLongTasks-1].Ref != "d" {
		t.Fatalf("conditions should be oldest first: %+v", o.Conditions)
	}
	if lt, _ := o.Detail["long_tasks"].([]map[string]any); len(lt) != 8 {
		t.Fatalf("long_tasks detail = %d entries", len(lt))
	}
}

func TestWorkerNoneOnline(t *testing.T) {
	f := &fakeFlower{
		workers: map[string]any{"celery@worker-1": flowerWorker(4, []string{"default"})},
		status:  map[string]bool{"celery@worker-1": false},
		tasks:   map[string]any{},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newWorkerProbe()
	seen := firstSeen{}
	cfg := workerCfg(srv.URL, "")
	fc := &flowerClient{base: srv.URL, http: p.client}
	o := p.round(context.Background(), fc, cfg, seen)
	if o.Err != "" || o.Metrics["workers_online"] != 0 || o.Metrics["workers_total"] != 1 {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	c, ok := model.HasCondition(o.Conditions, model.CondNotReady)
	if !ok || c.Detail != "no Celery workers online" || !c.Since.Equal(workerClock) {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
	if _, ok := o.Metrics["running_s"]; ok {
		t.Fatalf("running_s without active tasks: %v", o.Metrics)
	}
	// Since holds across polls while nothing is online, then clears.
	p.now = func() time.Time { return workerClock.Add(time.Minute) }
	o = p.round(context.Background(), fc, cfg, seen)
	if c, ok := model.HasCondition(o.Conditions, model.CondNotReady); !ok || !c.Since.Equal(workerClock) {
		t.Fatalf("second poll conditions = %+v", o.Conditions)
	}
	f.status["celery@worker-1"] = true
	o = p.round(context.Background(), fc, cfg, seen)
	if len(o.Conditions) != 0 || len(seen) != 0 {
		t.Fatalf("conditions=%+v seen=%v", o.Conditions, seen)
	}

	// Flower itself unreachable: Err observation, degraded health.
	srv.Close()
	o = p.round(context.Background(), fc, cfg, seen)
	if o.Err == "" || o.Metrics != nil || p.Health().State != probe.HealthDegraded {
		t.Fatalf("err=%q metrics=%v health=%+v", o.Err, o.Metrics, p.Health())
	}
}

func TestWorkerNameFilter(t *testing.T) {
	f := &fakeFlower{
		workers: map[string]any{
			"celery@api-worker-1":    flowerWorker(4, []string{"default"}, flowerActive("a", "x", workerClock.Add(-time.Second))),
			"celery@api-worker-2":    flowerWorker(4, []string{"default"}),
			"celery@report-worker-1": flowerWorker(2, []string{"exports"}, flowerActive("b", "y", workerClock.Add(-time.Hour))),
		},
		status: map[string]bool{"celery@api-worker-1": true, "celery@api-worker-2": true, "celery@report-worker-1": true},
		tasks:  map[string]any{},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newWorkerProbe()
	fc := &flowerClient{base: srv.URL, http: p.client}
	o := p.round(context.Background(), fc, workerCfg(srv.URL, "api-worker"), firstSeen{})
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if o.Metrics["workers_total"] != 2 || o.Metrics["workers_online"] != 2 || o.Metrics["pool_max"] != 8 || o.Metrics["active"] != 1 || o.Metrics["queues"] != 1 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if len(o.Conditions) != 0 {
		t.Fatalf("report worker's long task must be filtered out: %+v", o.Conditions)
	}
	// The full name works as a prefix too.
	o = p.round(context.Background(), fc, workerCfg(srv.URL, "celery@report"), firstSeen{})
	if o.Metrics["workers_total"] != 1 || o.Metrics["pool_max"] != 2 || len(o.Conditions) != 1 {
		t.Fatalf("metrics=%v conditions=%+v", o.Metrics, o.Conditions)
	}
	// A prefix that matches nothing leaves no workers and NotReady.
	o = p.round(context.Background(), fc, workerCfg(srv.URL, "nope"), firstSeen{})
	if o.Metrics["workers_total"] != 0 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondNotReady); !ok {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
}

func TestWorkerValidateAndStart(t *testing.T) {
	p := &WorkerProbe{}
	for _, bad := range []map[string]any{
		{},
		{"flower_url": "flower:5555"},
		{"flower_url": "http://flower:5555", "long_task": "forever"},
		{"flower_url": "http://flower:5555", "name": 1},
	} {
		if err := p.Validate(bad); err == nil {
			t.Errorf("spec %v should fail validation", bad)
		}
	}
	spec := map[string]any{"flower_url": "http://flower.bookstore:5555/", "name": "worker", "long_task": "30m", "interval": "1s", "_target": "workers", "_tick": 2 * time.Second}
	if err := p.Validate(spec); err != nil {
		t.Fatal(err)
	}
	c := parseWorkerSpec(spec)
	if c.flower != "http://flower.bookstore:5555" || c.longTask != 30*time.Minute || c.interval != workerMinInterval || c.tick != 2*time.Second || c.timeout != roundTimeout {
		t.Fatalf("config = %+v", c)
	}

	f := &fakeFlower{
		workers: map[string]any{"celery@w": flowerWorker(4, []string{"default"})},
		status:  map[string]bool{"celery@w": true},
		tasks:   map[string]any{},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 4)
	if err := p.Start(ctx, map[string]any{"flower_url": srv.URL, "_target": "workers", "_tick": 20 * time.Millisecond}, out); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-out:
		if o.Target != "workers" || o.Probe != "celery.worker" || o.Metrics["workers_online"] != 1 {
			t.Fatalf("observation = %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no observation from Start")
	}
	a, ok := probe.AccessFor("celery.worker")
	if !ok || !a.Implemented {
		t.Fatalf("access = %+v %v", a, ok)
	}
}
