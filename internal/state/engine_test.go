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
		{ID: "lb", Type: "loadbalancer", Label: "Load balancer"},
		{ID: "ingress", Type: "ingress", Label: "Ingress"},
		{ID: "api", Type: "workload", Label: "API", RunsOn: []string{"n1"}},
		{ID: "n1", Type: "node"},
		{ID: "db", Type: "database", Label: "Database"},
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
	cases := map[float64]string{38: "38", 1200: "1.2k", 2400: "2.4k", 1000: "1k", 0.2: "0.2", 3100000: "3.1M",
		// less than one keeps two digits that say something
		0: "0", 0.017: "0.017", 0.12: "0.12", 0.004: "0.004", 0.0123: "0.012", 0.5: "0.5", 0.96: "1", 1.04: "1", 1.26: "1.3", -0.017: "-0.017"}
	for in, want := range cases {
		if got := Num(in); got != want {
			t.Errorf("Num(%v) = %q, want %q", in, got, want)
		}
	}
	// a rate is said in the unit a person would count in
	rates := map[float64]string{40: "40 req/s", 1.26: "1.3 req/s", 0.96: "1 req/s", 0.24: "14.4 req/min", 0.017: "1 req/min",
		0.0042: "15.1 req/h", 0.0001: "0.36 req/h", 0: "0 req/s"}
	for in, want := range rates {
		if got := Rate(in, "req/s"); got != want {
			t.Errorf("Rate(%v) = %q, want %q", in, got, want)
		}
	}
	if got := Rate(0.5, "B lag"); got != "0.5 B lag" {
		t.Errorf("what is no rate keeps its unit: %q", got)
	}
	if Dur(35*time.Minute) != "35 min" || Dur(3*time.Hour) != "3 h" || Dur(3*time.Hour+2*time.Minute) != "3 h" || Dur(90*time.Second) != "2 min" {
		t.Errorf("Dur: %s %s %s", Dur(35*time.Minute), Dur(3*time.Hour), Dur(90*time.Second))
	}
	if Bytes(40<<30) != "40 GB" || Bytes(2048) != "2 kB" {
		t.Errorf("Bytes: %s %s", Bytes(40<<30), Bytes(2048))
	}
}

func TestHostedRowsFollowPlacement(t *testing.T) {
	// The api runs on n1 by topology; the cluster also sees a pod on n2,
	// crash-looping. n1's row must read flowing (its pod is fine) and n2 must
	// list the api although runs_on never named it.
	tp := topo()
	tp.Components = append(tp.Components, model.Component{ID: "n2", Type: "node"})
	b := bind.New()
	b.Apply(probe.Observation{Target: "api", At: now,
		Metrics:    map[string]float64{"replicas_ready": 1, "replicas_desired": 2, "restarts": 5, "restart_window_s": 300},
		Conditions: []model.Condition{{Kind: model.CondCrashLoopBackOff, Ref: "pod/api-2", Since: now.Add(-time.Minute)}},
		Detail: map[string]any{"placement": map[string]any{
			"n1": map[string]any{"pods": 1.0, "ready": 1.0},
			"n2": map[string]any{"pods": 1.0, "ready": 0.0, "restarts": 5.0},
		}}})
	for _, n := range []string{"n1", "n2"} {
		b.Apply(probe.Observation{Target: n, At: now, Metrics: map[string]float64{"cpu_pct": 30, "mem_pct": 40, "disk_pct": 50}})
	}
	s := Evaluate(Input{Topology: tp, Joined: b.All(), Now: now, Tick: 1, TickEvery: 5 * time.Second})
	if s.Components["api"].State != model.Failing {
		t.Fatalf("api should be failing, got %s", s.Components["api"].State)
	}
	n1 := s.Components["n1"].Hosted
	if len(n1) != 1 || n1[0].ID != "api" || !n1[0].Known || n1[0].State != model.Flowing || n1[0].Pods != 1 || n1[0].Ready != 1 {
		t.Errorf("n1 rows = %+v", n1)
	}
	n2 := s.Components["n2"].Hosted
	if len(n2) != 1 || n2[0].ID != "api" || !n2[0].Known || n2[0].State != model.Failing || n2[0].Ready != 0 || n2[0].Restarts != 5 {
		t.Errorf("n2 rows = %+v", n2)
	}
	// Without placement the row comes from runs_on alone and takes the
	// component's state.
	s = eval(t, probe.Observation{Target: "api", Metrics: map[string]float64{"replicas_ready": 2, "replicas_desired": 2}},
		probe.Observation{Target: "n1", Metrics: map[string]float64{"cpu_pct": 30}})
	rows := s.Components["n1"].Hosted
	if len(rows) != 1 || rows[0].Known || rows[0].Label != "API" {
		t.Errorf("runs_on rows = %+v", rows)
	}
}

