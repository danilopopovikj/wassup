package electric

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// newSync returns a probe with a fixed clock and short client timeouts.
func newSync() (*Sync, time.Time) {
	clock := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	p := &Sync{
		now:    func() time.Time { return clock },
		client: newClient(2 * time.Second),
		live:   newClient(300 * time.Millisecond),
	}
	return p, clock
}

func cfg(url, table string) config {
	return config{target: "electric", url: url, table: table, tick: time.Second, interval: 5 * time.Second, timeout: 2 * time.Second}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// electricServer fakes the health and shape endpoints of an active Electric.
type electricServer struct {
	secret   atomic.Value // string
	shapes   atomic.Int32
	lives    atomic.Int32
	liveWait time.Duration
}

func (e *electricServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
	})
	mux.HandleFunc("/v1/shape", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		e.secret.Store(q.Get("secret"))
		if q.Get("table") != "public.issues" {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": `Table "public.nope" does not exist`})
			return
		}
		if q.Get("live") == "true" {
			e.lives.Add(1)
			if e.liveWait > 0 {
				time.Sleep(e.liveWait)
			}
			w.Header().Set(electricHandle, q.Get("handle"))
			w.Header().Set(electricOffset, q.Get("offset"))
			w.Header().Set(electricUpToDte, "true")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		e.shapes.Add(1)
		w.Header().Set(electricHandle, "3833821-1721812114261")
		w.Header().Set(electricOffset, "0_0")
		w.Header().Set(electricSchema, `{"id":{"type":"uuid","pk_index":0},"title":{"type":"text"},"priority":{"type":"int4"}}`)
		writeJSON(w, http.StatusOK, []map[string]any{{"key": `"public"."issues"/"1"`, "value": map[string]any{"id": "1"}, "headers": map[string]any{"operation": "insert"}}})
	})
	return mux
}

func TestSyncActiveWithShape(t *testing.T) {
	e := &electricServer{}
	srv := httptest.NewServer(e.handler())
	defer srv.Close()

	p, clock := newSync()
	o := p.poll(context.Background(), cfg(srv.URL, "public.issues"))

	if o.Err != "" {
		t.Fatalf("unexpected err %q", o.Err)
	}
	if o.Target != "electric" || o.Probe != KindSync || !o.At.Equal(clock) {
		t.Fatalf("target/probe/at = %q/%q/%v", o.Target, o.Probe, o.At)
	}
	if o.Metrics["ready"] != 1 || o.Metrics["up_to_date"] != 1 || o.Metrics["columns"] != 3 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if _, ok := o.Metrics["latency_ms"]; !ok {
		t.Fatalf("latency_ms missing: %v", o.Metrics)
	}
	if _, ok := o.Metrics["shape_ms"]; !ok {
		t.Fatalf("shape_ms missing: %v", o.Metrics)
	}
	if _, ok := o.Metrics["busy"]; ok {
		t.Fatalf("busy must not be set on a healthy shape: %v", o.Metrics)
	}
	if len(o.Conditions) != 0 {
		t.Fatalf("unexpected conditions %v", o.Conditions)
	}
	if o.Detail["status"] != "active" || o.Detail["handle"] != "3833821-1721812114261" || o.Detail["offset"] != "0_0" || o.Detail["table"] != "public.issues" || o.Detail["url"] != srv.URL {
		t.Fatalf("detail = %v", o.Detail)
	}
	names, _ := o.Detail["schema"].([]string)
	if len(names) != 3 || names[0] != "id" || names[1] != "priority" || names[2] != "title" {
		t.Fatalf("schema = %v", o.Detail["schema"])
	}
	if e.shapes.Load() != 1 || e.lives.Load() != 1 {
		t.Fatalf("shape/live requests = %d/%d", e.shapes.Load(), e.lives.Load())
	}
	if s, _ := e.secret.Load().(string); s != "" {
		t.Fatalf("secret sent without secret_env: %q", s)
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}
}

