package pgprobe

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// ReplicationRow is one row of pg_stat_replication.
type ReplicationRow struct {
	ApplicationName string
	State           string
	LagBytes        float64
}

// SlotRow is one row of pg_replication_slots.
type SlotRow struct {
	Name          string
	Active        bool
	RetainedBytes float64
}

// WaitingRow is one waiting query from pg_stat_activity.
type WaitingRow struct {
	State     string
	WaitEvent string
	Query     string
	WaitS     float64
}

// Stats is everything one round of pg.stats queries returned.
type Stats struct {
	Version        string
	InRecovery     bool
	MaxConnections float64
	Connections    float64 // client backends
	Active         float64 // client backends in state active
	LockWaiters    float64 // client backends waiting on a lock
	Replication    []ReplicationRow
	Slots          []SlotRow
	UsedBytes      float64
	Waiting        []WaitingRow
}

// StatsOptions is the part of the spec that shapes the observation.
type StatsOptions struct {
	// Replica, when set, makes this a replication edge binding: the metrics
	// describe that one replica (its application_name or slot name).
	Replica string
	// DiskTotalBytes enables disk_pct.
	DiskTotalBytes float64
	// Via is copied into the detail so the panel can name the path.
	Via string
}

// slotName normalizes a replica name into the identifier PostgreSQL would
// have accepted as a slot name ("bookstore-db-3" -> "bookstore_db_3").
func slotName(replica string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '_'
	}, replica)
}

// findReplication returns the replication row for a replica by application
// name (exact, then normalized).
func findReplication(rows []ReplicationRow, replica string) (ReplicationRow, bool) {
	for _, r := range rows {
		if r.ApplicationName == replica {
			return r, true
		}
	}
	want := slotName(replica)
	for _, r := range rows {
		if slotName(r.ApplicationName) == want {
			return r, true
		}
	}
	return ReplicationRow{}, false
}

// findSlot returns the slot for a replica by name (exact, then normalized).
func findSlot(slots []SlotRow, replica string) (SlotRow, bool) {
	for _, s := range slots {
		if s.Name == replica {
			return s, true
		}
	}
	want := slotName(replica)
	for _, s := range slots {
		if slotName(s.Name) == want {
			return s, true
		}
	}
	return SlotRow{}, false
}

// Observe maps a round of pg.stats results onto the metrics vocabulary. For
// a component binding it reports connections, waiters, replication lag, WAL
// retention, size and disk_pct; for a replica binding (opts.Replica set) it
// reports that replica's lag_bytes and streaming flag and raises
// ReplicationBroken and SlotInactive when the replica is not streaming or
// its slot is inactive. Conditions come back without Since; the probe stamps
// them with the time it first saw them.
func Observe(s Stats, opts StatsOptions) (map[string]float64, []model.Condition, map[string]any) {
	m := map[string]float64{}
	detail := map[string]any{}
	var conds []model.Condition

	if s.Version != "" {
		detail["version"] = s.Version
	}
	if opts.Via != "" {
		detail["via"] = opts.Via
	}
	replicas := make([]map[string]any, 0, len(s.Replication))
	for _, r := range s.Replication {
		replicas = append(replicas, map[string]any{"application_name": r.ApplicationName, "state": r.State, "lag_bytes": r.LagBytes})
	}
	detail["replicas"] = replicas
	slots := make([]map[string]any, 0, len(s.Slots))
	for _, sl := range s.Slots {
		slots = append(slots, map[string]any{"slot_name": sl.Name, "active": sl.Active, "retained_bytes": sl.RetainedBytes})
	}
	detail["slots"] = slots

	if opts.Replica != "" {
		rep, hasRep := findReplication(s.Replication, opts.Replica)
		slot, hasSlot := findSlot(s.Slots, opts.Replica)
		name := opts.Replica
		if hasSlot {
			name = slot.Name
		} else {
			name = slotName(name)
		}
		streaming := hasRep && (rep.State == "streaming" || rep.State == "catchup")
		if hasRep {
			detail["state"] = rep.State
		}
		broken := !streaming || (hasSlot && !slot.Active)
		r := facet.ReplicationFacet{Slot: name, Streaming: !broken}
		if hasRep {
			r.Lag = facet.N(rep.LagBytes)
		}
		if hasSlot {
			r.WALRetained = facet.N(slot.RetainedBytes)
			r.SlotDetail = fmt.Sprintf("%s retains %s of WAL", name, humanBytes(slot.RetainedBytes))
		}
		if broken {
			r.Detail = fmt.Sprintf("replication slot %s inactive", name)
			if !hasSlot && !hasRep {
				r.Detail = fmt.Sprintf("no replication connection or slot for %s", opts.Replica)
			} else if !hasSlot {
				r.Detail = fmt.Sprintf("replica %s not streaming (state %q)", opts.Replica, rep.State)
			}
		}
		// The facet writes the canonical form shared by every replication
		// consumer: replicas, Electric, CDC connectors.
		var o probe.Observation
		facet.EmitReplication(&o, r, time.Time{})
		for k, v := range o.Metrics {
			m[k] = v
		}
		conds = append(conds, o.Conditions...)
		return m, conds, detail
	}

	m["connections_used"] = s.Connections
	if s.MaxConnections > 0 {
		m["connections_max"] = s.MaxConnections
	}
	m["active_connections"] = s.Active
	m["waiters"] = s.LockWaiters
	if len(s.Replication) > 0 {
		var lag float64
		for _, r := range s.Replication {
			lag = max(lag, r.LagBytes)
		}
		m["lag_bytes"] = lag
	}
	var retained float64
	hasInactive := false
	for _, sl := range s.Slots {
		if !sl.Active {
			hasInactive = true
			retained = max(retained, sl.RetainedBytes)
		}
	}
	if hasInactive {
		m["wal_retained_bytes"] = retained
	}
	if s.UsedBytes > 0 {
		m["used_bytes"] = s.UsedBytes
	}
	if opts.DiskTotalBytes > 0 {
		m["disk_pct"] = s.UsedBytes / opts.DiskTotalBytes * 100
		m["total_bytes"] = opts.DiskTotalBytes
	}
	if len(s.Waiting) > 0 {
		top := make([]map[string]any, 0, len(s.Waiting))
		for _, w := range s.Waiting {
			top = append(top, map[string]any{"state": w.State, "wait_event": w.WaitEvent, "query": w.Query, "wait_s": w.WaitS})
		}
		detail["top_waiting"] = top
	}
	detail["in_recovery"] = s.InRecovery
	return m, conds, detail
}

