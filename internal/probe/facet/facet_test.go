package facet

import (
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

var now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func has(conds []model.Condition, kind string) *model.Condition {
	for i := range conds {
		if conds[i].Kind == kind {
			return &conds[i]
		}
	}
	return nil
}

func TestWorkerCanonicalForm(t *testing.T) {
	o := &probe.Observation{Target: "workers"}
	EmitWorker(o, WorkerFacet{Online: NI(6), Total: NI(6), SlotsUsed: NI(24), SlotsMax: NI(24), Active: NI(24), Backlog: NI(850),
		Running:         []Task{{ID: "a", Name: "generate-invoice", Started: now.Add(-25 * time.Minute)}, {ID: "b", Name: "quick", Started: now.Add(-10 * time.Second)}},
		TypicalDuration: 90 * time.Second}, now)
	for k, want := range map[string]float64{"workers_online": 6, "pool_used": 24, "pool_max": 24, "active": 24, "waiters": 850, "p95_s": 90, "running_s": 1500} {
		if o.Metrics[k] != want {
			t.Errorf("%s = %v, want %v", k, o.Metrics[k], want)
		}
	}
	if c := has(o.Conditions, model.CondTaskRunning); c == nil || c.Detail != "generate-invoice" {
		t.Errorf("long task missing: %+v", o.Conditions)
	}
	if c := has(o.Conditions, model.CondPoolExhausted); c == nil || c.Detail != "all 24 slots busy" {
		t.Errorf("exhausted missing: %+v", o.Conditions)
	}
	o2 := &probe.Observation{Target: "w"}
	EmitWorker(o2, WorkerFacet{Online: NI(0), Total: NI(3)}, now)
	if _, ok := o2.Metrics["pool_max"]; ok {
		t.Error("pool_max must be omitted when unknown")
	}
	if _, ok := o2.Metrics["active"]; ok {
		t.Error("active must be omitted when unknown")
	}
	if has(o2.Conditions, model.CondNotReady) == nil {
		t.Error("no workers online must raise NotReady")
	}
}

func TestQueueReplicationJob(t *testing.T) {
	o := &probe.Observation{Target: "q"}
	EmitQueue(o, QueueFacet{Depth: NI(800), Pending: NI(50), Consumers: NI(6), Oldest: 14 * time.Minute, HasOldest: true, GrowthPerMin: N(30)}, now)
	if o.Metrics["depth"] != 850 || o.Metrics["pending"] != 50 || o.Metrics["oldest_age_s"] != 840 || o.Metrics["growth_per_min"] != 30 {
		t.Errorf("queue metrics %v", o.Metrics)
	}
	r := &probe.Observation{Target: "db-primary->electric"}
	EmitReplication(r, ReplicationFacet{Slot: "electric_slot_default", Streaming: false, WALRetained: N(12 << 30), BrokenSince: now.Add(-2 * time.Hour)}, now)
	if c := has(r.Conditions, model.CondReplicationBroken); c == nil || c.Ref != "slot/electric_slot_default" || c.Since.IsZero() {
		t.Errorf("replication broken missing: %+v", r.Conditions)
	}
	if has(r.Conditions, model.CondSlotInactive) == nil {
		t.Error("an inactive slot with retained WAL must raise SlotInactive")
	}
	ok := &probe.Observation{Target: "e"}
	EmitReplication(ok, ReplicationFacet{Slot: "s", Streaming: true, Lag: N(2048)}, now)
	if len(ok.Conditions) != 0 || ok.Metrics["lag_bytes"] != 2048 {
		t.Errorf("streaming replica should carry no conditions: %+v", ok)
	}
	j := &probe.Observation{Target: "billing"}
	EmitJob(j, JobFacet{Succeeded: NI(20), Failed: NI(3), LastFailure: &Failure{Ref: "run/8c1d", At: now.Add(-8 * time.Minute), Reason: "timed out"}, Schedule: "cron 0 * * * *"}, now)
	if c := has(j.Conditions, model.CondJobFailed); c == nil || c.Detail != "timed out" {
		t.Errorf("job failed missing: %+v", j.Conditions)
	}
	if j.Detail["schedule"] != "cron 0 * * * *" || j.Metrics["failed"] != 3 {
		t.Errorf("job detail %v %v", j.Detail, j.Metrics)
	}
}

func TestNodeWorkloadLoadBalancer(t *testing.T) {
	n := &probe.Observation{Target: "node-2"}
	EmitNode(n, NodeFacet{CPUPct: N(55), MemPct: N(96), Ready: true, MemoryPressure: true, PressureSince: now.Add(-10 * time.Minute), Killed: NI(2), BootedAt: now.Add(-3 * time.Hour)}, now)
	if has(n.Conditions, model.CondMemoryPressure) == nil || has(n.Conditions, model.CondRebooted) != nil || n.Metrics["killed"] != 2 {
		t.Errorf("node %+v %v", n.Conditions, n.Metrics)
	}
	n2 := &probe.Observation{Target: "node-3"}
	EmitNode(n2, NodeFacet{Ready: false, NotReadySince: now.Add(-14 * time.Minute), NotReadyDetail: "kubelet stopped posting", BootedAt: now.Add(-15 * time.Minute)}, now)
	if has(n2.Conditions, model.CondNotReady) == nil || has(n2.Conditions, model.CondRebooted) == nil {
		t.Errorf("node-3 %+v", n2.Conditions)
	}
	w := &probe.Observation{Target: "api"}
	EmitWorkload(w, WorkloadFacet{ReplicasReady: NI(2), ReplicasDesired: NI(3), Restarts: NI(6), Ready: true,
		Crashing: []Instance{{Ref: "pod/api-7d9f", Since: now.Add(-2 * time.Minute), Detail: "migration failed"}}}, now)
	if w.Metrics["restart_window_s"] != 300 || has(w.Conditions, model.CondCrashLoopBackOff) == nil {
		t.Errorf("workload %v %+v", w.Metrics, w.Conditions)
	}
	lb := &probe.Observation{Target: "lb"}
	targets := []Target{{ID: "node-1", Healthy: true}, {ID: "node-2", Healthy: true}, {ID: "node-3", Healthy: false, Detail: "tcp:6443 refused", Since: now.Add(-14 * time.Minute)}}
	EmitLoadBalancer(lb, LoadBalancerFacet{Rate: N(1100), Targets: targets}, now)
	if lb.Metrics["targets_healthy"] != 2 || lb.Metrics["targets_total"] != 3 {
		t.Errorf("lb %v", lb.Metrics)
	}
	edges := LoadBalancerEdges("lb", "hcloud.lb", targets, now)
	if len(edges) != 3 || edges[2].Target != "lb->node-3" || has(edges[2].Conditions, model.CondHealthCheckFailing) == nil || edges[0].Metrics["healthy"] != 1 {
		t.Errorf("edges %+v", edges)
	}
}

func TestCertificateExternalTrafficCache(t *testing.T) {
	c := &probe.Observation{Target: "ingress"}
	EmitIngress(c, IngressFacet{Rate: N(1100), Certificate: CertificateFacet{Known: true, NotAfter: now.Add(48 * time.Hour), RenewalFailed: &Failure{Ref: "certificate/tls", At: now.Add(-8 * 24 * time.Hour), Reason: "DNS challenge error"}}}, now)
	if c.Metrics["cert_days"] != 2 || has(c.Conditions, model.CondCertExpiring) == nil || has(c.Conditions, model.CondCertRenewalFailed) == nil {
		t.Errorf("cert %v %+v", c.Metrics, c.Conditions)
	}
	exp := &probe.Observation{Target: "ingress"}
	EmitCertificate(exp, CertificateFacet{Known: true, NotAfter: now.Add(-3 * time.Hour)}, now)
	if cc := has(exp.Conditions, model.CondCertExpired); cc == nil || !cc.Since.Equal(now.Add(-3*time.Hour)) || exp.Metrics["cert_days"] != 0 {
		t.Errorf("expired %v %+v", exp.Metrics, exp.Conditions)
	}
	e := &probe.Observation{Target: "github"}
	EmitExternal(e, ExternalFacet{LatencyMS: N(30000), TimeoutRate: N(100), TimingOut: true, TimingOutSince: now.Add(-12 * time.Minute), Detail: "HEAD timed out"}, now)
	if cc := has(e.Conditions, model.CondTimeout); cc == nil || cc.Detail != "HEAD timed out" {
		t.Errorf("external %+v", e.Conditions)
	}
	tr := &probe.Observation{Target: "api->db"}
	EmitTraffic(tr, TrafficFacet{Rate: N(800), Queued: NI(38), PoolUsed: NI(100), PoolMax: NI(100)}, now)
	if tr.Metrics["queued"] != 38 || tr.Metrics["waiters"] != 38 || has(tr.Conditions, model.CondPoolExhausted) == nil {
		t.Errorf("traffic %v %+v", tr.Metrics, tr.Conditions)
	}
	bl := &probe.Observation{Target: "fw->signoz"}
	EmitTraffic(bl, TrafficFacet{Blocked: true, BlockedRule: "allow-lb-only", BlockedSince: now.Add(-40 * time.Minute)}, now)
	if cc := has(bl.Conditions, model.CondFirewallDenied); cc == nil || cc.Detail != "allow-lb-only" {
		t.Errorf("blocked %+v", bl.Conditions)
	}
	ca := &probe.Observation{Target: "redis"}
	EmitCache(ca, CacheFacet{MemPct: N(100), HitRate: N(10), Full: true, Policy: "noeviction"}, now)
	if has(ca.Conditions, model.CondCacheFull) == nil || ca.Metrics["hit_rate"] != 10 {
		t.Errorf("cache %+v", ca)
	}
	st := &probe.Observation{Target: "vol"}
	EmitStorage(st, StorageFacet{UsedBytes: N(84), TotalBytes: N(100)}, now)
	if st.Metrics["disk_pct"] != 84 {
		t.Errorf("storage %v", st.Metrics)
	}
	ob := &probe.Observation{Target: "signoz"}
	EmitObservability(ob, ObservabilityFacet{IngestRate: N(0), NoDataSince: now.Add(-40 * time.Minute)}, now)
	if cc := has(ob.Conditions, model.CondNoData); cc == nil || !cc.Since.Equal(now.Add(-40*time.Minute)) {
		t.Errorf("observability %+v", ob.Conditions)
	}
}
