package pgprobe

import (
	"context"
	"strings"
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

// restricted is a healthy primary as a role without pg_read_all_stats sees
// it: the walsenders are listed, their state and lag are NULL, and the slots
// say the rest. The first walsender is the sync engine's, which sets no
// application name.
var restricted = Stats{
	Version:        "16.3",
	MaxConnections: 100,
	Connections:    42,
	ActivityHidden: true,
	Replication: []ReplicationRow{
		{PID: 101, ApplicationName: "", Hidden: true, LagHidden: true},
		{PID: 102, ApplicationName: "bookstore-db-2", Hidden: true, LagHidden: true},
	},
	Slots: []SlotRow{
		{Name: "_cnpg_bookstore_db_2", Type: "physical", Active: true, ActivePID: 102, RetainedBytes: 56, LagBytes: 56, LagKnown: true},
		{Name: "electric_slot_default", Type: "logical", Active: true, ActivePID: 101, RetainedBytes: 56, LagBytes: 24, LagKnown: true},
	},
}

func TestObserveReplicaStateHiddenIsNotBroken(t *testing.T) {
	for _, tc := range []struct {
		replica string
		lag     float64
	}{
		{"bookstore-db-2", 56},
		{"electric_slot_default", 24},
	} {
		m, conds, detail := Observe(restricted, StatsOptions{Replica: tc.replica})
		if len(conds) != 0 {
			t.Errorf("%s: a state the role may not see is not a broken replica: %+v", tc.replica, conds)
		}
		if m["streaming"] != 1 {
			t.Errorf("%s: the slot is active, streaming = %v", tc.replica, m["streaming"])
		}
		if lag, ok := m["lag_bytes"]; !ok || lag != tc.lag {
			t.Errorf("%s: lag_bytes = %v %v, want %v from the slot", tc.replica, lag, ok, tc.lag)
		}
		if detail["lag_source"] != "pg_replication_slots" {
			t.Errorf("%s: lag_source = %v", tc.replica, detail["lag_source"])
		}
		if note, _ := detail["note"].(string); !strings.Contains(note, "pg_read_all_stats") {
			t.Errorf("%s: the detail should say why the numbers come from the slot, got %q", tc.replica, note)
		}
		if _, ok := detail["state"]; ok {
			t.Errorf("%s: a hidden state is not reported: %v", tc.replica, detail["state"])
		}
	}
}

func TestObserveSlotActivityComesFromTheSlot(t *testing.T) {
	s := restricted
	s.Slots = []SlotRow{
		{Name: "_cnpg_bookstore_db_2", Type: "physical", Active: false, RetainedBytes: 1 << 30, LagBytes: 1 << 30, LagKnown: true},
	}
	s.Replication = nil
	m, conds, _ := Observe(s, StatsOptions{Replica: "bookstore-db-2"})
	if m["streaming"] != 0 {
		t.Errorf("an inactive slot is not streaming: %v", m)
	}
	rb, ok := model.HasCondition(conds, model.CondReplicationBroken)
	if !ok || rb.Ref != "slot/_cnpg_bookstore_db_2" {
		t.Errorf("ReplicationBroken = %+v %v", rb, ok)
	}
	// A walsender whose state is visible and wrong is broken as before, even
	// on an active slot.
	s = restricted
	s.Replication = []ReplicationRow{{PID: 102, ApplicationName: "bookstore-db-2", State: "startup", LagBytes: 10}}
	if _, conds, _ := Observe(s, StatsOptions{Replica: "bookstore-db-2"}); len(conds) == 0 {
		t.Error("a visible state that is not streaming is broken")
	}
	// A connected walsender without a slot and with a hidden state is
	// connected; nothing read says it is broken, and its lag is not known.
	s = Stats{Replication: []ReplicationRow{{PID: 7, ApplicationName: "bookstore-db-2", Hidden: true, LagHidden: true}}}
	m, conds, _ = Observe(s, StatsOptions{Replica: "bookstore-db-2"})
	if len(conds) != 0 || m["streaming"] != 1 {
		t.Errorf("connected walsender: %v %+v", m, conds)
	}
	if _, ok := m["lag_bytes"]; ok {
		t.Error("lag_bytes must be omitted when neither view shows it")
	}
}

func TestReplicaMatchesSlotApplicationOrInstance(t *testing.T) {
	for _, replica := range []string{
		"_cnpg_bookstore_db_2", // the slot name
		"bookstore-db-2",       // the application_name and the CloudNativePG instance
		"bookstore_db_2",       // the instance, normalized
		"Bookstore-DB-2",
	} {
		slot, ok := findSlot(restricted.Slots, replica)
		if !ok || slot.Name != "_cnpg_bookstore_db_2" {
			t.Errorf("findSlot(%q) = %+v %v", replica, slot, ok)
		}
		rep, ok := findReplication(restricted.Replication, replica, SlotRow{}, false)
		if !ok || rep.PID != 102 {
			t.Errorf("findReplication(%q) = %+v %v", replica, rep, ok)
		}
	}
	// The walsender of a slot is found through the slot's pid, whatever its
	// application name is.
	slot, _ := findSlot(restricted.Slots, "electric_slot_default")
	if rep, ok := findReplication(restricted.Replication, "electric_slot_default", slot, true); !ok || rep.PID != 101 {
		t.Errorf("walsender of the sync slot = %+v %v", rep, ok)
	}
	if _, ok := findSlot(restricted.Slots, "bookstore-db-3"); ok {
		t.Error("another instance must not match")
	}
	if _, ok := findReplication(restricted.Replication, "ghost", SlotRow{}, false); ok {
		t.Error("an empty application name must not match by accident")
	}
}

func TestObserveComponentWithHiddenColumns(t *testing.T) {
	m, conds, detail := Observe(restricted, StatsOptions{})
	if len(conds) != 0 {
		t.Errorf("conditions = %+v", conds)
	}
	if m["connections_used"] != 42 {
		t.Errorf("connections_used = %v", m["connections_used"])
	}
	for _, k := range []string{"active_connections", "waiters"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s was not readable and must be omitted, not 0", k)
		}
	}
	if m["lag_bytes"] != 56 || detail["lag_source"] != "pg_replication_slots" {
		t.Errorf("lag should come from the active slots: %v %v", m["lag_bytes"], detail["lag_source"])
	}
	if _, ok := m["wal_retained_bytes"]; ok {
		t.Error("no slot is inactive, nothing is retained for one")
	}
	reps := detail["replicas"].([]map[string]any)
	if reps[1]["state"] != "not visible to this role" {
		t.Errorf("replicas = %+v", reps)
	}
	if _, ok := reps[1]["lag_bytes"]; ok {
		t.Errorf("a hidden lag is not listed: %+v", reps[1])
	}
}

