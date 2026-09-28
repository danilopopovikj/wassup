package amqp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// newQueue returns a probe with a fixed clock and a short client timeout.
func newQueue() (*Queue, time.Time) {
	clock := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	p := &Queue{now: func() time.Time { return clock }, client: newClient(2 * time.Second)}
	return p, clock
}

func cfg(base, queue string) config {
	return config{target: "jobs", base: base, queue: queue, vhost: "/", user: "guest", password: "secret", auth: true, tick: time.Second, interval: time.Second, timeout: 2 * time.Second}
}

// queueJSON is a trimmed real management API response.
func queueJSON(ready int64) map[string]any {
	return map[string]any{
		"name": "jobs", "vhost": "/", "state": "running", "node": "rabbit@rabbitmq-0", "durable": true,
		"memory": 34688, "messages": ready + 3, "messages_ready": ready, "messages_unacknowledged": 3, "consumers": 2,
		"head_message_timestamp": time.Date(2026, 9, 28, 11, 58, 0, 0, time.UTC).Unix(),
		"message_stats": map[string]any{
			"publish_details":     map[string]any{"rate": 12.5},
			"ack_details":         map[string]any{"rate": 11.0},
			"deliver_get_details": map[string]any{"rate": 11.8},
		},
	}
}

func TestQueueNormal(t *testing.T) {
	var auth, rawPath, accept atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		rawPath.Store(r.URL.EscapedPath())
		accept.Store(r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(queueJSON(40))
	}))
	defer srv.Close()

	p, clock := newQueue()
	o := p.poll(context.Background(), cfg(srv.URL, "jobs"))
	if o.Err != "" {
		t.Fatalf("err = %q", o.Err)
	}
	if o.Target != "jobs" || o.Probe != KindQueue || !o.At.Equal(clock) {
		t.Fatalf("target/probe/at = %q/%q/%v", o.Target, o.Probe, o.At)
	}
	want := map[string]float64{"depth": 40, "unacked": 3, "consumers": 2, "rate": 11.8, "publish_rate": 12.5, "oldest_age_s": 120}
	for k, v := range want {
		if o.Metrics[k] != v {
			t.Errorf("metric %s = %v, want %v", k, o.Metrics[k], v)
		}
	}
	if _, ok := o.Metrics["growth_per_min"]; ok {
		t.Errorf("growth needs two readings: %v", o.Metrics)
	}
	if o.Detail["state"] != "running" || o.Detail["node"] != "rabbit@rabbitmq-0" || o.Detail["memory"] != int64(34688) || o.Detail["vhost"] != "/" {
		t.Errorf("detail = %v", o.Detail)
	}
	if o.Detail["url"] != srv.URL+"/api/queues/%2F/jobs" {
		t.Errorf("detail url = %v", o.Detail["url"])
	}
	// Basic auth for guest:secret.
	if a, _ := auth.Load().(string); a != "Basic Z3Vlc3Q6c2VjcmV0" {
		t.Errorf("authorization = %q", a)
	}
	if a, _ := accept.Load().(string); a != "application/json" {
		t.Errorf("accept = %q", a)
	}
	if rp, _ := rawPath.Load().(string); rp != "/api/queues/%2F/jobs" {
		t.Errorf("escaped path = %q", rp)
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}
}

func TestQueueAckRateFallbackAndSparseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "jobs", "messages_ready": 0, "messages_unacknowledged": 0, "consumers": 0,
			"message_stats": map[string]any{"ack_details": map[string]any{"rate": 0.5}},
		})
	}))
	defer srv.Close()
	p, _ := newQueue()
	o := p.poll(context.Background(), cfg(srv.URL, "jobs"))
	if o.Err != "" || o.Metrics["rate"] != 0.5 || o.Metrics["depth"] != 0 {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	for _, k := range []string{"publish_rate", "oldest_age_s"} {
		if _, ok := o.Metrics[k]; ok {
			t.Errorf("%s must not be fabricated: %v", k, o.Metrics)
		}
	}
}

func TestQueueMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Object Not Found","reason":"Not Found"}`))
	}))
	defer srv.Close()
	p, _ := newQueue()
	o := p.poll(context.Background(), cfg(srv.URL, "ghost"))
	if o.Err != `queue "ghost" does not exist in vhost "/" (HTTP 404)` {
		t.Fatalf("err = %q", o.Err)
	}
	if o.Metrics != nil {
		t.Fatalf("metrics must be absent: %v", o.Metrics)
	}
	if h := p.Health(); h.State != probe.HealthFailed {
		t.Fatalf("health = %+v", h)
	}
}

func TestQueueAuthRejectedAndServerError(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusUnauthorized)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	p, _ := newQueue()
	if o := p.poll(context.Background(), cfg(srv.URL, "jobs")); o.Err == "" || p.Health().State != probe.HealthFailed {
		t.Fatalf("401: err=%q health=%+v", o.Err, p.Health())
	}
	status.Store(http.StatusInternalServerError)
	if o := p.poll(context.Background(), cfg(srv.URL, "jobs")); o.Err == "" || p.Health().State != probe.HealthDegraded {
		t.Fatalf("500: err=%q health=%+v", o.Err, p.Health())
	}
	srv.Close()
	if o := p.poll(context.Background(), cfg(srv.URL, "jobs")); o.Err == "" || p.Health().State != probe.HealthDegraded {
		t.Fatalf("refused: err=%q health=%+v", o.Err, p.Health())
	}
}

