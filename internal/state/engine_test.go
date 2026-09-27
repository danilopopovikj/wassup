package state

import (
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/bind"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

var now = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func topo() *model.Topology {
	return &model.Topology{Name: "t", Components: []model.Component{
		{ID: "lb", Type: "lb", Label: "Load balancer"},
		{ID: "ingress", Type: "ingress", Label: "Ingress"},
		{ID: "api", Type: "workload", Label: "API", RunsOn: []string{"n1"}},
		{ID: "n1", Type: "node"},
		{ID: "db", Type: "db", Label: "Database"},
		{ID: "q", Type: "queue", Label: "Jobs"},
		{ID: "obs", Type: "observability", Label: "SigNoz"},
	}, Edges: []model.Edge{
		{From: "lb", To: "ingress", Kind: "http"},
		{From: "ingress", To: "api", Kind: "http"},
		{From: "api", To: "db", Kind: "sql"},
		{From: "api", To: "q", Kind: "queue"},
	}}
}

func eval(t *testing.T, obs ...probe.Observation) *model.Snapshot {
	t.Helper()
	b := bind.New()
	for _, o := range obs {
		if o.At.IsZero() {
			o.At = now
		}
		b.Apply(o)
	}
	return Evaluate(Input{Topology: topo(), Joined: b.All(), Now: now, Tick: 1, TickEvery: 5 * time.Second})
}

func TestUnboundNeverHealthy(t *testing.T) {
	s := eval(t)
	for id, es := range s.Components {
		if es.Marker != model.MarkerUnbound {
			t.Errorf("%s should be unbound, got %s %q", id, es.State, es.Label)
		}
		if es.Severity != model.Info {
			t.Errorf("%s unbound must not raise severity", id)
		}
	}
	for id, es := range s.Edges {
		if es.Marker != model.MarkerUnbound {
			t.Errorf("edge %s should be unbound", id)
		}
	}
	if len(s.Issues) != 0 {
		t.Errorf("no issues expected, got %d", len(s.Issues))
	}
}

func TestStaleKeepsLastState(t *testing.T) {
	b := bind.New()
	b.Apply(probe.Observation{Target: "api", At: now.Add(-time.Minute), Metrics: map[string]float64{"replicas_ready": 3, "replicas_desired": 3}})
	prev := &model.Snapshot{Components: map[string]model.ElementState{"api": {State: model.Flowing, Label: "flowing, 1k req/s"}}}
	s := Evaluate(Input{Topology: topo(), Joined: b.All(), Now: now, TickEvery: 5 * time.Second, Prev: prev})
	es := s.Components["api"]
	if es.Marker != model.MarkerStale || es.State != model.Flowing {
		t.Errorf("stale should keep the last state: %+v", es)
	}
}

func TestFlowingIdleFromEdges(t *testing.T) {
	s := eval(t,
		probe.Observation{Target: "lb", Metrics: map[string]float64{"rate": 1200}},
		probe.Observation{Target: "ingress", Metrics: map[string]float64{"rate": 1200}},
		probe.Observation{Target: "api", Metrics: map[string]float64{"replicas_ready": 3, "replicas_desired": 3}},
		probe.Observation{Target: "db", Metrics: map[string]float64{"cpu_pct": 10}},
		probe.Observation{Target: "q", Metrics: map[string]float64{"depth": 0}},
		probe.Observation{Target: "ingress->api", Metrics: map[string]float64{"rate": 1200, "error_rate": 0.1}},
	)
	if s.Edges["ingress->api"].Label != "flowing, 1.2k req/s" {
		t.Errorf("edge label %q", s.Edges["ingress->api"].Label)
	}
	if s.Components["api"].State != model.Flowing || s.Components["api"].Label != "flowing, 1.2k req/s" {
		t.Errorf("api %q", s.Components["api"].Label)
	}
	if s.Components["q"].State != model.Idle || s.Components["q"].Label != "idle" {
		t.Errorf("queue should be idle: %q", s.Components["q"].Label)
	}
	// lb->ingress has no observation but the ingress carries a rate: derived flow.
	if s.Edges["lb->ingress"].State != model.Flowing {
		t.Errorf("derived edge %q", s.Edges["lb->ingress"].Label)
	}
}

func TestCertCountdownWarnsBeforeFailing(t *testing.T) {
	s := eval(t,
		probe.Observation{Target: "ingress", Metrics: map[string]float64{"rate": 10, "cert_days": 2},
			Conditions: []model.Condition{{Kind: model.CondCertRenewalFailed, Since: now.Add(-8 * 24 * time.Hour), Detail: "DNS challenge error"}}},
	)
	es := s.Components["ingress"]
	if es.State == model.Failing || es.Severity != model.Warn {
		t.Errorf("2 days left should be warn, not failing: %s %s", es.State, es.Severity)
	}
	if !strings.Contains(strings.Join(es.Notes, "|"), "cert expires in 2 days") {
		t.Errorf("notes %v", es.Notes)
	}
	var cert *model.Gauge
	for i := range es.Gauges {
		if es.Gauges[i].Name == "cert" {
			cert = &es.Gauges[i]
		}
	}
	if cert == nil || cert.Level != "red" || cert.Value != "2 days" {
		t.Errorf("cert gauge %+v", cert)
	}
}

func TestSeverityRedGaugeWithoutStateChange(t *testing.T) {
	s := eval(t, probe.Observation{Target: "db", Metrics: map[string]float64{"cpu_pct": 93}})
	es := s.Components["db"]
	if es.State != model.Idle || es.Severity != model.Warn {
		t.Errorf("red gauge should warn without changing state: %s %s", es.State, es.Severity)
	}
}

func TestNoDataInsteadOfIdle(t *testing.T) {
	s := eval(t,
		probe.Observation{Target: "obs", Metrics: map[string]float64{"ingest_rate": 0}, Conditions: []model.Condition{{Kind: model.CondNoData, Since: now.Add(-40 * time.Minute)}}},
		probe.Observation{Target: "api", Metrics: map[string]float64{"replicas_ready": 3, "replicas_desired": 3}},
		probe.Observation{Target: "db", Metrics: map[string]float64{"cpu_pct": 10}},
	)
	if s.Components["obs"].Label != "failing, no data received for 40 min" {
		t.Errorf("obs %q", s.Components["obs"].Label)
	}
	if e := s.Edges["api->db"]; e.Marker != model.MarkerNoData || e.Label != "no data" {
		t.Errorf("edge should say no data, got %+v", e)
	}
}

func TestBlockedByFirewallCondition(t *testing.T) {
	s := eval(t,
		probe.Observation{Target: "api", Metrics: map[string]float64{"replicas_ready": 1, "replicas_desired": 1}},
		probe.Observation{Target: "db", Metrics: map[string]float64{"cpu_pct": 1}},
		probe.Observation{Target: "api->db", Conditions: []model.Condition{{Kind: model.CondFirewallDenied, Detail: "allow-lb-only", Since: now.Add(-time.Hour)}}},
	)
	if got := s.Edges["api->db"].Label; got != "blocked at firewall since 11:00, rule allow-lb-only" {
		t.Errorf("label %q", got)
	}
	if len(s.Issues) == 0 || s.Issues[0].Severity != model.Crit {
		t.Errorf("blocked edge must be a crit issue: %+v", s.Issues)
	}
}

func TestFormatting(t *testing.T) {
	cases := map[float64]string{38: "38", 1200: "1.2k", 2400: "2.4k", 1000: "1k", 0.2: "0.2", 3100000: "3.1M"}
	for in, want := range cases {
		if got := Num(in); got != want {
			t.Errorf("Num(%v) = %q, want %q", in, got, want)
		}
	}
	if Dur(35*time.Minute) != "35 min" || Dur(3*time.Hour) != "3 h" || Dur(3*time.Hour+2*time.Minute) != "3 h" || Dur(90*time.Second) != "2 min" {
		t.Errorf("Dur: %s %s %s", Dur(35*time.Minute), Dur(3*time.Hour), Dur(90*time.Second))
	}
	if Bytes(40<<30) != "40 GB" || Bytes(2048) != "2 kB" {
		t.Errorf("Bytes: %s %s", Bytes(40<<30), Bytes(2048))
	}
}
