package pgprobe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// PoolRow is one row of pgbouncer's SHOW POOLS.
type PoolRow struct {
	Database  string
	User      string
	ClActive  float64
	ClWaiting float64
	SvActive  float64
	SvIdle    float64
	SvUsed    float64
	MaxWaitS  float64
	PoolMode  string
}

// DatabaseRow is one row of SHOW DATABASES (only the pool sizing).
type DatabaseRow struct {
	Name     string
	PoolSize float64
}

// StatsRow is one row of SHOW STATS.
type StatsRow struct {
	Database      string
	AvgQueryCount float64 // queries per second over the last period
}

// PoolConfig is the part of SHOW CONFIG the probe uses.
type PoolConfig struct {
	DefaultPoolSize float64
	MaxClientConn   float64
}

// PoolInput is everything one round of pg.pool queries returned.
type PoolInput struct {
	Pools     []PoolRow
	Databases []DatabaseRow
	Stats     []StatsRow
	Config    PoolConfig
}

// PoolRowFromMap maps a SHOW POOLS row (column name to value) to a PoolRow.
// Column sets differ between pgbouncer versions; missing columns read as
// zero. maxwait and maxwait_us are combined into seconds.
func PoolRowFromMap(row map[string]any) PoolRow {
	num := func(k string) float64 {
		f, _ := toFloat(row[k])
		return f
	}
	r := PoolRow{
		Database:  toString(row["database"]),
		User:      toString(row["user"]),
		ClActive:  num("cl_active"),
		ClWaiting: num("cl_waiting"),
		SvActive:  num("sv_active"),
		SvIdle:    num("sv_idle"),
		SvUsed:    num("sv_used"),
		MaxWaitS:  num("maxwait") + num("maxwait_us")/1e6,
		PoolMode:  toString(row["pool_mode"]),
	}
	return r
}

// matches reports whether a pool is selected by filter: "" selects every
// pool but pgbouncer's own, otherwise the filter names a database, a user
// or "database/user".
func (r PoolRow) matches(filter string) bool {
	if filter == "" {
		return r.Database != "pgbouncer"
	}
	if db, user, ok := strings.Cut(filter, "/"); ok {
		return (db == "" || db == r.Database) && (user == "" || user == r.User)
	}
	return r.Database == filter || r.User == filter
}

// ObservePools maps pgbouncer's tables onto the edge vocabulary: pool_used
// (sv_active + sv_used), pool_max (the database's pool_size or
// default_pool_size, summed over the selected pools), waiters and queued
// (cl_waiting), rate (avg_query_count of the selected databases) and
// PoolExhausted when every server connection is active and clients wait.
// The condition comes back without Since; the probe stamps it.
func ObservePools(in PoolInput, filter string) (map[string]float64, []model.Condition, map[string]any) {
	m := map[string]float64{}
	detail := map[string]any{}
	var conds []model.Condition

	poolSize := map[string]float64{}
	for _, d := range in.Databases {
		if d.PoolSize > 0 {
			poolSize[d.Name] = d.PoolSize
		}
	}
	var used, active, maxSize, waiting, maxWait float64
	table := make([]map[string]any, 0, len(in.Pools))
	selected := map[string]bool{}
	for _, r := range in.Pools {
		if !r.matches(filter) {
			continue
		}
		selected[r.Database] = true
		size := poolSize[r.Database]
		if size == 0 {
			size = in.Config.DefaultPoolSize
		}
		used += r.SvActive + r.SvUsed
		active += r.SvActive
		maxSize += size
		waiting += r.ClWaiting
		maxWait = max(maxWait, r.MaxWaitS)
		table = append(table, map[string]any{
			"database": r.Database, "user": r.User, "pool_mode": r.PoolMode,
			"cl_active": r.ClActive, "cl_waiting": r.ClWaiting,
			"sv_active": r.SvActive, "sv_idle": r.SvIdle, "sv_used": r.SvUsed,
			"pool_size": size, "maxwait_s": r.MaxWaitS,
		})
	}
	if len(table) == 0 {
		detail["pools"] = table
		if filter != "" {
			detail["filter"] = filter
		}
		return m, conds, detail
	}
	m["pool_used"] = used
	if maxSize > 0 {
		m["pool_max"] = maxSize
	}
	m["waiters"] = waiting
	m["queued"] = waiting
	var rate float64
	hasRate := false
	for _, s := range in.Stats {
		if selected[s.Database] {
			rate += s.AvgQueryCount
			hasRate = true
		}
	}
	if hasRate {
		m["rate"] = rate
	}
	if maxSize > 0 && active >= maxSize && waiting > 0 {
		conds = append(conds, model.Condition{
			Kind:   model.CondPoolExhausted,
			Ref:    "pool/" + poolRef(filter, table),
			Detail: fmt.Sprintf("%.0f/%.0f server connections busy, %.0f clients waiting up to %.1fs", active, maxSize, waiting, maxWait),
		})
	}
	detail["pools"] = table
	detail["maxwait"] = maxWait
	if filter != "" {
		detail["filter"] = filter
	}
	if in.Config.DefaultPoolSize > 0 {
		detail["default_pool_size"] = in.Config.DefaultPoolSize
	}
	if in.Config.MaxClientConn > 0 {
		detail["max_client_conn"] = in.Config.MaxClientConn
	}
	return m, conds, detail
}

