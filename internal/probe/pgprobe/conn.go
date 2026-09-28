// Package pgprobe implements the PostgreSQL probes of wassup: pg.stats reads
// pg_stat_* views on a server and pg.pool reads the pgbouncer admin console.
// Both use pgx's simple protocol (pgbouncer has no extended protocol), run
// read-only statements only and reuse one connection per binding, reconnecting
// when it breaks. A connection is always released in order, with a terminate
// message: a connection that is cut instead reaches the server as a reset,
// and a port-forward or an SSH tunnel on the way goes down with it.
package pgprobe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// roundTimeout bounds one round of queries.
const roundTimeout = 5 * time.Second

// stopGrace is how long a round in flight may still run once the probe was
// told to stop.
const stopGrace = time.Second

// closeTimeout bounds the goodbye to the server.
const closeTimeout = time.Second

// defaultTick is used when the runtime did not inject one.
const defaultTick = 5 * time.Second

// connector is a lazily opened, reused connection. pgxpool is not linked
// (its puddle dependency is not available in this build), so the "pool" is
// one connection that is re-opened on failure; every probe round runs on it
// sequentially.
type connector struct {
	cfg  *pgx.ConnConfig
	conn *pgx.Conn
}

// newConnector reads the DSN from the environment variable named by
// spec[dsnKey] and prepares a simple-protocol config. appName is set as
// application_name when non-empty (not for pgbouncer's admin console).
func newConnector(spec map[string]any, dsnKey, appName string) (*connector, error) {
	env := probe.Str(spec, dsnKey, "")
	if env == "" {
		return nil, fmt.Errorf("%q is required", dsnKey)
	}
	dsn, ok := os.LookupEnv(env)
	if !ok || dsn == "" {
		return nil, fmt.Errorf("environment variable %s (%s) is not set", env, dsnKey)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid DSN: %w", env, err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.ConnectTimeout = roundTimeout
	if appName != "" {
		if cfg.RuntimeParams == nil {
			cfg.RuntimeParams = map[string]string{}
		}
		if _, set := cfg.RuntimeParams["application_name"]; !set {
			cfg.RuntimeParams["application_name"] = appName
		}
	}
	return &connector{cfg: cfg}, nil
}

// acquire returns the live connection, opening one when needed.
func (c *connector) acquire(ctx context.Context) (*pgx.Conn, error) {
	if c.conn != nil && !c.conn.IsClosed() {
		return c.conn, nil
	}
	conn, err := pgx.ConnectConfig(ctx, c.cfg)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return conn, nil
}

// drop closes the connection after an error so the next round reconnects.
// It sends the terminate message on a context of its own, because the
// probe's context is usually done by the time the connection is released.
// When a round ran into its timeout the driver is already closing the
// connection in the background; drop waits for that goodbye too.
func (c *connector) drop() {
	if c.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = c.conn.Close(ctx)
	select {
	case <-c.conn.PgConn().CleanupDone():
	case <-ctx.Done():
	}
	c.conn = nil
}

// close releases the connection when the probe stops.
func (c *connector) close() { c.drop() }

// roundContext returns the context of one round of queries. It does not end
// with the probe's context: a query that is cancelled halfway makes the
// driver cut the connection and open a second one for the cancel request. A
// round in flight when the probe stops gets stopGrace to finish instead, and
// every round is bounded by roundTimeout.
func roundContext(ctx context.Context) (context.Context, context.CancelFunc) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), roundTimeout)
	stop := context.AfterFunc(ctx, func() {
		t := time.NewTimer(stopGrace)
		defer t.Stop()
		select {
		case <-t.C:
			cancel()
		case <-rctx.Done():
		}
	})
	return rctx, func() {
		stop()
		cancel()
	}
}

// closed is what Done returns for a probe that never started.
var closed = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// lifetime implements probe.Closer for both probes: the runtime waits for
// Done before the process exits, so a connection is not cut before it said
// goodbye.
type lifetime struct {
	done chan struct{}
}

// Done implements probe.Closer.
func (l *lifetime) Done() <-chan struct{} {
	if l.done == nil {
		return closed
	}
	return l.done
}

// spawn runs the shared loop in a goroutine and closes Done when the loop
// has stopped and released the connection.
func (l *lifetime) spawn(ctx context.Context, out chan<- probe.Observation, every, interval time.Duration, c *connector, poll func(ctx context.Context) probe.Observation) {
	l.done = make(chan struct{})
	go func() {
		defer close(l.done)
		run(ctx, out, every, interval, c, poll)
	}()
}

// database returns the database name of the DSN, for messages and validation.
func (c *connector) database() string { return c.cfg.Database }

// isMisconfigured reports whether an error means the probe will never work
// as configured (authentication, unknown database) as opposed to a transient
// network or load problem.
func isMisconfigured(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		switch pgErr.SQLState() {
		case "28000", "28P01", "3D000", "42501":
			return true
		}
	}
	return false
}

// tick returns the injected tick or the default.
func tick(spec map[string]any) time.Duration {
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		return d
	}
	return defaultTick
}

// target returns the bound element id.
func target(spec map[string]any) string { return probe.Str(spec, "_target", "") }

// run is the shared loop: poll at interval, re-emit the last observation
// every tick in between so the engine can tell stale from idle, stop and
// close the connection when ctx ends. poll gets the context of its round
// (roundContext), not ctx.
func run(ctx context.Context, out chan<- probe.Observation, every, interval time.Duration, c *connector, poll func(ctx context.Context) probe.Observation) {
	defer c.close()
	if interval < every {
		interval = every
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var last probe.Observation
	var lastPoll time.Time
	for {
		now := time.Now()
		if lastPoll.IsZero() || now.Sub(lastPoll) >= interval {
			rctx, cancel := roundContext(ctx)
			last = poll(rctx)
			cancel()
			lastPoll = now
			if ctx.Err() != nil {
				return
			}
		} else {
			last.At = now
		}
		if !probe.Send(ctx, out, last) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// toFloat converts the scalar types pgx yields in simple protocol.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case int16:
		return float64(x), true
	case int:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(x, "%g", &f); err == nil {
			return f, true
		}
	case pgtype.Numeric:
		if f, err := x.Float64Value(); err == nil && f.Valid {
			return f.Float64, true
		}
	}
	return 0, false
}

// toString renders a scalar as text.
func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}