func TestRowsKeepNullApartFromZero(t *testing.T) {
	pid, lag, state := int64(9), 0.0, "streaming"
	if r := replicationRow(&pid, "a", nil, nil); !r.Hidden || !r.LagHidden || r.PID != 9 {
		t.Errorf("NULL state and lag: %+v", r)
	}
	if r := replicationRow(&pid, "a", &state, &lag); r.Hidden || r.LagHidden || r.State != "streaming" {
		t.Errorf("visible state and a lag of 0: %+v", r)
	}
	if r := slotRow("s", "logical", true, nil, nil, nil); r.LagKnown || r.ActivePID != 0 {
		t.Errorf("a slot without a position: %+v", r)
	}
	if r := slotRow("s", "logical", true, &pid, &lag, &lag); !r.LagKnown || r.ActivePID != 9 {
		t.Errorf("a slot at the current position: %+v", r)
	}
}

func TestTransactionsBecomeARateBetweenTwoRounds(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	var tx txCounter
	if _, ok := tx.rate(1000, 0, at); ok {
		t.Error("one reading of a counter is no rate")
	}
	if r, ok := tx.rate(1500, 0, at.Add(10*time.Second)); !ok || r != 50 {
		t.Errorf("500 more in 10 s: %v %v", r, ok)
	}
	if r, ok := tx.rate(1501, 0, at.Add(70*time.Second)); !ok || r != 0 {
		t.Errorf("one more in a minute rounds to none, and is known: %v %v", r, ok)
	}
	if r, ok := tx.rate(1501, 0, at.Add(80*time.Second)); !ok || r != 0 {
		t.Errorf("nothing more: %v %v", r, ok)
	}
	// the statistics were reset, or the connection reached another server
	if _, ok := tx.rate(12, 0, at.Add(90*time.Second)); ok {
		t.Error("a counter that went down is no basis for a rate")
	}
	if r, ok := tx.rate(112, 0, at.Add(100*time.Second)); !ok || r != 10 {
		t.Errorf("the round after the reset counts from the reset: %v %v", r, ok)
	}
	// two readings of the same moment
	if _, ok := tx.rate(200, 0, at.Add(100*time.Second)); ok {
		t.Error("no time went by")
	}
}