// A probe that failed, timed out or may not read its source delivered
// nothing. The element says "no data" with the reason; failing is only ever
// concluded from data that was read.
func TestProbeFailureIsNoDataNeverFailing(t *testing.T) {
	refused := "failed to connect to `host=127.0.0.1 user=bookstore database=bookstore`: dial error: dial tcp 127.0.0.1:55432: connect: connection refused"
	s := eval(t,
		probe.Observation{Target: "api", Probe: "k8s.workload", Metrics: map[string]float64{"replicas_ready": 3, "replicas_desired": 3, "rate": 40}},
		probe.Observation{Target: "db", Probe: "pg.stats", Err: refused},
		probe.Observation{Target: "q", Probe: "redis.list", Metrics: map[string]float64{"depth": 0}},
		probe.Observation{Target: "api->q", Probe: "pg.pool", Err: "permission denied for relation pg_stat_replication"},
	)
	db := s.Components["db"]
	if db.State == model.Failing || db.Marker != model.MarkerUnbound || db.Severity != model.Info {
		t.Errorf("db = %+v", db)
	}
	if db.Label != "no data, connection refused" {
		t.Errorf("db label = %q", db.Label)
	}
	if errs, _ := db.Detail["probe_errors"].([]string); len(errs) != 1 || !strings.Contains(errs[0], "pg.stats: "+refused) {
		t.Errorf("the whole error belongs in the detail: %+v", db.Detail["probe_errors"])
	}
	// Both ends of api->q are bound and api has a rate the edge could
	// borrow. Its own probe failed, so it says that instead.
	e := s.Edges["api->q"]
	if e.Marker != model.MarkerNoData || e.State != model.Idle || e.Severity != model.Info {
		t.Errorf("edge = %+v", e)
	}
	if e.Label != "no data, permission denied for relation pg_stat_replication" {
		t.Errorf("edge label = %q", e.Label)
	}
	if len(s.Issues) != 0 {
		t.Errorf("a failed probe is not an issue of the system: %+v", s.Issues)
	}
}

// One probe of several failing keeps the element bound on what the others
// read, and the failure stays visible in the detail.
func TestPartialProbeFailureKeepsTheData(t *testing.T) {
	s := eval(t,
		probe.Observation{Target: "api", Probe: "k8s.workload", Metrics: map[string]float64{"replicas_ready": 3, "replicas_desired": 3}},
		probe.Observation{Target: "db", Probe: "cnpg.cluster", Metrics: map[string]float64{"replicas_ready": 2, "replicas_desired": 2}},
		probe.Observation{Target: "db", Probe: "pg.stats", Err: "connection refused"},
		probe.Observation{Target: "api->db", Probe: "pg.pool", Metrics: map[string]float64{"rate": 12, "pool_used": 3, "pool_max": 20}},
		probe.Observation{Target: "api->db", Probe: "signoz.edge", Err: "timeout"},
	)
	if db := s.Components["db"]; db.Marker != "" || db.Metrics["replicas_ready"] != 2 {
		t.Errorf("db = %+v", db)
	} else if errs, _ := db.Detail["probe_errors"].([]string); len(errs) != 1 || errs[0] != "pg.stats: connection refused" {
		t.Errorf("probe_errors = %+v", db.Detail["probe_errors"])
	}
	if e := s.Edges["api->db"]; e.State != model.Flowing || e.Marker != "" {
		t.Errorf("edge = %+v", e)
	} else if errs, _ := e.Detail["probe_errors"].([]string); len(errs) != 1 {
		t.Errorf("probe_errors = %+v", e.Detail["probe_errors"])
	}
}

