package pgprobe

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/danilopopovikj/wassup/internal/model"
)

func TestPoolRowFromMap(t *testing.T) {
	// pgbouncer 1.21 declares these as int8/int4 and text; the simple
	// protocol yields int64/int32/string.
	row := map[string]any{
		"database": "app", "user": "api", "cl_active": int64(100), "cl_waiting": int32(38),
		"cl_active_cancel_req": int64(0), "sv_active": int64(100), "sv_idle": int64(0), "sv_used": int64(0),
		"sv_tested": int64(0), "sv_login": int64(0), "maxwait": int64(3), "maxwait_us": int64(250000),
		"pool_mode": "transaction",
	}
	r := PoolRowFromMap(row)
	want := PoolRow{Database: "app", User: "api", ClActive: 100, ClWaiting: 38, SvActive: 100, MaxWaitS: 3.25, PoolMode: "transaction"}
	if r != want {
		t.Errorf("row = %+v, want %+v", r, want)
	}
	// Older versions ship fewer columns and text numbers still parse.
	old := PoolRowFromMap(map[string]any{"database": "app", "user": "api", "sv_active": "7", "cl_waiting": pgtype.Numeric{}})
	if old.SvActive != 7 || old.ClWaiting != 0 || old.MaxWaitS != 0 {
		t.Errorf("old row = %+v", old)
	}
}

func TestPoolMatches(t *testing.T) {
	r := PoolRow{Database: "app", User: "api"}
	cases := map[string]bool{"": true, "app": true, "api": true, "app/api": true, "app/": true, "/api": true, "other": false, "app/other": false}
	for f, want := range cases {
		if got := r.matches(f); got != want {
			t.Errorf("matches(%q) = %v, want %v", f, got, want)
		}
	}
	if (PoolRow{Database: "pgbouncer", User: "admin"}).matches("") {
		t.Error("pgbouncer's own pool is excluded by default")
	}
}

// exhausted is scenario 02: 100 of 100 server connections busy, 38 clients
// waiting, 800 queries per second.
var exhausted = PoolInput{
	Pools: []PoolRow{
		{Database: "app", User: "api", ClActive: 100, ClWaiting: 38, SvActive: 100, MaxWaitS: 4.2, PoolMode: "transaction"},
		{Database: "app", User: "worker", ClActive: 4, SvActive: 2, SvIdle: 3, PoolMode: "transaction"},
		{Database: "pgbouncer", User: "pgbouncer", ClActive: 1},
	},
	Databases: []DatabaseRow{{Name: "app", PoolSize: 100}},
	Stats:     []StatsRow{{Database: "app", AvgQueryCount: 800}, {Database: "pgbouncer", AvgQueryCount: 1}},
	Config:    PoolConfig{DefaultPoolSize: 20, MaxClientConn: 1000},
}

func TestObservePoolsFiltered(t *testing.T) {
	m, conds, detail := ObservePools(exhausted, "app/api")
	want := map[string]float64{"pool_used": 100, "pool_max": 100, "waiters": 38, "queued": 38, "rate": 800}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	c, ok := model.HasCondition(conds, model.CondPoolExhausted)
	if !ok {
		t.Fatalf("PoolExhausted missing: %+v", conds)
	}
	if c.Ref != "pool/app/api" {
		t.Errorf("ref = %q", c.Ref)
	}
	if detail["maxwait"] != 4.2 || detail["default_pool_size"] != float64(20) || detail["max_client_conn"] != float64(1000) {
		t.Errorf("detail = %+v", detail)
	}
	if pools, ok := detail["pools"].([]map[string]any); !ok || len(pools) != 1 || pools[0]["pool_size"] != float64(100) {
		t.Errorf("pools = %+v", detail["pools"])
	}
}

func TestObservePoolsAggregate(t *testing.T) {
	m, conds, _ := ObservePools(exhausted, "")
	// Both app pools, pgbouncer's own excluded: 102 used of 200, 38 waiting.
	if m["pool_used"] != 102 || m["pool_max"] != 200 || m["waiters"] != 38 || m["rate"] != 800 {
		t.Errorf("metrics = %v", m)
	}
	if len(conds) != 0 {
		t.Errorf("aggregate has idle capacity, no PoolExhausted: %+v", conds)
	}
}

func TestObservePoolsDefaultSizeAndIdle(t *testing.T) {
	in := PoolInput{
		Pools:  []PoolRow{{Database: "app", User: "api", SvActive: 5, SvIdle: 15}},
		Config: PoolConfig{DefaultPoolSize: 20},
	}
	m, conds, _ := ObservePools(in, "")
	if m["pool_max"] != 20 || m["pool_used"] != 5 || m["waiters"] != 0 {
		t.Errorf("metrics = %v", m)
	}
	if _, ok := m["rate"]; ok {
		t.Error("rate omitted without SHOW STATS")
	}
	if len(conds) != 0 {
		t.Errorf("unexpected %+v", conds)
	}
	m, _, detail := ObservePools(in, "nope")
	if len(m) != 0 || detail["filter"] != "nope" {
		t.Errorf("no match should yield no metrics: %v %v", m, detail)
	}
}

func TestPoolValidate(t *testing.T) {
	p := &PoolProbe{}
	if err := p.Validate(map[string]any{}); err == nil {
		t.Error("dsn_env required")
	}
	if err := p.Validate(map[string]any{"dsn_env": "PGB_DSN", "pool": "app", "via": "k8s.workload/api"}); err != nil {
		t.Errorf("valid: %v", err)
	}
}

func TestPoolStartRejectsNonAdminDatabase(t *testing.T) {
	t.Setenv("WASSUP_TEST_PGB_DSN", "postgres://stats:pw@pgbouncer:6432/app")
	p := &PoolProbe{}
	if err := p.Start(t.Context(), map[string]any{"dsn_env": "WASSUP_TEST_PGB_DSN"}, nil); err == nil {
		t.Error("a DSN for another database than pgbouncer should fail")
	}
}
