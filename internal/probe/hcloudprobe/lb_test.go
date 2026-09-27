package hcloudprobe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// lbJSON is scenario 07: three nodes behind the lb, node-3 (server 103) is
// failing its 6443 health check while a label selector target also matched it.
const lbJSON = `{"load_balancer": {
  "id": 5, "name": "lb", "algorithm": {"type": "round_robin"}, "location": {"name": "fsn1"},
  "services": [
    {"protocol": "tcp", "listen_port": 6443, "destination_port": 6443, "health_check": {"protocol": "tcp", "port": 6443}},
    {"protocol": "tcp", "listen_port": 443, "destination_port": 30443, "health_check": {"protocol": "tcp", "port": 30443}}
  ],
  "targets": [
    {"type": "server", "server": {"id": 101}, "health_status": [{"listen_port": 6443, "status": "healthy"}, {"listen_port": 443, "status": "healthy"}]},
    {"type": "label_selector", "label_selector": {"selector": "role=node"}, "targets": [
      {"type": "server", "server": {"id": 102}, "health_status": [{"listen_port": 6443, "status": "healthy"}, {"listen_port": 443, "status": "healthy"}]},
      {"type": "server", "server": {"id": 103}, "health_status": [{"listen_port": 443, "status": "healthy"}, {"listen_port": 6443, "status": "unhealthy"}]}
    ]}
  ]
}}`

const metricsJSON = `{"metrics": {"start": "2026-09-27T10:00:00Z", "end": "2026-09-27T10:01:00Z", "step": 60, "time_series": {
  "open_connections": {"values": [[1790503200, "310"], [1790503260, "340"]]},
  "requests_per_second": {"values": [[1790503200, "1050.5"], [1790503260, "1100"]]}
}}}`

func TestSummarize(t *testing.T) {
	var resp struct {
		LoadBalancer LoadBalancer `json:"load_balancer"`
	}
	if err := json.Unmarshal([]byte(lbJSON), &resp); err != nil {
		t.Fatal(err)
	}
	s := Summarize(resp.LoadBalancer)
	if s.Healthy != 2 || s.Total != 3 {
		t.Errorf("healthy/total = %d/%d", s.Healthy, s.Total)
	}
	if len(s.Targets) != 3 || s.Targets[2].ServerID != 103 || s.Targets[2].Healthy || s.Targets[2].Via != "role=node" {
		t.Errorf("targets = %+v", s.Targets)
	}
	if s.Targets[2].Status != "443 healthy, 6443 unhealthy" {
		t.Errorf("status = %q", s.Targets[2].Status)
	}
	if ok, status := healthOf(nil); ok || status != "no health status" {
		t.Errorf("empty health = %v %q", ok, status)
	}
}