// up binds the components of topo with what a cluster alone says: they
// run, and nothing says how much goes through them.
func up() []probe.Observation {
	return []probe.Observation{
		{Target: "api", Probe: "k8s.workload", Metrics: map[string]float64{"replicas_ready": 2, "replicas_desired": 2}},
		{Target: "n1", Probe: "k8s.node", Metrics: map[string]float64{"cpu_pct": 20, "mem_pct": 30}},
		{Target: "db", Probe: "cnpg.cluster", Metrics: map[string]float64{"cpu_pct": 10}},
		{Target: "ingress", Probe: "k8s.ingress", Metrics: map[string]float64{"cert_days": 60}},
	}
}

func TestAComponentSaysItsOwnRateInItsOwnUnit(t *testing.T) {
	s := eval(t, append(up(),
		probe.Observation{Target: "db", Probe: "pg.stats", Metrics: map[string]float64{"rate": 95}},
		probe.Observation{Target: "api->db", Probe: "pg.pool", Metrics: map[string]float64{"rate": 120}},
		probe.Observation{Target: "ingress->api", Probe: "signoz.edge", Metrics: map[string]float64{"rate": 40}},
	)...)
	if got := s.Components["db"].Label; got != "flowing, 95 tx/s" {
		t.Errorf("the database counts its transactions itself: %q", got)
	}
	// the API reports no rate: what is sent to it stands in, not the queries
	// it runs, which are the database's work
	if got := s.Components["api"].Label; got != "flowing, 40 req/s" {
		t.Errorf("api: %q", got)
	}
	// a rate of nothing of its own does not hide an edge that flows
	s = eval(t, append(up(),
		probe.Observation{Target: "db", Probe: "pg.stats", Metrics: map[string]float64{"rate": 0}},
		probe.Observation{Target: "api->db", Probe: "pg.pool", Metrics: map[string]float64{"rate": 120}},
	)...)
	if got := s.Components["db"].Label; got != "flowing, 120 tx/s" {
		t.Errorf("db: %q", got)
	}
}

func TestIdleIsOnlySaidWhereSomethingWasMeasured(t *testing.T) {
	s := eval(t, up()...)
	for _, id := range []string{"api", "db", "ingress"} {
		if es := s.Components[id]; es.State != model.Idle || es.Label != NoRate || es.Marker != model.MarkerUnmetered {
			t.Errorf("%s: nothing measures it: %s %q %q", id, es.State, es.Label, es.Marker)
		}
	}
	// a machine that is up, with nothing on it that anybody counts, says up
	if es := s.Components["n1"]; es.State != model.Idle || es.Label != "up" || es.Marker != model.MarkerUnmetered {
		t.Errorf("n1: %s %q %q", es.State, es.Label, es.Marker)
	}
	for _, id := range []string{"ingress->api", "api->db"} {
		if es := s.Edges[id]; es.State != model.Idle || es.Label != NoRate || es.Marker != model.MarkerUnmetered {
			t.Errorf("%s: nothing measures it: %s %q %q", id, es.State, es.Label, es.Marker)
		}
	}
	if sev, _, _ := WorstSeverity(s); sev != model.Info {
		t.Errorf("not knowing a rate is no trouble: %v", sev)
	}

	s = eval(t, append(up(),
		probe.Observation{Target: "ingress->api", Probe: "signoz.edge", Metrics: map[string]float64{"rate": 0}},
		probe.Observation{Target: "q", Probe: "celery.queue", Metrics: map[string]float64{"depth": 0}},
	)...)
	if es := s.Edges["ingress->api"]; es.State != model.Idle || es.Label != "idle" {
		t.Errorf("a rate of nothing was read: %s %q", es.State, es.Label)
	}
	for _, id := range []string{"api", "ingress", "q"} {
		if es := s.Components[id]; es.Label != "idle" {
			t.Errorf("%s: what touches it was measured at nothing: %q", id, es.Label)
		}
	}
	if es := s.Components["db"]; es.Label != NoRate {
		t.Errorf("db: nothing measures it still: %q", es.Label)
	}
	if es := s.Edges["api->db"]; es.Label != NoRate {
		t.Errorf("api->db: %q", es.Label)
	}
}

