package netprobe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// newPing returns a probe wired to a short client timeout and a fake clock.
func newPing(timeout time.Duration) (*Ping, *time.Time) {
	clock := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	p := &Ping{
		now:    func() time.Time { return clock },
		client: newClient(timeout),
	}
	return p, &clock
}

func TestPingSuccess(t *testing.T) {
	var gotUA atomic.Value
	var method atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA.Store(r.UserAgent())
		method.Store(r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p, _ := newPing(2 * time.Second)
	a := p.ping(context.Background(), http.MethodHead, srv.URL, 2*time.Second, 0)
	o := p.observe("github", srv.URL, a)

	if o.Target != "github" || o.Probe != KindPing {
		t.Fatalf("target/probe = %q/%q", o.Target, o.Probe)
	}
	if o.Metrics["status"] != 204 || o.Metrics["error_rate"] != 0 || o.Metrics["timeout_rate"] != 0 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if o.Metrics["latency_ms"] < 0 {
		t.Fatalf("latency_ms = %v", o.Metrics["latency_ms"])
	}
	if len(o.Conditions) != 0 {
		t.Fatalf("unexpected conditions %v", o.Conditions)
	}
	if o.Detail["status"] != 204 || o.Detail["url"] != srv.URL || o.Detail["window"] != 1 {
		t.Fatalf("detail = %v", o.Detail)
	}
	if ua, _ := gotUA.Load().(string); ua != userAgent {
		t.Fatalf("user agent = %q", ua)
	}
	if m, _ := method.Load().(string); m != http.MethodHead {
		t.Fatalf("method = %q", m)
	}
}

func TestPingExpectStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p, _ := newPing(2 * time.Second)

	// 401 is fine when that is what we expect...
	o := p.observe("x", srv.URL, p.ping(context.Background(), http.MethodGet, srv.URL, 2*time.Second, 401))
	if o.Metrics["error_rate"] != 0 {
		t.Fatalf("error_rate = %v, want 0", o.Metrics["error_rate"])
	}
	// ...and an error when we expected 200.
	o = p.observe("x", srv.URL, p.ping(context.Background(), http.MethodGet, srv.URL, 2*time.Second, 200))
	if o.Metrics["error_rate"] != 50 {
		t.Fatalf("error_rate = %v, want 50", o.Metrics["error_rate"])
	}
	if o.Detail["last_error"] != "status 401, expected 200" {
		t.Fatalf("last_error = %v", o.Detail["last_error"])
	}
}

func TestPingServerErrors(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, _ := newPing(2 * time.Second)
	var o probe.Observation
	for i := 0; i < 12; i++ {
		o = p.observe("github", srv.URL, p.ping(context.Background(), http.MethodHead, srv.URL, 2*time.Second, 0))
	}
	// Window holds the last 10 attempts, all 500s by now.
	if o.Metrics["error_rate"] != 100 {
		t.Fatalf("error_rate = %v, want 100", o.Metrics["error_rate"])
	}
	if o.Metrics["timeout_rate"] != 0 {
		t.Fatalf("timeout_rate = %v, want 0", o.Metrics["timeout_rate"])
	}
	if o.Metrics["status"] != 500 || o.Detail["window"] != pingWindow {
		t.Fatalf("status/window = %v/%v", o.Metrics["status"], o.Detail["window"])
	}
	// A 500 is an answer, not a timeout: no Timeout condition.
	if _, ok := model.HasCondition(o.Conditions, model.CondTimeout); ok {
		t.Fatalf("unexpected Timeout condition on 500s")
	}
}