// humanBytes renders a byte count for detail text.
func humanBytes(b float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", b, units[i])
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

// walNow is the LSN to measure lag from: the write position on a primary,
// the replay position on a standby.
const walNow = "CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END"

// collect runs the pg.stats queries on conn.
func collect(ctx context.Context, conn *pgx.Conn) (Stats, error) {
	var s Stats
	var maxConn string
	if err := conn.QueryRow(ctx, "SHOW max_connections").Scan(&maxConn); err != nil {
		return s, fmt.Errorf("SHOW max_connections: %w", err)
	}
	s.MaxConnections, _ = strconv.ParseFloat(strings.TrimSpace(maxConn), 64)
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version'), pg_is_in_recovery()").Scan(&s.Version, &s.InRecovery); err != nil {
		return s, fmt.Errorf("server_version: %w", err)
	}
	var total, active, waiters int64
	if err := conn.QueryRow(ctx,
		"SELECT count(*), count(*) FILTER (WHERE state = 'active'), count(*) FILTER (WHERE wait_event_type = 'Lock') "+
			"FROM pg_stat_activity WHERE backend_type = 'client backend'").Scan(&total, &active, &waiters); err != nil {
		return s, fmt.Errorf("pg_stat_activity: %w", err)
	}
	s.Connections, s.Active, s.LockWaiters = float64(total), float64(active), float64(waiters)

	rows, err := conn.Query(ctx, "SELECT coalesce(application_name, ''), coalesce(state, ''), "+
		"coalesce(pg_wal_lsn_diff("+walNow+", replay_lsn), 0)::float8 FROM pg_stat_replication ORDER BY application_name")
	if err != nil {
		return s, fmt.Errorf("pg_stat_replication: %w", err)
	}
	s.Replication, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReplicationRow, error) {
		var r ReplicationRow
		err := row.Scan(&r.ApplicationName, &r.State, &r.LagBytes)
		return r, err
	})
	if err != nil {
		return s, fmt.Errorf("pg_stat_replication: %w", err)
	}

	rows, err = conn.Query(ctx, "SELECT slot_name::text, active, "+
		"coalesce(pg_wal_lsn_diff("+walNow+", restart_lsn), 0)::float8 FROM pg_replication_slots ORDER BY slot_name")
	if err != nil {
		return s, fmt.Errorf("pg_replication_slots: %w", err)
	}
	s.Slots, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (SlotRow, error) {
		var r SlotRow
		err := row.Scan(&r.Name, &r.Active, &r.RetainedBytes)
		return r, err
	})
	if err != nil {
		return s, fmt.Errorf("pg_replication_slots: %w", err)
	}

	if err := conn.QueryRow(ctx, "SELECT coalesce(sum(pg_database_size(datname)), 0)::float8 FROM pg_database "+
		"WHERE NOT datistemplate AND has_database_privilege(datname, 'CONNECT')").Scan(&s.UsedBytes); err != nil {
		return s, fmt.Errorf("pg_database_size: %w", err)
	}

	rows, err = conn.Query(ctx, "SELECT coalesce(state, ''), coalesce(wait_event, ''), left(query, 120), "+
		"coalesce(extract(epoch FROM now() - query_start), 0)::float8 FROM pg_stat_activity "+
		"WHERE backend_type = 'client backend' AND state = 'active' AND wait_event IS NOT NULL "+
		"ORDER BY query_start LIMIT 5")
	if err != nil {
		return s, fmt.Errorf("waiting queries: %w", err)
	}
	s.Waiting, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (WaitingRow, error) {
		var w WaitingRow
		err := row.Scan(&w.State, &w.WaitEvent, &w.Query, &w.WaitS)
		return w, err
	})
	if err != nil {
		return s, fmt.Errorf("waiting queries: %w", err)
	}
	return s, nil
}