func TestAMachineIsBusyWhenWhatRunsOnItIs(t *testing.T) {
	// no edge touches n1: all it has is the API that runs on it
	s := eval(t, append(up(), probe.Observation{Target: "ingress->api", Probe: "signoz.edge", Metrics: map[string]float64{"rate": 40}})...)
	if es := s.Components["n1"]; es.State != model.Flowing || es.Label != "flowing" {
		t.Errorf("the API on it flows: %s %q", es.State, es.Label)
	}
	s = eval(t, append(up(), probe.Observation{Target: "ingress->api", Probe: "signoz.edge", Metrics: map[string]float64{"rate": 0}})...)
	if es := s.Components["n1"]; es.State != model.Idle || es.Label != "idle" {
		t.Errorf("the API on it was measured at nothing: %s %q", es.State, es.Label)
	}
	s = eval(t, append(up(), probe.Observation{Target: "api", Probe: "k8s.workload",
		Metrics:    map[string]float64{"replicas_ready": 0, "replicas_desired": 2},
		Conditions: []model.Condition{{Kind: model.CondCrashLoopBackOff, Ref: "pod/api-1", Since: now.Add(-time.Minute)}}})...)
	if es := s.Components["n1"]; es.State == model.Failing || es.State == model.Flowing {
		t.Errorf("a machine does not fail, and is not busy, because what runs on it crashes: %s %q", es.State, es.Label)
	}
}

// The cluster knows a machine by a name of its own. What runs on it is
// counted on the machine whose probe reported that name.
func TestPlacementFindsTheMachineByTheNameTheClusterKnows(t *testing.T) {
	s := eval(t,
		probe.Observation{Target: "n1", Probe: "k8s.node", Metrics: map[string]float64{"cpu_pct": 20},
			Detail: map[string]any{"name": "bookstore-agent-large-x7k"}},
		probe.Observation{Target: "api", Probe: "k8s.workload", Metrics: map[string]float64{"replicas_ready": 2, "replicas_desired": 2},
			Detail: map[string]any{"placement": map[string]any{"bookstore-agent-large-x7k": map[string]any{"pods": 2, "ready": 2}}}},
	)
	hosted := s.Components["n1"].Hosted
	if len(hosted) != 1 || hosted[0].ID != "api" {
		t.Fatalf("hosted = %+v", hosted)
	}
	if h := hosted[0]; !h.Known || h.Pods != 2 || h.Ready != 2 {
		t.Errorf("the API on n1 = %+v, want its 2 pods of 2 ready", h)
	}
}

// A machine knows what goes through it only when that is known of
// everything on it: a probe that failed says nothing, and so does the
// machine.
func TestAMachineWithSomethingUnknownOnItDoesNotSayIdle(t *testing.T) {
	s := eval(t,
		probe.Observation{Target: "n1", Probe: "k8s.node", Metrics: map[string]float64{"cpu_pct": 20}},
		probe.Observation{Target: "api", Probe: "k8s.workload", Err: "connection refused"},
	)
	if es := s.Components["api"]; es.Marker != model.MarkerUnbound {
		t.Fatalf("api = %s %q %q", es.State, es.Label, es.Marker)
	}
	if es := s.Components["n1"]; es.State != model.Idle || es.Label != "up" || es.Marker != model.MarkerUnmetered {
		t.Errorf("nothing was measured on n1: %s %q %q", es.State, es.Label, es.Marker)
	}
}

