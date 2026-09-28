// Package pgprobe implements the PostgreSQL probes of wassup: pg.stats reads
// pg_stat_* views on a server and pg.pool reads the pgbouncer admin console.
// Both use pgx's simple protocol (pgbouncer has no extended protocol), run
// read-only statements only and reuse one connection per binding, reconnecting
// when it breaks. Statements are sent through reads, which refuses what is
// not a SELECT or a SHOW, and pg.stats runs each round in a read-only
// transaction (readonly.go). A connection is always released in order, with a terminate
// message: a connection that is cut instead reaches the server as a reset,
// and a port-forward or an SSH tunnel on the way goes down with it.
package pgprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// roundTimeout bounds one round of queries.
const roundTimeout = 5 * time.Second

// stopGrace is how long a round in flight may still run once the probe was
// told to stop.
const stopGrace = probe.StopGrace

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
	// src says where the settings came from, for the messages that tell a
	// person what to check.
	src source
	// via and spec are set when the binding names a tunnel wassup opens
	// itself (via: k8s.service/<namespace>/<name>:<port>); tunnel is the open
	// one. The DSN or the fields then only say who connects to which
	// database: the host and port to dial are the tunnel's.
	via    string
	spec   map[string]any
	tunnel *probe.Tunnel
}

// newConnector reads the connection from the spec, the DSN in the
// environment variable named by spec[dsnKey] or the fields host, port, user,
// database, sslmode and password_env, and prepares a simple-protocol config.
// appName is set as application_name when non-empty (not for pgbouncer's
// admin console). defaultDB is the database of a spec that gives fields and
// names none.
func newConnector(spec map[string]any, dsnKey, appName, defaultDB string) (*connector, error) {
	cfg, src, err := configFor(spec, dsnKey, defaultDB)
	if err != nil {
		return nil, err
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
	c := &connector{cfg: cfg, src: src}
	if via := probe.Str(spec, "via", ""); via != "" {
		if _, _, ok := probe.SplitVia(via); ok {
			c.via, c.spec = via, spec
		}
	}
	return c, nil
}

// acquire returns the live connection, opening one when needed, through the
// tunnel when the binding names one.
func (c *connector) acquire(ctx context.Context) (*pgx.Conn, error) {
	if c.conn != nil && !c.conn.IsClosed() {
		return c.conn, nil
	}
	cfg := c.cfg
	if c.via != "" {
		if c.tunnel == nil {
			t, err := probe.OpenTunnel(ctx, c.via, c.spec)
			if err != nil {
				// Worded here: the database was not reached yet, and what
				// the cluster said must not be read as its answer.
				return nil, &explained{say: "the tunnel " + c.via + " could not be opened, so the database was not reached", cause: err}
			}
			c.tunnel = t
		}
		var err error
		if cfg, err = through(c.cfg, c.tunnel.Addr); err != nil {
			return nil, fmt.Errorf("via %s: %w", c.via, err)
		}
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return conn, nil
}

// through returns a copy of cfg that dials addr instead of the DSN's host.
// The TLS settings keep the server name of the DSN, so a certificate is
// still checked against the name the database goes by.
func through(cfg *pgx.ConnConfig, addr string) (*pgx.ConnConfig, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, err
	}
	out := cfg.Copy()
	out.Host, out.Port = host, uint16(port)
	for _, f := range out.Fallbacks {
		f.Host, f.Port = host, uint16(port)
	}
	return out, nil
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
	// The tunnel goes after the goodbye went through it. After an error it
	// may be the tunnel that broke, so the next round opens a new one.
	if c.tunnel != nil {
		c.tunnel.Close()
		c.tunnel = nil
	}
}

// close releases the connection when the probe stops.
func (c *connector) close() {
	c.drop()
	if c.tunnel != nil {
		c.tunnel.Close()
		c.tunnel = nil
	}
}

// roundContext returns the context of one round of queries: a query that is
// cancelled halfway makes the driver cut the connection and open a second
// one for the cancel request, so a round does not end with the probe's
// context (probe.RoundContext).
func roundContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return probe.RoundContext(ctx, roundTimeout)
}

// lifetime implements probe.Closer for both probes: the runtime waits for
// Done before the process exits, so a connection is not cut before it said
// goodbye.
type lifetime struct{ probe.Lifetime }

// spawn runs the shared loop in a goroutine; Done closes when the loop has
// stopped and released the connection.
func (l *lifetime) spawn(ctx context.Context, out chan<- probe.Observation, every, interval time.Duration, c *connector, poll func(ctx context.Context) probe.Observation) {
	l.Go(func() { run(ctx, out, every, interval, c, poll) })
}

// failed records a round that could not read its source. A round that was
// cut short because the probe was told to stop says nothing about the
// source: its error is the stop itself and is not reported as the probe's
// health.
func failed(stopping context.Context, h *probe.Health, o *probe.Observation, err error) {
	o.Err = err.Error()
	if stopping.Err() != nil {
		return
	}
	if isMisconfigured(err) {
		h.Set(probe.HealthFailed, o.Err)
	} else {
		h.Set(probe.HealthDegraded, o.Err)
	}
}

// database returns the database name of the connection, for messages and
// validation.
func (c *connector) database() string { return c.cfg.Database }

// role returns the role the connection logs in as.
func (c *connector) role() string {
	if c.cfg == nil {
		return ""
	}
	return c.cfg.User
}

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