// Every round of wassup ends with a commit the database counts. A database
// nobody else uses reads nothing, whatever the number of bindings on it.
func TestTheRateLeavesOutTheRoundsOfWassupItself(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	var tx txCounter
	tx.rate(1000, 0, at)
	// four bindings, a round each every 5 s, and nobody else
	if r, ok := tx.rate(1004, 4, at.Add(5*time.Second)); !ok || r != 0 {
		t.Errorf("4 more, all of them wassup's: %v %v", r, ok)
	}
	if r, ok := tx.rate(1058, 8, at.Add(10*time.Second)); !ok || r != 10 {
		t.Errorf("54 more, 4 of them wassup's: %v %v", r, ok)
	}
	// a round of another binding that the database has not counted yet
	if r, ok := tx.rate(1061, 12, at.Add(15*time.Second)); !ok || r != 0 {
		t.Errorf("fewer than wassup's own is none, not less than none: %v %v", r, ok)
	}

	led := &ownRounds{n: map[string]float64{}}
	led.add("k8s.service/shop/db-rw:5432/bookstore")
	led.add("k8s.service/shop/db-rw:5432/bookstore")
	led.add("k8s.service/shop/db-rw:5432/audit")
	if n := led.rounds("k8s.service/shop/db-rw:5432/bookstore"); n != 2 {
		t.Errorf("rounds = %v, want 2: a database counts its own", n)
	}
}

func TestObserveReportsTheRateOnlyWhenItIsKnown(t *testing.T) {
	s := primary
	if m, _, _ := Observe(s, StatsOptions{}); len(m) == 0 {
		t.Fatal("no metrics")
	} else if _, ok := m["rate"]; ok {
		t.Errorf("rate = %v before a second round", m["rate"])
	}
	s.TxPerSecond, s.TxRateKnown = 0, true
	if m, _, _ := Observe(s, StatsOptions{}); m["rate"] != 0 {
		t.Errorf("rate = %v", m["rate"])
	} else if _, ok := m["rate"]; !ok {
		t.Error("a database that finished nothing reads 0, which is known")
	}
	s.TxPerSecond = 56.5
	if m, _, _ := Observe(s, StatsOptions{}); m["rate"] != 56.5 {
		t.Errorf("rate = %v", m["rate"])
	}
	// a replication edge reports the stream and not the database's rate
	if m, _, _ := Observe(s, StatsOptions{Replica: "bookstore-db-2"}); m["lag_bytes"] != 2048 {
		t.Errorf("lag = %v", m["lag_bytes"])
	} else if _, ok := m["rate"]; ok {
		t.Errorf("rate = %v on a replication edge", m["rate"])
	}
}

func TestAConsumerAheadOfThePositionIsNotBehind(t *testing.T) {
	pid := int64(9)
	ahead, behind := -1.0, 4096.0
	state := "streaming"
	if r := slotRow("s", "logical", true, &pid, &ahead, &ahead); r.LagBytes != 0 || r.RetainedBytes != 0 || !r.LagKnown {
		t.Errorf("slot = %+v", r)
	}
	if r := slotRow("s", "logical", true, &pid, &behind, &behind); r.LagBytes != 4096 || r.RetainedBytes != 4096 {
		t.Errorf("slot = %+v", r)
	}
	if r := replicationRow(&pid, "a", &state, &ahead); r.LagBytes != 0 || r.LagHidden {
		t.Errorf("replica = %+v", r)
	}
}