func TestSyncLiveTimeoutIsUpToDate(t *testing.T) {
	e := &electricServer{liveWait: 2 * time.Second}
	srv := httptest.NewServer(e.handler())
	defer srv.Close()

	p, _ := newSync()
	o := p.poll(context.Background(), cfg(srv.URL, "public.issues"))
	if o.Err != "" || o.Metrics["up_to_date"] != 1 {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	if p.Health().State != probe.HealthOK {
		t.Fatalf("health = %+v", p.Health())
	}
}

func TestSyncWaiting(t *testing.T) {
	var status atomic.Value
	status.Store("waiting")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": status.Load().(string)})
	}))
	defer srv.Close()

	p, clock := newSync()
	o := p.poll(context.Background(), cfg(srv.URL, ""))
	if o.Err != "" || o.Metrics["ready"] != 0 {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	c, ok := model.HasCondition(o.Conditions, model.CondNotReady)
	if !ok || c.Detail != detailWaiting || !c.Since.Equal(clock) {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
	if _, ok := o.Metrics["shape_ms"]; ok {
		t.Fatalf("no table, no shape metrics: %v", o.Metrics)
	}
	if p.Health().State != probe.HealthDegraded {
		t.Fatalf("health = %+v", p.Health())
	}

	// Since is stable across polls while still not ready.
	p.now = func() time.Time { return clock.Add(time.Minute) }
	status.Store("starting")
	o = p.poll(context.Background(), cfg(srv.URL, ""))
	c, ok = model.HasCondition(o.Conditions, model.CondNotReady)
	if !ok || c.Detail != detailStarting || !c.Since.Equal(clock) {
		t.Fatalf("second poll conditions = %+v", o.Conditions)
	}
}

func TestSyncTableNotFound(t *testing.T) {
	e := &electricServer{}
	srv := httptest.NewServer(e.handler())
	defer srv.Close()

	p, clock := newSync()
	o := p.poll(context.Background(), cfg(srv.URL, "public.nope"))
	// A shape that cannot be served is a sync engine that is not ready: the
	// facet writes ready=0 and NotReady on the component, with the table in
	// the detail, and no shape latency for a request that served nothing.
	if o.Err != "" || o.Metrics["ready"] != 0 {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	c, ok := model.HasCondition(o.Conditions, model.CondNotReady)
	if !ok || c.Ref != "electric" || !c.Since.Equal(clock) {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
	if c.Detail != `shape public.nope: HTTP 404 Not Found: Table "public.nope" does not exist` {
		t.Fatalf("detail = %q", c.Detail)
	}
	if _, ok := o.Metrics["shape_ms"]; ok {
		t.Fatalf("no shape served, no shape latency: %v", o.Metrics)
	}
	if _, ok := o.Metrics["up_to_date"]; ok {
		t.Fatalf("up_to_date must not be reported without a shape: %v", o.Metrics)
	}
	if e.lives.Load() != 0 {
		t.Fatalf("live poll attempted after 404")
	}
	if p.Health().State != probe.HealthDegraded {
		t.Fatalf("health = %+v", p.Health())
	}
}

func TestSyncBusy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
	})
	mux.HandleFunc("/v1/shape", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"message": "Could not acquire a snapshot"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, _ := newSync()
	o := p.poll(context.Background(), cfg(srv.URL, "public.issues"))
	if o.Err != "" || o.Metrics["busy"] != 1 || o.Metrics["ready"] != 1 {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondNotReady); ok {
		t.Fatalf("429 is busy, not NotReady: %+v", o.Conditions)
	}
	if note, _ := o.Detail["shape_error"].(string); note == "" {
		t.Fatalf("detail should note the throttle: %v", o.Detail)
	}
	if p.Health().State != probe.HealthDegraded {
		t.Fatalf("health = %+v", p.Health())
	}
}

func TestSyncSecretForwarded(t *testing.T) {
	e := &electricServer{}
	srv := httptest.NewServer(e.handler())
	defer srv.Close()

	t.Setenv("ELECTRIC_TEST_SECRET", "s3cret")
	spec := map[string]any{"url": srv.URL, "table": "public.issues", "secret_env": "ELECTRIC_TEST_SECRET", "_target": "electric", "_tick": 5 * time.Second}
	c, err := parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	if c.secret != "s3cret" || c.interval != 5*time.Second || c.timeout != defaultTimeout {
		t.Fatalf("config = %+v", c)
	}
	p, _ := newSync()
	o := p.poll(context.Background(), c)
	if o.Err != "" || o.Metrics["up_to_date"] != 1 {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	if s, _ := e.secret.Load().(string); s != "s3cret" {
		t.Fatalf("secret query param = %q", s)
	}

	// A missing secret env is a misconfiguration, caught before polling.
	if _, err := parse(map[string]any{"url": srv.URL, "secret_env": "ELECTRIC_TEST_MISSING"}); err == nil {
		t.Fatal("missing secret_env should fail")
	}
	// Rejected secret fails the probe.
	rej := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rej.Close()
	o = p.poll(context.Background(), cfg(rej.URL, ""))
	if o.Err == "" || p.Health().State != probe.HealthFailed {
		t.Fatalf("err=%q health=%+v", o.Err, p.Health())
	}
}

func TestSyncConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close()

	p, clock := newSync()
	o := p.poll(context.Background(), cfg(dead, "public.issues"))
	if o.Err == "" || o.Metrics != nil {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	// ConnectionRefused sits on the component, like every facet condition;
	// the URL is in the detail.
	c, ok := model.HasCondition(o.Conditions, model.CondConnectionRefused)
	if !ok || !c.Since.Equal(clock) || c.Ref != "electric" || !strings.Contains(c.Detail, dead) {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
	if p.Health().State != probe.HealthDegraded {
		t.Fatalf("health = %+v", p.Health())
	}
	// Since is stable while the outage lasts.
	p.now = func() time.Time { return clock.Add(30 * time.Second) }
	o = p.poll(context.Background(), cfg(dead, "public.issues"))
	if c, ok := model.HasCondition(o.Conditions, model.CondConnectionRefused); !ok || !c.Since.Equal(clock) {
		t.Fatalf("second poll conditions = %+v", o.Conditions)
	}
}

func TestSyncValidateAndStart(t *testing.T) {
	p := &Sync{}
	for _, bad := range []map[string]any{
		{},
		{"url": "electric:3000"},
		{"url": "ftp://x"},
		{"url": "http://x", "timeout": "soon"},
		{"url": "http://x", "table": 3},
	} {
		if err := p.Validate(bad); err == nil {
			t.Errorf("spec %v should fail validation", bad)
		}
	}
	if err := p.Validate(map[string]any{"url": "http://electric.bookstore:3000", "table": "issues", "interval": "30s"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer((&electricServer{}).handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 4)
	if err := p.Start(ctx, map[string]any{"url": srv.URL, "_target": "electric", "_tick": 20 * time.Millisecond}, out); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-out:
		if o.Target != "electric" || o.Probe != KindSync || o.Metrics["ready"] != 1 {
			t.Fatalf("observation = %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no observation from Start")
	}
	if probe.Known(KindSync) == false {
		t.Fatal("kind not registered")
	}
	a, _ := probe.AccessFor(KindSync)
	if !a.Implemented {
		t.Fatal("access must say implemented")
	}
}