func TestLastValue(t *testing.T) {
	var m LoadBalancerMetricsResponse
	if err := json.Unmarshal([]byte(metricsJSON), &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := LastValue(m, "open_connections"); !ok || v != 340 {
		t.Errorf("open_connections = %v %v", v, ok)
	}
	if v, ok := LastValue(m, "requests_per_second"); !ok || v != 1100 {
		t.Errorf("requests_per_second = %v %v", v, ok)
	}
	if _, ok := LastValue(m, "bandwidth"); ok {
		t.Error("missing series should be unknown")
	}
}

func TestTargetsFromSpec(t *testing.T) {
	got, err := targetsFromSpec(map[string]any{"node-1": "node-1", "node-2": 102, "node-3": "103"})
	if err != nil {
		t.Fatal(err)
	}
	if got["node-1"].Name != "node-1" || got["node-2"].ID != 102 || got["node-3"].ID != 103 {
		t.Errorf("targets = %+v", got)
	}
	if _, err := targetsFromSpec(map[string]any{"node-1": true}); err == nil {
		t.Error("bool target should fail")
	}
	if _, err := targetsFromSpec("node-1"); err == nil {
		t.Error("non-map targets should fail")
	}
	if err := (&LBProbe{}).Validate(map[string]any{"name": "lb", "targets": map[string]any{"node-1": ""}}); err == nil {
		t.Error("empty target should fail validation")
	}
}

// fakeAPI serves the scenario 07 load balancer and its metrics.
func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/load_balancers", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "lb" {
			_, _ = w.Write([]byte(`{"load_balancers": []}`))
			return
		}
		_, _ = w.Write([]byte(`{"load_balancers": [{"id": 5, "name": "lb"}]}`))
	})
	mux.HandleFunc("/load_balancers/5", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error": {"code": "unauthorized", "message": "unable to authenticate"}}`))
			return
		}
		_, _ = w.Write([]byte(lbJSON))
	})
	mux.HandleFunc("/load_balancers/5/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("type"), "requests_per_second") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(metricsJSON))
	})
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("name") {
		case "node-1":
			_, _ = w.Write([]byte(`{"servers": [{"id": 101, "name": "node-1"}]}`))
		case "node-3":
			_, _ = w.Write([]byte(`{"servers": [{"id": 103, "name": "node-3"}]}`))
		default:
			_, _ = w.Write([]byte(`{"servers": []}`))
		}
	})
	mux.HandleFunc("/firewalls/9", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"firewall": {"id": 9, "name": "allow-lb-only", "applied_to": [{"type": "label_selector", "label_selector": {"selector": "role=node"}}], "rules": [
          {"direction": "in", "protocol": "tcp", "port": "80", "source_ips": ["10.0.0.5/32"], "description": "allow-lb-only"},
          {"direction": "in", "protocol": "tcp", "port": "443", "source_ips": ["10.0.0.5/32"], "description": "allow-lb-only"},
          {"direction": "out", "protocol": "tcp", "port": "1-65535", "destination_ips": ["0.0.0.0/0"]}
        ]}}`))
	})
	return httptest.NewServer(mux)
}

func TestLBPoll(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	t.Setenv("WASSUP_TEST_HCLOUD_TOKEN", "tok")
	spec := map[string]any{
		"name": "lb", "token_env": "WASSUP_TEST_HCLOUD_TOKEN", "endpoint": srv.URL, "_target": "lb",
		"targets": map[string]any{"node-1": "node-1", "node-2": 102, "node-3": "node-3", "node-9": "node-9"},
	}
	p := &LBProbe{}
	c, err := newClient(spec)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := refFromSpec(spec)
	targets, _ := targetsFromSpec(spec["targets"])
	seen := since{}
	obs := p.poll(context.Background(), c, &ref, targets, "lb", seen)
	byTarget := map[string]probe.Observation{}
	for _, o := range obs {
		byTarget[o.Target] = o
	}
	lb := byTarget["lb"]
	if lb.Err != "" || lb.Probe != "hcloud.lb" {
		t.Fatalf("lb observation = %+v", lb)
	}
	for k, want := range map[string]float64{"connections": 340, "rate": 1100, "targets_healthy": 2, "targets_total": 3} {
		if lb.Metrics[k] != want {
			t.Errorf("lb %s = %v, want %v", k, lb.Metrics[k], want)
		}
	}
	if lb.Detail["algorithm"] != "round_robin" || lb.Detail["location"] != "fsn1" {
		t.Errorf("lb detail = %+v", lb.Detail)
	}
	if e := byTarget["lb->node-1"]; e.Metrics["healthy"] != 1 || len(e.Conditions) != 0 {
		t.Errorf("lb->node-1 = %+v", e)
	}
	if e := byTarget["lb->node-2"]; e.Metrics["healthy"] != 1 {
		t.Errorf("lb->node-2 (id target) = %+v", e)
	}
	e := byTarget["lb->node-3"]
	if e.Metrics["healthy"] != 0 {
		t.Errorf("lb->node-3 = %+v", e)
	}
	c3, ok := model.HasCondition(e.Conditions, model.CondHealthCheckFailing)
	if !ok || c3.Ref != "target/node-3" || c3.Detail != "443 healthy, 6443 unhealthy" || c3.Since.IsZero() {
		t.Errorf("HealthCheckFailing = %+v %v", c3, ok)
	}
	if e := byTarget["lb->node-9"]; e.Err == "" {
		t.Errorf("unknown server should report an error: %+v", e)
	}
	if h := p.Health(); h.State != probe.HealthDegraded {
		t.Errorf("an unresolved target degrades health: %+v", h)
	}
	// Second poll keeps Since.
	first := c3.Since
	time.Sleep(2 * time.Millisecond)
	obs = p.poll(context.Background(), c, &ref, targets, "lb", seen)
	for _, o := range obs {
		if o.Target == "lb->node-3" {
			if c, _ := model.HasCondition(o.Conditions, model.CondHealthCheckFailing); !c.Since.Equal(first) {
				t.Errorf("Since moved from %v to %v", first, c.Since)
			}
		}
	}
	if len(seen) != 1 {
		t.Errorf("seen = %v", seen)
	}
}

func TestLBPollUnauthorized(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	t.Setenv("WASSUP_TEST_HCLOUD_TOKEN", "wrong")
	spec := map[string]any{"id": 5, "token_env": "WASSUP_TEST_HCLOUD_TOKEN", "endpoint": srv.URL, "targets": map[string]any{"node-1": 101}}
	p := &LBProbe{}
	c, _ := newClient(spec)
	ref, _ := refFromSpec(spec)
	targets, _ := targetsFromSpec(spec["targets"])
	obs := p.poll(context.Background(), c, &ref, targets, "lb", since{})
	if len(obs) != 2 || obs[0].Err == "" || obs[1].Err == "" || obs[1].Target != "lb->node-1" {
		t.Errorf("obs = %+v", obs)
	}
	if h := p.Health(); h.State != probe.HealthFailed {
		t.Errorf("401 is a misconfiguration: %+v", h)
	}
}

func TestFirewallPoll(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	t.Setenv("WASSUP_TEST_HCLOUD_TOKEN", "tok")
	spec := map[string]any{"id": 9, "token_env": "WASSUP_TEST_HCLOUD_TOKEN", "endpoint": srv.URL}
	p := &FirewallProbe{}
	c, _ := newClient(spec)
	ref, _ := refFromSpec(spec)
	seen := since{}

	// Component binding: rule count and detail only.
	o := p.poll(context.Background(), c, &ref, 0, "tcp", "fw", seen)
	if o.Err != "" || o.Metrics["rules"] != 3 || len(o.Conditions) != 0 {
		t.Fatalf("fw = %+v", o)
	}
	if _, ok := o.Metrics["allowed"]; ok {
		t.Error("allowed is an edge metric")
	}
	if rules, ok := o.Detail["rules"].([]map[string]any); !ok || len(rules) != 3 || rules[0]["description"] != "allow-lb-only" {
		t.Errorf("rules detail = %+v", o.Detail["rules"])
	}
	if at, ok := o.Detail["applied_to"].([]string); !ok || len(at) != 1 || at[0] != "selector/role=node" {
		t.Errorf("applied_to = %+v", o.Detail["applied_to"])
	}

	// Edge binding for OTLP 4317: denied.
	e := p.poll(context.Background(), c, &ref, 4317, "tcp", "fw->signoz", seen)
	if e.Metrics["allowed"] != 0 {
		t.Errorf("allowed = %v", e.Metrics["allowed"])
	}
	cond, ok := model.HasCondition(e.Conditions, model.CondFirewallDenied)
	if !ok || cond.Ref != "rule/allow-lb-only" || cond.Detail != "allow-lb-only" || cond.Since.IsZero() {
		t.Errorf("FirewallDenied = %+v %v", cond, ok)
	}
	if len(e.Events) != 0 {
		t.Error("the firewall probe never emits events")
	}

	// Edge binding for 443: allowed, and the denial memory is cleared.
	a := p.poll(context.Background(), c, &ref, 443, "tcp", "fw->api", seen)
	if a.Metrics["allowed"] != 1 || len(a.Conditions) != 0 || a.Detail["allowed_by"] != "allow-lb-only" {
		t.Errorf("443 = %+v", a)
	}
	if len(seen) != 0 {
		t.Errorf("seen should be cleared once allowed: %v", seen)
	}
}

func TestStartWithoutToken(t *testing.T) {
	spec := map[string]any{"name": "lb", "token_env": "WASSUP_TEST_HCLOUD_TOKEN_MISSING"}
	if err := (&LBProbe{}).Start(context.Background(), spec, nil); err == nil {
		t.Error("missing token should fail Start")
	}
	if err := (&FirewallProbe{}).Start(context.Background(), spec, nil); err == nil {
		t.Error("missing token should fail Start")
	}
}

func TestRunStopsAndReemits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan probe.Observation, 64)
	polls := 0
	done := make(chan struct{})
	go func() {
		run(ctx, out, 5*time.Millisecond, 50*time.Millisecond, func(context.Context) []probe.Observation {
			polls++
			return []probe.Observation{{Target: "lb"}, {Target: "lb->node-1"}}
		})
		close(done)
	}()
	var n int
	deadline := time.After(2 * time.Second)
	for n < 10 {
		select {
		case o := <-out:
			if o.At.IsZero() {
				t.Error("At must be stamped on every emit")
			}
			n++
		case <-deadline:
			t.Fatalf("only %d observations", n)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run did not stop")
	}
	if polls > 3 {
		t.Errorf("polled %d times for %d observations; re-emits should not hit the API", polls, n)
	}
}
