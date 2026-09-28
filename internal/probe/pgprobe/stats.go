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

// ReplicationRow is one row of pg_stat_replication. PostgreSQL shows every
// role the row itself (pid, user, application name) but returns NULL for the
// detail columns to a role without pg_read_all_stats, its own walsender
// included. Hidden says so: State and LagBytes were not readable, which is
// not the same as a replica that does not stream.
type ReplicationRow struct {
	PID             int64
	ApplicationName string
	State           string
	LagBytes        float64
	Hidden          bool
	LagHidden       bool
}

// SlotRow is one row of pg_replication_slots, which every role can read.
// Active is the only source of slot activity. LagBytes is how far the
// consumer is behind: the distance to confirmed_flush_lsn for a logical
// slot, to restart_lsn for a physical one; LagKnown is false when the slot
// has no position yet.
type SlotRow struct {
	Name          string
	Type          string // physical or logical
	Active        bool
	ActivePID     int64
	RetainedBytes float64
	LagBytes      float64
	LagKnown      bool
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
	// ActivityHidden is set when sessions of other roles are in
	// pg_stat_activity with their state hidden (no pg_read_all_stats):
	// Connections still counts them, Active and LockWaiters are not known.
	ActivityHidden bool
	Replication    []ReplicationRow
	Slots          []SlotRow
	UsedBytes      float64
	Waiting        []WaitingRow
}

// StatsOptions is the part of the spec that shapes the observation.
type StatsOptions struct {
	// Replica, when set, makes this a replication edge binding: the metrics
	// describe that one replica. It names the slot, the application_name or
	// the instance behind a CloudNativePG slot (bookstore-db-2 has the slot
	// _cnpg_bookstore_db_2).
	Replica string
	// DiskTotalBytes enables disk_pct.
	DiskTotalBytes float64
	// Via is copied into the detail so the panel can name the path.
	Via string
	// Role is the role of the connection, named in the detail when it may
	// not see everything.
	Role string
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

// cnpgSlotPrefix is how CloudNativePG names the physical slot of an
// instance: _cnpg_ followed by the instance name with dashes as underscores.
const cnpgSlotPrefix = "_cnpg_"

// bareName is the normalized replica name without the CloudNativePG prefix,
// so a slot name, an application name and an instance name of the same
// replica compare equal.
func bareName(name string) string {
	return strings.TrimPrefix(slotName(name), cnpgSlotPrefix)
}

// findSlot returns the slot of a replica: by exact name, then by normalized
// name, then as the CloudNativePG slot of that instance.
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
	want = bareName(replica)
	for _, s := range slots {
		if strings.HasPrefix(s.Name, cnpgSlotPrefix) && bareName(s.Name) == want {
			return s, true
		}
	}
	return SlotRow{}, false
}

// findReplication returns the replication row of a replica: the walsender
// that holds its slot, else by application name (exact, normalized, without
// the CloudNativePG prefix).
func findReplication(rows []ReplicationRow, replica string, slot SlotRow, hasSlot bool) (ReplicationRow, bool) {
	if hasSlot && slot.ActivePID != 0 {
		for _, r := range rows {
			if r.PID == slot.ActivePID {
				return r, true
			}
		}
	}
	for _, r := range rows {
		if r.ApplicationName == replica {
			return r, true
		}
	}
	want := bareName(replica)
	for _, r := range rows {
		if r.ApplicationName != "" && bareName(r.ApplicationName) == want {
			return r, true
		}
	}
	return ReplicationRow{}, false
}

// streamingState reports whether a walsender state means the replica
// receives the stream.
func streamingState(state string) bool { return state == "streaming" || state == "catchup" }

// lagFromSlot is the note left in the detail when the numbers could not be
// read from pg_stat_replication.
const lagFromSlot = "lag read from pg_replication_slots: the role lacks pg_read_all_stats, so pg_stat_replication hides state and lag"

