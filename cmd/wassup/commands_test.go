package main

import (
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/app"
	"github.com/danilopopovikj/wassup/internal/bind"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/state"
)

// probeRun evaluates the observations of one `wassup probe` run over a small
// bookstore: an api and a database, the pool between them and a cache edge
// without a probe of its own.
func probeRun(t *testing.T, bindings model.Bindings, inst []app.Instance, obs ...probe.Observation) map[string]probeRow {
	t.Helper()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cfg := &model.Config{
		Topology: model.Topology{Name: "bookstore", Components: []model.Component{
			{ID: "api", Type: "workload", Label: "API"},
			{ID: "db", Type: "database", Label: "Database"},
			{ID: "cache", Type: "cache", Label: "Cache"},
		}, Edges: []model.Edge{
			{From: "api", To: "db", Kind: "sql"},
			{From: "api", To: "cache", Kind: "cache"},
		}},
		Bindings: bindings,
	}
	b := bind.New()
	for _, o := range obs {
		o.At = now
		b.Apply(o)
	}
	joined := b.All()
	snap := state.Evaluate(state.Input{Topology: &cfg.Topology, Joined: joined, Now: now, Tick: 1, TickEvery: 5 * time.Second})
	rows := map[string]probeRow{}
	for _, r := range probeRows(cfg, snap, joined, inst) {
		rows[r.ID] = r
	}
	return rows
}

var bookstoreBindings = model.Bindings{
	Components: map[string][]model.ProbeSpec{
		"api":   {{"probe": "k8s.workload"}},
		"db":    {{"probe": "pg.stats"}},
		"cache": {{"probe": "redis.info"}},
	},
	Edges: map[string][]model.ProbeSpec{
		"api->db": {{"probe": "pg.pool"}},
	},
}

var bookstoreUp = []probe.Observation{
	{Target: "api", Probe: "k8s.workload", Metrics: map[string]float64{"replicas_ready": 3, "replicas_desired": 3}},
	{Target: "db", Probe: "pg.stats", Metrics: map[string]float64{"connections_used": 12, "connections_max": 100}},
	{Target: "cache", Probe: "redis.info", Metrics: map[string]float64{"hit_rate": 97}},
}

func TestProbeRowsEdgeWithFailedProbeIsUnbound(t *testing.T) {
	obs := append([]probe.Observation{
		{Target: "api->db", Probe: "pg.pool", Err: "failed to connect to `host=127.0.0.1`: connection refused"},
	}, bookstoreUp...)
	rows := probeRun(t, bookstoreBindings, nil, obs...)
	r := rows["api->db"]
	if r.Bound {
		t.Errorf("an edge whose only probe failed must not read bound: %+v", r)
	}
	if !strings.HasPrefix(r.Label, "unbound, ") || !strings.Contains(r.Label, "pg.pool") || !strings.Contains(r.Label, "connection refused") {
		t.Errorf("the label should say which probe failed and why, got %q", r.Label)
	}
	if r.State != string(model.Idle) {
		t.Errorf("state = %q, an unbound element reads idle", r.State)
	}
	for _, id := range []string{"api", "db", "cache", "api->cache"} {
		if !rows[id].Bound {
			t.Errorf("%s should be bound: %+v", id, rows[id])
		}
	}
}

func TestProbeRowsEdgeWhoseProbeNeverStartedIsUnbound(t *testing.T) {
	inst := []app.Instance{
		{Kind: "pg.pool", Target: "api->db", Health: string(probe.HealthFailed), Error: "environment variable POOL_DSN (dsn_env) is not set"},
	}
	r := probeRun(t, bookstoreBindings, inst, bookstoreUp...)["api->db"]
	if r.Bound {
		t.Errorf("an edge whose probe never reported must not read bound: %+v", r)
	}
	if want := "unbound, pg.pool: environment variable POOL_DSN (dsn_env) is not set"; r.Label != want {
		t.Errorf("label = %q, want %q", r.Label, want)
	}
}

func TestProbeRowsBoundWhenOneProbeDelivers(t *testing.T) {
	bindings := model.Bindings{
		Components: bookstoreBindings.Components,
		Edges: map[string][]model.ProbeSpec{
			"api->db": {{"probe": "pg.pool"}, {"probe": "signoz.edge"}},
		},
	}
	obs := append([]probe.Observation{
		{Target: "api->db", Probe: "pg.pool", Err: "connection refused"},
		{Target: "api->db", Probe: "signoz.edge", Metrics: map[string]float64{"rate": 40}},
	}, bookstoreUp...)
	r := probeRun(t, bindings, nil, obs...)["api->db"]
	if !r.Bound || r.State != string(model.Flowing) {
		t.Errorf("one probe delivered, the edge is bound and flowing: %+v", r)
	}
}

func TestProbeRowsComponentKeepsTheEngineLabel(t *testing.T) {
	obs := []probe.Observation{
		bookstoreUp[0], bookstoreUp[2],
		{Target: "db", Probe: "pg.stats", Err: "connection refused"},
	}
	rows := probeRun(t, bookstoreBindings, nil, obs...)
	if r := rows["db"]; r.Bound || r.Label != "unbound, connection refused" {
		t.Errorf("db = %+v", r)
	}
	if r := rows["api->db"]; r.Bound {
		t.Errorf("an edge to an unbound component with no data of its own is unbound: %+v", r)
	}
}

func TestProbeCounts(t *testing.T) {
	rows := []probeRow{
		{ID: "api", Kind: "workload", Bound: true},
		{ID: "db", Kind: "database", Bound: false},
		{ID: "api->db", Kind: "edge", Bound: false},
		{ID: "api->cache", Kind: "edge", Bound: true},
	}
	c := countProbeRows(rows)
	if c.Bound != 1 || c.Total != 2 || c.EdgesBound != 1 || c.EdgesTotal != 2 {
		t.Errorf("counts = %+v", c)
	}
	if !c.unbound() {
		t.Error("an unbound edge alone must fail the run")
	}
	if all := countProbeRows(rows[:1]); all.unbound() {
		t.Errorf("everything bound: %+v", all)
	}
}