// A database with roles takes the state of its primary. A machine that
// holds the database reads what the database reads.
func TestAMachineReadsTheDatabaseOnItAfterItTookItsPrimarysState(t *testing.T) {
	top := topo()
	for i := range top.Components {
		if top.Components[i].ID == "db" {
			// three instances on one machine named: nothing says which holds
			// which, so the machine holds the database as a whole
			top.Components[i].Roles = &model.Roles{Primary: "db-primary", Replicas: []string{"db-r1", "db-r2"}}
			top.Components[i].RunsOn = []string{"n1"}
		}
		if top.Components[i].ID == "api" {
			top.Components[i].RunsOn = nil
		}
	}
	b := bind.New()
	for _, o := range []probe.Observation{
		{Target: "n1", Probe: "k8s.node", Metrics: map[string]float64{"cpu_pct": 20}},
		{Target: "db-primary", Probe: "pg.stats", Metrics: map[string]float64{"rate": 850, "connections_used": 4}},
	} {
		o.At = now
		b.Apply(o)
	}
	s := Evaluate(Input{Topology: top, Joined: b.All(), Now: now, Tick: 1, TickEvery: 5 * time.Second})
	if es := s.Components["db"]; es.State != model.Flowing {
		t.Fatalf("db = %s %q", es.State, es.Label)
	}
	if es := s.Components["n1"]; es.State != model.Flowing {
		t.Errorf("the database on n1 flows: n1 = %s %q", es.State, es.Label)
	}
}

// A workload that says where its pods are says where they are not as well:
// on a machine its runs_on names and its placement does not, there is none,
// and that is known. The diagram leaves the place empty instead of drawing
// a copy that is not there.
func TestAMachineKnowsWhatIsNotOnIt(t *testing.T) {
	tp := &model.Topology{Name: "t", Components: []model.Component{
		{ID: "n1", Type: "node"}, {ID: "n2", Type: "node"},
		{ID: "api", Type: "workload", Label: "API", RunsOn: []string{"n1", "n2"}},
	}}
	b := bind.New()
	for _, o := range []probe.Observation{
		{Target: "n1", Probe: "k8s.node", Metrics: map[string]float64{"cpu_pct": 20}},
		{Target: "n2", Probe: "k8s.node", Metrics: map[string]float64{"cpu_pct": 20}},
		{Target: "api", Probe: "k8s.workload", Metrics: map[string]float64{"replicas_ready": 1, "replicas_desired": 1},
			Detail: map[string]any{"placement": map[string]any{"n1": map[string]any{"pods": 1, "ready": 1}}}},
	} {
		o.At = now
		b.Apply(o)
	}
	s := Evaluate(Input{Topology: tp, Joined: b.All(), Now: now, Tick: 1, TickEvery: 5 * time.Second})
	here, there := s.Components["n1"].Hosted, s.Components["n2"].Hosted
	if len(here) != 1 || !here[0].Known || here[0].Pods != 1 {
		t.Errorf("n1 holds the pod: %+v", here)
	}
	if len(there) != 1 || there[0].ID != "api" || !there[0].Known || there[0].Pods != 0 || there[0].State != model.Idle {
		t.Errorf("n2 holds none, and that is known: %+v", there)
	}
}

// Idle says when work was last seen, where a probe knows: a call that
// SigNoz counted three hours ago, a job that ran twelve minutes ago.
func TestIdleSaysWhenWorkWasLastSeen(t *testing.T) {
	s := eval(t, append(up(),
		probe.Observation{Target: "ingress->api", Probe: "signoz.edge", Metrics: map[string]float64{"rate": 0},
			Detail: map[string]any{"last_seen": now.Add(-3 * time.Hour).UTC().Format(time.RFC3339)}},
		probe.Observation{Target: "api->db", Probe: "signoz.edge", Metrics: map[string]float64{"rate": 0},
			Detail: map[string]any{"last_seen": now.Add(-10 * time.Minute).UTC().Format(time.RFC3339), "unit": "queries/s"}},
	)...)
	if got := s.Edges["ingress->api"].Label; got != "idle, last call 3 h ago" {
		t.Errorf("ingress->api: %q", got)
	}
	if got := s.Edges["api->db"].Label; got != "idle, last query 10 min ago" {
		t.Errorf("api->db: %q", got)
	}
	if got := s.Components["api"].Label; got != "idle, last call 3 h ago" {
		t.Errorf("api: %q, want what was last sent to it", got)
	}
}