// Observe maps a round of pg.stats results onto the metrics vocabulary. For
// a component binding it reports connections, waiters, replication lag, WAL
// retention, size and disk_pct; for a replica binding (opts.Replica set) it
// reports that replica's lag_bytes and streaming flag and raises
// ReplicationBroken and SlotInactive when the replica is not streaming or
// its slot is inactive. Conditions come back without Since; the probe stamps
// them with the time it first saw them.
//
// Broken is only ever said from what was read: a slot that is not active, a
// walsender whose state is visible and is not streaming, or no slot and no
// walsender at all. A state the role may not see says nothing; the slot's
// activity and position stand in for it.
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
	hidden := false
	replicas := make([]map[string]any, 0, len(s.Replication))
	for _, r := range s.Replication {
		row := map[string]any{"application_name": r.ApplicationName}
		if r.Hidden {
			hidden = true
			row["state"] = "not visible to this role"
		} else {
			row["state"] = r.State
		}
		if !r.LagHidden {
			row["lag_bytes"] = r.LagBytes
		}
		replicas = append(replicas, row)
	}
	detail["replicas"] = replicas
	slots := make([]map[string]any, 0, len(s.Slots))
	for _, sl := range s.Slots {
		row := map[string]any{"slot_name": sl.Name, "active": sl.Active, "retained_bytes": sl.RetainedBytes}
		if sl.Type != "" {
			row["slot_type"] = sl.Type
		}
		if sl.LagKnown {
			row["lag_bytes"] = sl.LagBytes
		}
		slots = append(slots, row)
	}
	detail["slots"] = slots

	if opts.Replica != "" {
		slot, hasSlot := findSlot(s.Slots, opts.Replica)
		rep, hasRep := findReplication(s.Replication, opts.Replica, slot, hasSlot)
		name := opts.Replica
		if hasSlot {
			name = slot.Name
		} else {
			name = slotName(name)
		}
		stateKnown := hasRep && !rep.Hidden
		if stateKnown {
			detail["state"] = rep.State
		}
		r := facet.ReplicationFacet{Slot: name}
		switch {
		case hasSlot && !slot.Active:
			r.Detail = fmt.Sprintf("replication slot %s inactive", name)
		case stateKnown && !streamingState(rep.State):
			r.Detail = fmt.Sprintf("replica %s not streaming (state %q)", opts.Replica, rep.State)
		case !hasSlot && !hasRep:
			r.Detail = fmt.Sprintf("no replication connection or slot for %s", opts.Replica)
		default:
			// An active slot, a streaming walsender, or a connected walsender
			// whose state the role may not see.
			r.Streaming = true
		}
		switch {
		case hasRep && !rep.LagHidden:
			r.Lag = facet.N(rep.LagBytes)
		case hasSlot && slot.LagKnown:
			r.Lag = facet.N(slot.LagBytes)
			detail["lag_source"] = "pg_replication_slots"
			if !hasRep || rep.Hidden {
				detail["note"] = lagFromSlot
			}
		case hasRep:
			detail["note"] = "lag not visible: the role lacks pg_read_all_stats and the replica has no replication slot"
		}
		if hasSlot {
			r.WALRetained = facet.N(slot.RetainedBytes)
			r.SlotDetail = fmt.Sprintf("%s retains %s of WAL", name, humanBytes(slot.RetainedBytes))
		}
		// The facet writes the canonical form shared by every replication
		// consumer: replicas, Electric, CDC connectors.
		var o probe.Observation
		facet.EmitReplication(&o, r, time.Time{})
		for k, v := range o.Metrics {
			m[k] = v
		}
		conds = append(conds, o.Conditions...)
		addGrant(detail, opts.Role, hidden, false)
		return m, conds, detail
	}

	m["connections_used"] = s.Connections
	if s.MaxConnections > 0 {
		m["connections_max"] = s.MaxConnections
	}
	if s.ActivityHidden {
		detail["activity_note"] = "active connections and lock waiters not visible: the role lacks pg_read_all_stats"
	} else {
		m["active_connections"] = s.Active
		m["waiters"] = s.LockWaiters
	}
	var lag float64
	hasLag := false
	for _, r := range s.Replication {
		if !r.LagHidden {
			lag, hasLag = max(lag, r.LagBytes), true
		}
	}
	if !hasLag && hidden {
		// The walsenders are there but their positions are hidden: the active
		// slots say how far their consumers are behind.
		for _, sl := range s.Slots {
			if sl.Active && sl.LagKnown {
				lag, hasLag = max(lag, sl.LagBytes), true
			}
		}
		if hasLag {
			detail["lag_source"] = "pg_replication_slots"
			detail["note"] = lagFromSlot
		}
	}
	if hasLag {
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
	addGrant(detail, opts.Role, hidden, s.ActivityHidden)
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

// replicationRow builds a row from the nullable columns of
// pg_stat_replication.
func replicationRow(pid *int64, name string, state *string, lag *float64) ReplicationRow {
	r := ReplicationRow{ApplicationName: name, Hidden: state == nil, LagHidden: lag == nil}
	if pid != nil {
		r.PID = *pid
	}
	if state != nil {
		r.State = *state
	}
	if lag != nil {
		r.LagBytes = *lag
	}
	return r
}

// slotRow builds a row from the nullable columns of pg_replication_slots.
func slotRow(name, kind string, active bool, pid *int64, retained, lag *float64) SlotRow {
	r := SlotRow{Name: name, Type: kind, Active: active, LagKnown: lag != nil}
	if pid != nil {
		r.ActivePID = *pid
	}
	if retained != nil {
		r.RetainedBytes = *retained
	}
	if lag != nil {
		r.LagBytes = *lag
	}
	return r
}

// walNow is the LSN to measure lag from: the write position on a primary,
// the replay position on a standby.
const walNow = "CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END"

// collect runs the pg.stats queries on conn, in one read-only transaction.
func collect(ctx context.Context, conn *pgx.Conn) (Stats, error) {
	var s Stats
	err := readOnlyTx(ctx, conn, func(r reads) error {
		var err error
		s, err = collectStats(ctx, r)
		return err
	})
	return s, err
}

// collectStats runs the pg.stats queries.
func collectStats(ctx context.Context, conn reads) (Stats, error) {
	var s Stats
	var maxConn string
	if err := conn.QueryRow(ctx, "SHOW max_connections").Scan(&maxConn); err != nil {
		return s, fmt.Errorf("SHOW max_connections: %w", err)
	}
	s.MaxConnections, _ = strconv.ParseFloat(strings.TrimSpace(maxConn), 64)
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version'), pg_is_in_recovery()").Scan(&s.Version, &s.InRecovery); err != nil {
		return s, fmt.Errorf("server_version: %w", err)
	}
	// A role without pg_read_all_stats sees the sessions of other roles with
	// backend_type, state and wait_event hidden. They are connections all the
	// same (they hold a database), but what they do is not known.
	var total, hiddenSessions, active, waiters int64
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FILTER (WHERE backend_type = 'client backend'), "+
			"count(*) FILTER (WHERE backend_type IS NULL AND datid IS NOT NULL), "+
			"count(*) FILTER (WHERE backend_type = 'client backend' AND state = 'active'), "+
			"count(*) FILTER (WHERE backend_type = 'client backend' AND wait_event_type = 'Lock') "+
			"FROM pg_stat_activity").Scan(&total, &hiddenSessions, &active, &waiters); err != nil {
		return s, fmt.Errorf("pg_stat_activity: %w", err)
	}
	s.Connections, s.Active, s.LockWaiters = float64(total+hiddenSessions), float64(active), float64(waiters)
	s.ActivityHidden = hiddenSessions > 0

	// state and replay_lsn come back NULL for a role without
	// pg_read_all_stats; they are read as NULL, never folded into a value.
	rows, err := conn.Query(ctx, "SELECT pid, coalesce(application_name, ''), state, "+
		"pg_wal_lsn_diff("+walNow+", replay_lsn)::float8 FROM pg_stat_replication ORDER BY application_name, pid")
	if err != nil {
		return s, fmt.Errorf("pg_stat_replication: %w", err)
	}
	s.Replication, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReplicationRow, error) {
		var pid *int64
		var name string
		var state *string
		var lag *float64
		if err := row.Scan(&pid, &name, &state, &lag); err != nil {
			return ReplicationRow{}, err
		}
		return replicationRow(pid, name, state, lag), nil
	})
	if err != nil {
		return s, fmt.Errorf("pg_stat_replication: %w", err)
	}

	rows, err = conn.Query(ctx, "SELECT slot_name::text, slot_type::text, active, active_pid, "+
		"pg_wal_lsn_diff("+walNow+", restart_lsn)::float8, "+
		"pg_wal_lsn_diff("+walNow+", CASE WHEN slot_type = 'logical' THEN confirmed_flush_lsn ELSE restart_lsn END)::float8 "+
		"FROM pg_replication_slots ORDER BY slot_name")
	if err != nil {
		return s, fmt.Errorf("pg_replication_slots: %w", err)
	}
	s.Slots, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (SlotRow, error) {
		var name, kind string
		var active bool
		var pid *int64
		var retained, lag *float64
		if err := row.Scan(&name, &kind, &active, &pid, &retained, &lag); err != nil {
			return SlotRow{}, err
		}
		return slotRow(name, kind, active, pid, retained, lag), nil
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
	SpecFields: []string{"dsn_env", "host", "port", "user", "database", "sslmode", "password_env", "via", "replica", "disk_total_bytes", "interval"},
	Needs: "a connection for a role that may log in: either a DSN in the environment variable named by dsn_env, or host, port, user, database and sslmode " +
		"with the password in the variable named by password_env (taken as it is, nothing to encode). With via naming a tunnel, host and port may be left out. " +
		"Every role reads connections, slots and sizes; state and lag of replicas and what other sessions do take pg_read_all_stats (or pg_monitor), " +
		"and the detail says how to grant it",
	Implemented: true,
	Tier:        probe.TierData,
}

