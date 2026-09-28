package pgprobe

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// querier is the part of a connection or a transaction a probe reads with.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// reads is the only way a probe sends a statement: it refuses what is not
// one SELECT or one SHOW before it is sent. collect and collectPools take a
// reads, never a connection, so the compiler keeps them behind it.
type reads struct{ q querier }

// Query sends a statement that reads.
func (r reads) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := refuse(sql); err != nil {
		return nil, err
	}
	return r.q.Query(ctx, sql, args...)
}

// QueryRow sends a statement that reads and returns its first row.
func (r reads) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := refuse(sql); err != nil {
		return refused{err}
	}
	return r.q.QueryRow(ctx, sql, args...)
}

// refused is the row of a statement that was not sent.
type refused struct{ err error }

// Scan implements pgx.Row.
func (r refused) Scan(...any) error { return r.err }

// sideEffects are the functions and clauses that change something from
// inside a SELECT. A read-only transaction stops the writes to tables; it
// does not stop a function that ends a session or moves a slot. The list is
// a tripwire for a statement added later, not a parser: what the role may
// do is decided by its grants.
var sideEffects = []string{
	" into ", " for update", " for share", " for no key update", " for key share",
	"pg_terminate_backend", "pg_cancel_backend", "pg_reload_conf", "pg_rotate_logfile",
	"pg_promote", "pg_switch_wal", "pg_create_", "pg_drop_", "pg_replication_slot_advance",
	"pg_replication_origin_", "pg_logical_emit_message", "pg_wal_replay_", "pg_backup_",
	"pg_start_backup", "pg_stop_backup", "pg_stat_reset", "pg_advisory", "pg_notify",
	"set_config", "nextval", "setval", "dblink",
	"lo_import", "lo_export", "lo_create", "lo_unlink", "lo_put", "lo_from_bytea", "lowrite",
}

// refuse returns the error of a statement that is not one SELECT or one
// SHOW, or that names something with a side effect.
func refuse(sql string) error {
	s := strings.ToLower(strings.Join(strings.Fields(sql), " "))
	verb, _, _ := strings.Cut(s, " ")
	switch {
	case verb != "select" && verb != "show":
		return fmt.Errorf("%w: a %s statement was not sent", probe.ErrReadOnly, verb)
	case strings.Contains(s, ";"):
		return fmt.Errorf("%w: more than one statement was not sent", probe.ErrReadOnly)
	}
	for _, w := range sideEffects {
		if strings.Contains(s, w) {
			return fmt.Errorf("%w: a statement with %s was not sent", probe.ErrReadOnly, strings.TrimSpace(w))
		}
	}
	return nil
}

// readOnlyTx runs fn in a read-only transaction, so the server itself
// refuses a write whatever the role is allowed to do. It ends with a commit:
// nothing was changed, and a rollback every round would count as one in
// pg_stat_database, where people watch the rollback ratio. When fn fails the
// transaction is left as it is: the caller drops the connection, which ends
// it.
func readOnlyTx(ctx context.Context, conn *pgx.Conn, fn func(reads) error) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin read only: %w", err)
	}
	if err := fn(reads{tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("end read only: %w", err)
	}
	return nil
}