// statsAccess documents pg.stats.
var statsAccess = probe.Access{
	Kind:   "pg.stats",
	Source: "pg_stat_activity, pg_stat_replication, pg_replication_slots and pg_database_size over a read-only connection",
	Delivers: "connections_used, connections_max, active_connections, waiters, lag_bytes, wal_retained_bytes, used_bytes, disk_pct; " +
		"on a replication edge (replica set): lag_bytes, streaming, wal_retained_bytes and ReplicationBroken/SlotInactive; " +
		"detail: version, replicas, slots, top_waiting",
	SpecFields:  []string{"dsn_env", "via", "replica", "disk_total_bytes", "interval"},
	Needs:       "a DSN in the environment variable named by dsn_env for a role with pg_read_all_stats (or pg_monitor)",
	Implemented: true,
}

func init() {
	probe.Register(statsAccess, func() probe.Probe { return &StatsProbe{} })
}

// StatsProbe is pg.stats. Spec: dsn_env (required), via, replica,
// disk_total_bytes, interval (default = tick).
type StatsProbe struct {
	h probe.Health
	lifetime
}

// Kind implements probe.Probe.
func (p *StatsProbe) Kind() string { return statsAccess.Kind }

// Validate implements probe.Probe.
func (p *StatsProbe) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "dsn_env"); err != nil {
		return err
	}
	if v, ok := spec["disk_total_bytes"]; ok {
		if n, isNum := probe.Num(spec, "disk_total_bytes"); !isNum || n <= 0 {
			return fmt.Errorf("disk_total_bytes must be a positive number, got %v", v)
		}
	}
	if v, ok := spec["interval"].(string); ok {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("interval: %w", err)
		}
	}
	return nil
}

// Health implements probe.Probe.
func (p *StatsProbe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *StatsProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	c, err := newConnector(spec, "dsn_env", "wassup")
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	opts := StatsOptions{Replica: probe.Str(spec, "replica", ""), Via: probe.Str(spec, "via", "")}
	opts.DiskTotalBytes, _ = probe.Num(spec, "disk_total_bytes")
	tgt := target(spec)
	every := tick(spec)
	interval := probe.Dur(spec, "interval", every)
	since := map[string]time.Time{}
	poll := func(ctx context.Context) probe.Observation {
		o := probe.Observation{Target: tgt, Probe: p.Kind(), At: time.Now()}
		conn, err := c.acquire(ctx)
		if err == nil {
			var s Stats
			s, err = collect(ctx, conn)
			if err == nil {
				o.Metrics, o.Conditions, o.Detail = Observe(s, opts)
				stamp(o.Conditions, since, o.At)
				p.h.Set(probe.HealthOK, "")
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

// stamp sets Since on each condition to the first time this probe saw that
// kind and ref, and forgets the ones that went away.
func stamp(conds []model.Condition, since map[string]time.Time, now time.Time) {
	live := map[string]bool{}
	for i := range conds {
		key := conds[i].Kind + " " + conds[i].Ref
		live[key] = true
		if t, ok := since[key]; ok {
			conds[i].Since = t
		} else {
			since[key] = now
			conds[i].Since = now
		}
	}
	for k := range since {
		if !live[k] {
			delete(since, k)
		}
	}
}
