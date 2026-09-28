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

func TestWorkerPoolCanonicalForm(t *testing.T) {
	o := &probe.Observation{Target: "workers"}
	EmitWorkerPool(o, WorkerPool{Known: true, Online: 6, Total: 6, SlotsUsed: 24, SlotsMax: 24, Active: 24, HasActive: true, Backlog: 850, HasBacklog: true,
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
	if has(o.Conditions, model.CondNotReady) != nil {
		t.Error("workers are online")
	}
	// No slots notion: pool metrics are omitted, not zero.
	o2 := &probe.Observation{Target: "w"}
	EmitWorkerPool(o2, WorkerPool{Known: true, Online: 0, Total: 3}, now)
	if _, ok := o2.Metrics["pool_max"]; ok {
		t.Error("pool_max must be omitted when unknown")
	}
	if has(o2.Conditions, model.CondNotReady) == nil {
		t.Error("no workers online must raise NotReady")
	}
}

func TestQueueAndReplicationAndJob(t *testing.T) {
	o := &probe.Observation{Target: "q"}
	EmitQueue(o, Queue{Known: true, Depth: 800, Pending: 50, HasPending: true, Consumers: 6, HasConsumers: true, Oldest: 14 * time.Minute, HasOldest: true, GrowthPerMin: 30, HasGrowth: true}, now)
	if o.Metrics["depth"] != 850 || o.Metrics["pending"] != 50 || o.Metrics["oldest_age_s"] != 840 || o.Metrics["growth_per_min"] != 30 {
		t.Errorf("queue metrics %v", o.Metrics)
	}
	r := &probe.Observation{Target: "db-primary->electric"}
	EmitReplication(r, Replication{Known: true, Slot: "electric_slot_default", Streaming: false, WALRetained: 12 << 30, HasWALRetained: true, BrokenSince: now.Add(-2 * time.Hour)}, now)
	if c := has(r.Conditions, model.CondReplicationBroken); c == nil || c.Ref != "slot/electric_slot_default" || c.Since.IsZero() {
		t.Errorf("replication broken missing: %+v", r.Conditions)
	}
	if r.Metrics["streaming"] != 0 || r.Metrics["wal_retained_bytes"] != float64(12<<30) {
		t.Errorf("replication metrics %v", r.Metrics)
	}
	ok := &probe.Observation{Target: "e"}
	EmitReplication(ok, Replication{Known: true, Slot: "s", Streaming: true, Lag: 2048, HasLag: true}, now)
	if len(ok.Conditions) != 0 || ok.Metrics["lag_bytes"] != 2048 {
		t.Errorf("streaming replica should carry no conditions: %+v", ok)
	}
	j := &probe.Observation{Target: "billing"}
	EmitJob(j, Job{Known: true, Succeeded: 20, Failed: 3, LastFailure: &Failure{Ref: "run/8c1d", At: now.Add(-8 * time.Minute), Reason: "timed out"}, Schedule: "cron 0 * * * *"}, now)
	if c := has(j.Conditions, model.CondJobFailed); c == nil || c.Detail != "timed out" {
		t.Errorf("job failed missing: %+v", j.Conditions)
	}
	if j.Detail["schedule"] != "cron 0 * * * *" || j.Metrics["failed"] != 3 {
		t.Errorf("job detail %v %v", j.Detail, j.Metrics)
	}
}