func init() {
	probe.Register(statsAccess, func() probe.Probe { return &StatsProbe{} })
}

// StatsProbe is pg.stats. Spec: dsn_env, or host, port, user, database,
// sslmode and password_env; via, replica, disk_total_bytes, interval
// (default = tick).
type StatsProbe struct {
	h probe.Health
	lifetime
}

// Kind implements probe.Probe.
func (p *StatsProbe) Kind() string { return statsAccess.Kind }

// Validate implements probe.Probe.
func (p *StatsProbe) Validate(spec map[string]any) error {
	if err := validateConn(spec, "dsn_env"); err != nil {
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
	return probe.ValidateVia(probe.Str(spec, "via", ""))
}

// Health implements probe.Probe.
func (p *StatsProbe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *StatsProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	c, err := newConnector(spec, "dsn_env", "wassup", "")
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	opts := StatsOptions{Replica: probe.Str(spec, "replica", ""), Via: probe.Str(spec, "via", ""), Role: c.role()}
	opts.DiskTotalBytes, _ = probe.Num(spec, "disk_total_bytes")
	tgt := target(spec)
	every := tick(spec)
	interval := probe.Dur(spec, "interval", every)
	since := map[string]time.Time{}
	poll := func(rctx context.Context) probe.Observation {
		o := probe.Observation{Target: tgt, Probe: p.Kind(), At: time.Now()}
		conn, err := c.acquire(rctx)
		if err == nil {
			var s Stats
			s, err = collect(rctx, conn)
			if err == nil {
				o.Metrics, o.Conditions, o.Detail = Observe(s, opts)
				stamp(o.Conditions, since, o.At)
				// The probe read what the role may see, so it is in order; the
				// message says what the role may not see and how to grant it.
				note, _ := o.Detail["grant_note"].(string)
				p.h.Set(probe.HealthOK, note)
				return o
			}
		}
		// Worded while the tunnel is still known: drop forgets it.
		err = c.explain(err, conn != nil)
		c.drop()
		failed(ctx, &p.h, &o, err)
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