// poolRef names the pool in a condition ref.
func poolRef(filter string, table []map[string]any) string {
	if filter != "" {
		return filter
	}
	if len(table) == 1 {
		return fmt.Sprintf("%v/%v", table[0]["database"], table[0]["user"])
	}
	return "all"
}

// showRows runs a pgbouncer SHOW command and returns its rows as maps.
func showRows(ctx context.Context, conn *pgx.Conn, what string) ([]map[string]any, error) {
	rows, err := conn.Query(ctx, "SHOW "+what)
	if err != nil {
		return nil, fmt.Errorf("SHOW %s: %w", what, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		return nil, fmt.Errorf("SHOW %s: %w", what, err)
	}
	return out, nil
}

// collectPools runs SHOW POOLS, CONFIG, DATABASES and STATS.
func collectPools(ctx context.Context, conn *pgx.Conn) (PoolInput, error) {
	var in PoolInput
	rows, err := showRows(ctx, conn, "POOLS")
	if err != nil {
		return in, err
	}
	for _, r := range rows {
		in.Pools = append(in.Pools, PoolRowFromMap(r))
	}
	rows, err = showRows(ctx, conn, "CONFIG")
	if err != nil {
		return in, err
	}
	for _, r := range rows {
		v, _ := toFloat(r["value"])
		switch toString(r["key"]) {
		case "default_pool_size":
			in.Config.DefaultPoolSize = v
		case "max_client_conn":
			in.Config.MaxClientConn = v
		}
	}
	// SHOW DATABASES and SHOW STATS are refinements; a failure there does not
	// lose the pool figures.
	if rows, err := showRows(ctx, conn, "DATABASES"); err == nil {
		for _, r := range rows {
			size, _ := toFloat(r["pool_size"])
			in.Databases = append(in.Databases, DatabaseRow{Name: toString(r["name"]), PoolSize: size})
		}
	}
	if rows, err := showRows(ctx, conn, "STATS"); err == nil {
		for _, r := range rows {
			avg, ok := toFloat(r["avg_query_count"])
			if !ok {
				continue
			}
			in.Stats = append(in.Stats, StatsRow{Database: toString(r["database"]), AvgQueryCount: avg})
		}
	}
	return in, nil
}

// poolAccess documents pg.pool.
var poolAccess = probe.Access{
	Kind:   "pg.pool",
	Source: "pgbouncer's admin console (SHOW POOLS, CONFIG, DATABASES, STATS)",
	Delivers: "pool_used, pool_max, waiters, queued, rate; PoolExhausted when every server connection is busy and clients wait; " +
		"detail: pools table, maxwait, default_pool_size, max_client_conn",
	SpecFields:  []string{"dsn_env", "pool", "via", "interval"},
	Needs:       "a DSN for database \"pgbouncer\" in the environment variable named by dsn_env, as a stats_users or admin_users role",
	Implemented: true,
}

func init() {
	probe.Register(poolAccess, func() probe.Probe { return &PoolProbe{} })
}

// PoolProbe is pg.pool. Spec: dsn_env (required, database "pgbouncer"),
// pool (database, user or "database/user" filter), via, interval.
type PoolProbe struct {
	h probe.Health
	lifetime
}

// Kind implements probe.Probe.
func (p *PoolProbe) Kind() string { return poolAccess.Kind }

// Validate implements probe.Probe.
func (p *PoolProbe) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "dsn_env"); err != nil {
		return err
	}
	if v, ok := spec["interval"].(string); ok {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("interval: %w", err)
		}
	}
	return nil
}

// Health implements probe.Probe.
func (p *PoolProbe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *PoolProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	c, err := newConnector(spec, "dsn_env", "")
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	if db := c.database(); db != "" && db != "pgbouncer" {
		err := fmt.Errorf("dsn_env must point at the pgbouncer admin database, got %q", db)
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	filter := probe.Str(spec, "pool", "")
	via := probe.Str(spec, "via", "")
	tgt := target(spec)
	every := tick(spec)
	interval := probe.Dur(spec, "interval", every)
	since := map[string]time.Time{}
	poll := func(ctx context.Context) probe.Observation {
		o := probe.Observation{Target: tgt, Probe: p.Kind(), At: time.Now()}
		conn, err := c.acquire(ctx)
		if err == nil {
			var in PoolInput
			in, err = collectPools(ctx, conn)
			if err == nil {
				o.Metrics, o.Conditions, o.Detail = ObservePools(in, filter)
				if via != "" {
					o.Detail["via"] = via
				}
				stamp(o.Conditions, since, o.At)
				if len(o.Metrics) == 0 {
					p.h.Set(probe.HealthDegraded, "no pool matches "+filter)
				} else {
					p.h.Set(probe.HealthOK, "")
				}
				return o
			}
		}
		c.drop()
		o.Err = err.Error()
		if isMisconfigured(err) {
			p.h.Set(probe.HealthFailed, o.Err)
		} else {
			p.h.Set(probe.HealthDegraded, o.Err)
		}
		return o
	}
	p.spawn(ctx, out, every, interval, c, poll)
	return nil
}
