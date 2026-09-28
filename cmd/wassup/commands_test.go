package main

import (
	"encoding/json"
	"reflect"
	"runtime/debug"
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
	if !strings.HasPrefix(r.Label, "no data, ") || !strings.Contains(r.Label, "pg.pool") || !strings.Contains(r.Label, "connection refused") {
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
	if want := "no data, pg.pool: environment variable POOL_DSN (dsn_env) is not set"; r.Label != want {
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

func TestProbeRowsComponentWithFailedProbe(t *testing.T) {
	obs := []probe.Observation{
		bookstoreUp[0], bookstoreUp[2],
		{Target: "db", Probe: "pg.stats", Err: "connection refused"},
	}
	rows := probeRun(t, bookstoreBindings, nil, obs...)
	if r := rows["db"]; r.Bound || r.Label != "no data, pg.stats: connection refused" {
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

// With several probes on an element and one of them failing, the element
// stays bound and the report names the probe that failed and why.
func TestProbeRowsNameTheProbeThatFailed(t *testing.T) {
	bindings := model.Bindings{
		Components: map[string][]model.ProbeSpec{
			"api":   {{"probe": "k8s.workload"}},
			"db":    {{"probe": "cnpg.cluster"}, {"probe": "pg.stats"}, {"probe": "k8s.pvc"}},
			"cache": {{"probe": "redis.info"}},
		},
	}
	inst := []app.Instance{
		{Kind: "k8s.pvc", Target: "db", Health: string(probe.HealthDegraded), Message: "no data yet"},
	}
	obs := []probe.Observation{
		bookstoreUp[0], bookstoreUp[2],
		{Target: "db", Probe: "cnpg.cluster", Metrics: map[string]float64{"replicas_ready": 2, "replicas_desired": 2}},
		{Target: "db", Probe: "pg.stats", Err: "permission denied for view pg_stat_replication"},
	}
	r := probeRun(t, bindings, inst, obs...)["db"]
	if !r.Bound {
		t.Fatalf("one probe delivered, db is bound: %+v", r)
	}
	want := []probeResult{
		{Probe: "cnpg.cluster", Status: "ok", Metrics: map[string]float64{"replicas_ready": 2, "replicas_desired": 2}},
		{Probe: "pg.stats", Status: "error", Error: "permission denied for view pg_stat_replication"},
		{Probe: "k8s.pvc", Status: "silent", Error: "no data yet"},
	}
	if !reflect.DeepEqual(r.Results, want) {
		t.Errorf("results = %+v\nwant      %+v", r.Results, want)
	}
	if f := r.failed(); len(f) != 2 || f[0].Probe != "pg.stats" {
		t.Errorf("failed = %+v", f)
	}
	// The JSON form carries status, error and metrics per probe.
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`"probe_results":[`, `"status":"error"`, `"error":"permission denied for view pg_stat_replication"`, `"metrics":{"replicas_desired":2,"replicas_ready":2}`} {
		if !strings.Contains(string(b), part) {
			t.Errorf("json misses %s: %s", part, b)
		}
	}
}

// Idle is only said where a rate was read. With nothing measuring traffic
// the report says so instead.
func TestProbeRowsIdleNeedsARate(t *testing.T) {
	rows := probeRun(t, bookstoreBindings, nil, append([]probe.Observation{
		{Target: "api->db", Probe: "pg.pool", Metrics: map[string]float64{"rate": 0, "pool_used": 0, "pool_max": 20}},
	}, bookstoreUp...)...)
	if r := rows["api->db"]; r.Label != "idle" || r.Note != "" {
		t.Errorf("a rate of 0 was read, the edge is idle: %+v", r)
	}
	if r := rows["db"]; r.Label != "idle" {
		t.Errorf("db has an edge with a rate: %+v", r)
	}
	if r := rows["api->cache"]; r.Label != noRate || r.Note == "" || r.State != string(model.Idle) {
		t.Errorf("nothing measures api->cache: %+v", r)
	}
	if r := rows["cache"]; r.Label != noRate {
		t.Errorf("nothing measures the traffic of cache: %+v", r)
	}
}

func TestVersionFromBuildInfo(t *testing.T) {
	installed := &debug.BuildInfo{Main: debug.Module{Version: "v0.4.2"}}
	if got := versionFrom("dev", installed); got != "v0.4.2" {
		t.Errorf("go install ...@v0.4.2 should print the module version, got %q", got)
	}
	if got := versionFrom("v0.5.0", installed); got != "v0.5.0" {
		t.Errorf("a version set at link time wins, got %q", got)
	}
	local := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "313299b0a1b2c3d4e5f6"}, {Key: "vcs.modified", Value: "true"},
	}}
	if got := versionFrom("dev", local); got != "dev-313299b0a1b2-dirty" {
		t.Errorf("a local build names its commit, got %q", got)
	}
	if got := versionFrom("dev", &debug.BuildInfo{}); got != "dev" {
		t.Errorf("nothing known: %q", got)
	}
}