// A probe that knows what it counts says the unit, and the edge takes it.
func TestAnEdgeTakesTheUnitItsProbeSays(t *testing.T) {
	s := eval(t, append(up(),
		probe.Observation{Target: "api->db", Probe: "signoz.edge", Metrics: map[string]float64{"rate": 55}, Detail: map[string]any{"unit": "queries/s"}},
	)...)
	if got := s.Edges["api->db"].Label; got != "flowing, 55 queries/s" {
		t.Errorf("api->db: %q", got)
	}
}

// A scheduled job is idle between its runs and says when it last ran, or
// that it did not run in the window its counts cover.
func TestAScheduledJobSaysWhenItRan(t *testing.T) {
	topo := &model.Topology{Components: []model.Component{{ID: "sweep", Type: "scheduledjob"}, {ID: "nightly", Type: "scheduledjob"}}}
	b := bind.New()
	b.Apply(probe.Observation{Target: "sweep", Probe: "hatchet.workflow", At: now, Metrics: map[string]float64{"active": 0, "succeeded": 24, "failed": 0},
		Detail: map[string]any{"last_run": now.Add(-12 * time.Minute), "window": "24h0m0s"}})
	b.Apply(probe.Observation{Target: "nightly", Probe: "hatchet.workflow", At: now, Metrics: map[string]float64{"active": 0, "succeeded": 0, "failed": 0},
		Detail: map[string]any{"window": "24h0m0s"}})
	s := Evaluate(Input{Topology: topo, Now: now, Joined: b.All(), Tick: 1, TickEvery: 5 * time.Second})
	if got := s.Components["sweep"].Label; got != "idle, ran 12 min ago" {
		t.Errorf("sweep: %q", got)
	}
	if got := s.Components["nightly"].Label; got != "idle, no run in 24 h" {
		t.Errorf("nightly: %q", got)
	}
}

// A service of others is failing from pings when they fail more than once,
// and not while the system's own calls to it are counted and succeed: a ping
// says what wassup's machine sees, the calls what the system does.
func TestAPingThatTimedOutOnceDoesNotFailAServiceOfOthers(t *testing.T) {
	topo := &model.Topology{Components: []model.Component{
		{ID: "api", Type: "workload"}, {ID: "pay", Type: "external", Label: "Payments"},
	}, Edges: []model.Edge{{From: "api", To: "pay", Kind: "external"}}}
	run := func(obs ...probe.Observation) *model.Snapshot {
		b := bind.New()
		for _, o := range obs {
			o.At = now
			b.Apply(o)
		}
		return Evaluate(Input{Topology: topo, Joined: b.All(), Now: now, Tick: 1, TickEvery: 5 * time.Second})
	}
	ping := func(timeouts float64) probe.Observation {
		return probe.Observation{Target: "pay", Probe: "http.ping", Metrics: map[string]float64{"timeout_rate": timeouts, "error_rate": timeouts, "latency_ms": 400}, Detail: map[string]any{"window": 10}}
	}
	if s := run(ping(10)); s.Components["pay"].State == model.Failing {
		t.Errorf("one timeout in ten: %q", s.Components["pay"].Label)
	}
	if s := run(ping(30)); s.Components["pay"].Label != "failing, 30 percent timeouts" {
		t.Errorf("three in ten: %q", s.Components["pay"].Label)
	}
	calls := probe.Observation{Target: "api->pay", Probe: "signoz.edge", Metrics: map[string]float64{"rate": 0.02, "error_rate": 0}}
	if s := run(ping(30), calls); s.Components["pay"].State == model.Failing {
		t.Errorf("the API's calls succeed: %q", s.Components["pay"].Label)
	}
}