func TestPingTimeouts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	const timeout = 100 * time.Millisecond
	p, clock := newPing(timeout)
	first := *clock
	var o probe.Observation
	for i := 0; i < pingStreak; i++ {
		o = p.observe("github", srv.URL, p.ping(context.Background(), http.MethodHead, srv.URL, timeout, 0))
		if i < pingStreak-1 {
			if _, ok := model.HasCondition(o.Conditions, model.CondTimeout); ok {
				t.Fatalf("Timeout raised after %d attempt(s)", i+1)
			}
		}
		*clock = clock.Add(5 * time.Second)
	}
	if o.Metrics["error_rate"] != 100 || o.Metrics["timeout_rate"] != 100 {
		t.Fatalf("metrics = %v, want 100/100", o.Metrics)
	}
	c, ok := model.HasCondition(o.Conditions, model.CondTimeout)
	if !ok {
		t.Fatalf("no Timeout condition after %d timeouts: %+v", pingStreak, o)
	}
	// The external facet names the bound component; the URL stays in detail.
	if c.Ref != "github" {
		t.Fatalf("ref = %q", c.Ref)
	}
	if o.Detail["url"] != srv.URL {
		t.Fatalf("url detail = %v", o.Detail["url"])
	}
	if !c.Since.Equal(first) {
		t.Fatalf("since = %v, want first failure %v", c.Since, first)
	}
	if c.Detail != "HEAD timed out after 100ms" {
		t.Fatalf("detail = %q", c.Detail)
	}
	if o.Detail["last_error"] != c.Detail {
		t.Fatalf("last_error = %v", o.Detail["last_error"])
	}

	// A success clears the streak.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv2.Close()
	o = p.observe("github", srv2.URL, p.ping(context.Background(), http.MethodHead, srv2.URL, time.Second, 0))
	if _, ok := model.HasCondition(o.Conditions, model.CondTimeout); ok {
		t.Fatalf("Timeout still raised after a success")
	}
	if o.Metrics["timeout_rate"] != 75 {
		t.Fatalf("timeout_rate = %v, want 75", o.Metrics["timeout_rate"])
	}
}

func TestPingConnectionRefused(t *testing.T) {
	// Grab a free port and close it so the connection is refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + l.Addr().String()
	l.Close()

	p, _ := newPing(time.Second)
	var o probe.Observation
	for i := 0; i < pingStreak; i++ {
		o = p.observe("github", url, p.ping(context.Background(), http.MethodHead, url, time.Second, 0))
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondTimeout); !ok {
		t.Fatalf("connection refused streak did not raise Timeout: %+v", o)
	}
	if o.Metrics["error_rate"] != 100 || o.Metrics["timeout_rate"] != 0 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
}

func TestPingValidate(t *testing.T) {
	p := &Ping{}
	cases := map[string]map[string]any{
		"missing url":   {},
		"relative url":  {"url": "/health"},
		"bad scheme":    {"url": "ftp://x"},
		"bad method":    {"url": "https://x", "method": "POST"},
		"bad status":    {"url": "https://x", "expect_status": 42},
		"bad timeout":   {"url": "https://x", "timeout": "soon"},
		"string status": {"url": "https://x", "expect_status": "200"},
	}
	for name, spec := range cases {
		if err := p.Validate(spec); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if err := p.Validate(map[string]any{"url": "https://api.github.com", "method": "get", "expect_status": 200, "timeout": "3s"}); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestPingStartStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	p := &Ping{}
	out := make(chan probe.Observation, 16)
	ctx, cancel := context.WithCancel(context.Background())
	spec := map[string]any{"url": srv.URL, "_target": "github", "_tick": 20 * time.Millisecond}
	if err := p.Start(ctx, spec, out); err != nil {
		t.Fatal(err)
	}
	if p.Health().State != probe.HealthOK {
		t.Fatalf("health = %+v", p.Health())
	}
	var got []probe.Observation
	deadline := time.After(3 * time.Second)
	for len(got) < 3 {
		select {
		case o := <-out:
			got = append(got, o)
		case <-deadline:
			t.Fatalf("got %d observations, want 3", len(got))
		}
	}
	for _, o := range got {
		if o.Target != "github" || o.Probe != KindPing || o.Metrics["status"] != 200 {
			t.Fatalf("bad observation %+v", o)
		}
	}
	cancel()
	// After cancel the goroutine must stop: drain whatever is buffered and
	// make sure nothing new arrives.
	time.Sleep(100 * time.Millisecond)
	for len(out) > 0 {
		<-out
	}
	select {
	case o := <-out:
		t.Fatalf("observation after cancel: %+v", o)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPingStartRejectsBadSpec(t *testing.T) {
	p := &Ping{}
	if err := p.Start(context.Background(), map[string]any{}, make(chan probe.Observation)); err == nil {
		t.Fatal("expected error")
	}
	if p.Health().State != probe.HealthFailed {
		t.Fatalf("health = %+v", p.Health())
	}
}