func TestQueueGrowth(t *testing.T) {
	var ready atomic.Int64
	ready.Store(100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(queueJSON(ready.Load()))
	}))
	defer srv.Close()

	p, clock := newQueue()
	c := cfg(srv.URL, "jobs")
	if o := p.poll(context.Background(), c); o.Err != "" {
		t.Fatal(o.Err)
	}
	ready.Store(130)
	p.now = func() time.Time { return clock.Add(30 * time.Second) }
	o := p.poll(context.Background(), c)
	if o.Err != "" || o.Metrics["depth"] != 130 {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	if g := o.Metrics["growth_per_min"]; g != 60 {
		t.Fatalf("growth_per_min = %v, want 60", g)
	}
}

func TestQueueVhostEncoding(t *testing.T) {
	var rawPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawPath.Store(r.URL.EscapedPath())
		_ = json.NewEncoder(w).Encode(queueJSON(1))
	}))
	defer srv.Close()

	p, _ := newQueue()
	c := cfg(srv.URL, "email/outbound")
	c.vhost = "book store"
	if o := p.poll(context.Background(), c); o.Err != "" {
		t.Fatal(o.Err)
	}
	if rp, _ := rawPath.Load().(string); rp != "/api/queues/book%20store/email%2Foutbound" {
		t.Fatalf("escaped path = %q", rp)
	}
	c.vhost = "/"
	if got := c.requestURL(); got != srv.URL+"/api/queues/%2F/email%2Foutbound" {
		t.Fatalf("requestURL = %q", got)
	}
}

func TestQueueParseAndValidate(t *testing.T) {
	p := &Queue{}
	for _, bad := range []map[string]any{
		{},
		{"management_url": "http://rabbit:15672"},
		{"queue": "jobs"},
		{"management_url": "rabbit:15672", "queue": "jobs"},
		{"management_url": "http://rabbit:15672", "queue": "jobs", "vhost": 3},
		{"management_url": "http://rabbit:15672", "queue": "jobs", "timeout": "later"},
	} {
		if err := p.Validate(bad); err == nil {
			t.Errorf("spec %v should fail validation", bad)
		}
	}
	good := map[string]any{"management_url": "http://rabbitmq.bookstore:15672/", "queue": "jobs", "_target": "jobs", "_tick": 10 * time.Second}
	if err := p.Validate(good); err != nil {
		t.Fatal(err)
	}

	// Default user guest with the default password variable unset: fail.
	t.Setenv(defaultPassEnv, "")
	if _, err := parse(good); err == nil {
		t.Fatal("guest without a password should fail")
	}
	t.Setenv(defaultPassEnv, "pw")
	c, err := parse(good)
	if err != nil {
		t.Fatal(err)
	}
	if c.user != "guest" || c.password != "pw" || !c.auth || c.vhost != "/" || c.tick != 10*time.Second || c.interval != 10*time.Second || c.base != "http://rabbitmq.bookstore:15672" {
		t.Fatalf("config = %+v", c)
	}
	// Custom user and variable.
	t.Setenv("RMQ_MON", "mon")
	c, err = parse(map[string]any{"management_url": "http://r:15672", "queue": "q", "user": "monitor", "password_env": "RMQ_MON", "vhost": "prod"})
	if err != nil || c.user != "monitor" || c.password != "mon" || c.vhost != "prod" {
		t.Fatalf("config = %+v err = %v", c, err)
	}
	// Empty user and empty password_env: unauthenticated.
	c, err = parse(map[string]any{"management_url": "http://r:15672", "queue": "q", "user": "", "password_env": ""})
	if err != nil || c.auth {
		t.Fatalf("config = %+v err = %v", c, err)
	}
	// Empty password_env with a user: fail.
	if _, err := parse(map[string]any{"management_url": "http://r:15672", "queue": "q", "user": "monitor", "password_env": ""}); err == nil {
		t.Fatal("user without password_env should fail")
	}
}

func TestQueueStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(queueJSON(7))
	}))
	defer srv.Close()
	t.Setenv("RMQ_TEST_PW", "pw")
	p := &Queue{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 4)
	spec := map[string]any{"management_url": srv.URL, "queue": "jobs", "password_env": "RMQ_TEST_PW", "_target": "jobs", "_tick": 20 * time.Millisecond}
	if err := p.Start(ctx, spec, out); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-out:
		if o.Target != "jobs" || o.Probe != KindQueue || o.Metrics["depth"] != 7 {
			t.Fatalf("observation = %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no observation from Start")
	}
	a, ok := probe.AccessFor(KindQueue)
	if !ok || !a.Implemented {
		t.Fatalf("access = %+v %v", a, ok)
	}
}
