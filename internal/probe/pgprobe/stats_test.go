package pgprobe

import (
	"context"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// primary is what scenario 03 (primary disk filling) looks like from the
// primary: two replicas, one streaming and one whose slot went inactive.
var primary = Stats{
	Version:        "16.3",
	MaxConnections: 100,
	Connections:    42,
	Active:         7,
	LockWaiters:    2,
	Replication: []ReplicationRow{
		{ApplicationName: "bookstore-db-2", State: "streaming", LagBytes: 2048},
	},
	Slots: []SlotRow{
		{Name: "bookstore_db_2", Active: true, RetainedBytes: 2048},
		{Name: "bookstore_db_3", Active: false, RetainedBytes: 42949672960},
	},
	UsedBytes: 84 << 30,
	Waiting: []WaitingRow{
		{State: "active", WaitEvent: "transactionid", Query: "UPDATE invoices SET status=$1 WHERE id=$2", WaitS: 12.5},
	},
}

func TestObserveComponent(t *testing.T) {
	m, conds, detail := Observe(primary, StatsOptions{DiskTotalBytes: 100 << 30, Via: "k8s.workload/api"})
	want := map[string]float64{
		"connections_used": 42, "connections_max": 100, "active_connections": 7, "waiters": 2,
		"lag_bytes": 2048, "wal_retained_bytes": 42949672960, "used_bytes": 84 << 30, "disk_pct": 84, "total_bytes": 100 << 30,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if len(m) != len(want) {
		t.Errorf("extra metrics: %v", m)
	}
	if len(conds) != 0 {
		t.Errorf("component binding should not raise replica conditions: %+v", conds)
	}
	if detail["version"] != "16.3" || detail["via"] != "k8s.workload/api" {
		t.Errorf("detail = %+v", detail)
	}
	top, ok := detail["top_waiting"].([]map[string]any)
	if !ok || len(top) != 1 || top[0]["query"] != "UPDATE invoices SET status=$1 WHERE id=$2" {
		t.Errorf("top_waiting = %+v", detail["top_waiting"])
	}
	if slots, ok := detail["slots"].([]map[string]any); !ok || len(slots) != 2 {
		t.Errorf("slots = %+v", detail["slots"])
	}
}

func TestObserveComponentWithoutOptionals(t *testing.T) {
	m, _, detail := Observe(Stats{MaxConnections: 100, Connections: 100, Active: 92}, StatsOptions{})
	if m["connections_used"] != 100 || m["connections_max"] != 100 || m["active_connections"] != 92 {
		t.Errorf("metrics = %v", m)
	}
	for _, k := range []string{"lag_bytes", "wal_retained_bytes", "disk_pct", "used_bytes"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s should be omitted when unknown", k)
		}
	}
	if _, ok := detail["top_waiting"]; ok {
		t.Error("top_waiting should be omitted when nobody waits")
	}
}

func TestObserveReplicaStreaming(t *testing.T) {
	m, conds, detail := Observe(primary, StatsOptions{Replica: "bookstore-db-2"})
	if m["lag_bytes"] != 2048 || m["streaming"] != 1 || m["wal_retained_bytes"] != 2048 {
		t.Errorf("metrics = %v", m)
	}
	if len(conds) != 0 {
		t.Errorf("unexpected conditions %+v", conds)
	}
	if detail["state"] != "streaming" {
		t.Errorf("detail = %+v", detail)
	}
}

func TestObserveReplicaBroken(t *testing.T) {
	m, conds, _ := Observe(primary, StatsOptions{Replica: "bookstore-db-3"})
	if m["streaming"] != 0 || m["wal_retained_bytes"] != 42949672960 {
		t.Errorf("metrics = %v", m)
	}
	if _, ok := m["lag_bytes"]; ok {
		t.Error("lag_bytes unknown without a replication row")
	}
	rb, ok := model.HasCondition(conds, model.CondReplicationBroken)
	if !ok {
		t.Fatalf("ReplicationBroken missing: %+v", conds)
	}
	if rb.Ref != "slot/bookstore_db_3" || rb.Detail != "replication slot bookstore_db_3 inactive" {
		t.Errorf("ReplicationBroken = %+v", rb)
	}
	si, ok := model.HasCondition(conds, model.CondSlotInactive)
	if !ok || si.Ref != "slot/bookstore_db_3" {
		t.Errorf("SlotInactive = %+v %v", si, ok)
	}
}

func TestObserveReplicaUnknown(t *testing.T) {
	_, conds, _ := Observe(primary, StatsOptions{Replica: "ghost"})
	rb, ok := model.HasCondition(conds, model.CondReplicationBroken)
	if !ok || rb.Ref != "slot/ghost" {
		t.Fatalf("ReplicationBroken = %+v %v", rb, ok)
	}
	if _, ok := model.HasCondition(conds, model.CondSlotInactive); ok {
		t.Error("no slot means no SlotInactive")
	}
}

func TestStampKeepsFirstSince(t *testing.T) {
	since := map[string]time.Time{}
	t0 := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	c1 := []model.Condition{{Kind: model.CondSlotInactive, Ref: "slot/a"}}
	stamp(c1, since, t0)
	c2 := []model.Condition{{Kind: model.CondSlotInactive, Ref: "slot/a"}, {Kind: model.CondReplicationBroken, Ref: "slot/a"}}
	stamp(c2, since, t0.Add(time.Minute))
	if !c2[0].Since.Equal(t0) {
		t.Errorf("since moved: %v", c2[0].Since)
	}
	if !c2[1].Since.Equal(t0.Add(time.Minute)) {
		t.Errorf("new condition since = %v", c2[1].Since)
	}
	stamp(nil, since, t0.Add(2*time.Minute))
	if len(since) != 0 {
		t.Errorf("cleared conditions should be forgotten: %v", since)
	}
}

func TestSlotName(t *testing.T) {
	if got := slotName("Bookstore-DB-3"); got != "bookstore_db_3" {
		t.Errorf("slotName = %q", got)
	}
}

func TestStatsValidate(t *testing.T) {
	p := &StatsProbe{}
	if err := p.Validate(map[string]any{}); err == nil {
		t.Error("dsn_env required")
	}
	if err := p.Validate(map[string]any{"dsn_env": "PG_DSN", "disk_total_bytes": 100, "interval": "30s"}); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := p.Validate(map[string]any{"dsn_env": "PG_DSN", "disk_total_bytes": "big"}); err == nil {
		t.Error("bad disk_total_bytes")
	}
	if err := p.Validate(map[string]any{"dsn_env": "PG_DSN", "interval": "often"}); err == nil {
		t.Error("bad interval")
	}
}

func TestStartFailsWithoutEnv(t *testing.T) {
	p := &StatsProbe{}
	out := make(chan probe.Observation, 1)
	err := p.Start(context.Background(), map[string]any{"dsn_env": "WASSUP_TEST_MISSING_DSN"}, out)
	if err == nil {
		t.Fatal("missing env var should fail Start")
	}
	if h := p.Health(); h.State != probe.HealthFailed {
		t.Errorf("health = %+v", h)
	}
	t.Setenv("WASSUP_TEST_BAD_DSN", "postgres://user@host:notaport/db")
	if err := p.Start(context.Background(), map[string]any{"dsn_env": "WASSUP_TEST_BAD_DSN"}, out); err == nil {
		t.Error("invalid DSN should fail Start")
	}
}

func TestRunReemitsBetweenPolls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 64)
	polls := 0
	poll := func(context.Context) probe.Observation {
		polls++
		return probe.Observation{Target: "db", Metrics: map[string]float64{"n": float64(polls)}}
	}
	done := make(chan struct{})
	go func() {
		run(ctx, out, 5*time.Millisecond, 40*time.Millisecond, &connector{}, poll)
		close(done)
	}()
	var got []probe.Observation
	deadline := time.After(2 * time.Second)
	for len(got) < 12 {
		select {
		case o := <-out:
			got = append(got, o)
		case <-deadline:
			t.Fatalf("only %d observations", len(got))
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run did not stop on ctx cancel")
	}
	if got[0].Metrics["n"] != 1 {
		t.Errorf("first observation should come from the first poll: %v", got[0])
	}
	if polls >= len(got) {
		t.Errorf("expected re-emits between polls: polls=%d observations=%d", polls, len(got))
	}
	if got[1].At.IsZero() {
		t.Error("re-emitted observation should carry a fresh At")
	}
}
