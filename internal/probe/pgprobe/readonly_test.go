package pgprobe

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestRefuse(t *testing.T) {
	reads := []string{
		"SHOW max_connections",
		"SHOW POOLS",
		"select 1",
		"  SELECT\n\tcount(*) FROM pg_stat_activity",
		"SELECT current_setting('server_version'), pg_is_in_recovery()",
		"SELECT pg_wal_lsn_diff(" + walNow + ", replay_lsn)::float8 FROM pg_stat_replication",
		"SELECT slot_name::text FROM pg_replication_slots ORDER BY slot_name",
		"SELECT coalesce(sum(pg_database_size(datname)), 0)::float8 FROM pg_database",
	}
	for _, sql := range reads {
		if err := refuse(sql); err != nil {
			t.Errorf("%q: %v", sql, err)
		}
	}
	writes := []string{
		"",
		"INSERT INTO orders VALUES (1)",
		"update orders set paid = true",
		"DELETE FROM orders",
		"TRUNCATE orders",
		"DROP TABLE orders",
		"ALTER SYSTEM SET max_connections = 1",
		"CREATE TABLE t (id int)",
		"VACUUM",
		"SET default_transaction_read_only = off",
		"BEGIN",
		"COPY orders FROM STDIN",
		"CALL refresh()",
		"DO $$ BEGIN END $$",
		"WITH gone AS (DELETE FROM orders RETURNING *) SELECT count(*) FROM gone",
		"SELECT 1; DELETE FROM orders",
		"SELECT * INTO copy FROM orders",
		"SELECT * FROM orders FOR UPDATE",
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity",
		"SELECT pg_cancel_backend(1)",
		"SELECT pg_drop_replication_slot('electric_slot_default')",
		"SELECT pg_create_physical_replication_slot('s')",
		"SELECT pg_replication_slot_advance('s', '0/0')",
		"SELECT pg_reload_conf()",
		"SELECT pg_promote()",
		"SELECT pg_switch_wal()",
		"SELECT pg_stat_reset()",
		"SELECT set_config('default_transaction_read_only', 'off', false)",
		"SELECT setval('orders_id_seq', 1)",
		"SELECT nextval('orders_id_seq')",
		// The admin console of pgbouncer.
		"PAUSE", "RESUME", "RELOAD", "SHUTDOWN", "KILL bookstore", "RECONNECT", "SUSPEND", "DISABLE bookstore",
	}
	for _, sql := range writes {
		if err := refuse(sql); !errors.Is(err, probe.ErrReadOnly) {
			t.Errorf("%q: err = %v, want ErrReadOnly", sql, err)
		}
	}
}

// recordingServer is a PostgreSQL server that records every statement and
// answers each with an empty result.
type recordingServer struct {
	ln net.Listener

	mu   sync.Mutex
	seen []string
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &recordingServer{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *recordingServer) serve(conn net.Conn) {
	defer conn.Close()
	be := pgproto3.NewBackend(conn, conn)
	if _, err := be.ReceiveStartupMessage(); err != nil {
		return
	}
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	be.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if be.Flush() != nil {
		return
	}
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		q, ok := msg.(*pgproto3.Query)
		if !ok {
			return
		}
		s.mu.Lock()
		s.seen = append(s.seen, q.String)
		s.mu.Unlock()
		be.Send(&pgproto3.EmptyQueryResponse{})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if be.Flush() != nil {
			return
		}
	}
}

func (s *recordingServer) statements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *recordingServer) connect(t *testing.T) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig("postgres://wassup@" + s.ln.Addr().String() + "/bookstore?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// A statement that is refused never reaches the server.
func TestReadsSendsNothingThatWrites(t *testing.T) {
	srv := newRecordingServer(t)
	r := reads{srv.connect(t)}
	ctx := context.Background()

	if _, err := r.Query(ctx, "DELETE FROM orders"); !errors.Is(err, probe.ErrReadOnly) {
		t.Errorf("Query: err = %v, want ErrReadOnly", err)
	}
	var n int
	if err := r.QueryRow(ctx, "SELECT pg_terminate_backend(1)").Scan(&n); !errors.Is(err, probe.ErrReadOnly) {
		t.Errorf("QueryRow: err = %v, want ErrReadOnly", err)
	}
	if _, err := showRows(ctx, r, "POOLS; SHUTDOWN"); !errors.Is(err, probe.ErrReadOnly) {
		t.Errorf("showRows: err = %v, want ErrReadOnly", err)
	}
	if got := srv.statements(); len(got) != 0 {
		t.Fatalf("the server received %q", got)
	}

	rows, err := r.Query(ctx, "SHOW POOLS")
	if err != nil {
		t.Fatalf("SHOW POOLS: %v", err)
	}
	rows.Close()
	if got := srv.statements(); len(got) != 1 || got[0] != "SHOW POOLS" {
		t.Errorf("the server received %q, want SHOW POOLS", got)
	}
}

// A round of pg.stats opens a read-only transaction before its first
// statement, so the server holds it to reading.
func TestCollectRunsInAReadOnlyTransaction(t *testing.T) {
	srv := newRecordingServer(t)
	// The server answers with empty results, so the round ends at its first
	// statement; what matters is what was sent and in which order.
	_, _ = collect(context.Background(), srv.connect(t))
	got := srv.statements()
	if len(got) < 2 {
		t.Fatalf("the server received %q, want the transaction and a statement", got)
	}
	if got[0] != "begin read only" {
		t.Errorf("the first statement is %q, want begin read only", got[0])
	}
	if !strings.HasPrefix(got[1], "SHOW ") {
		t.Errorf("the second statement is %q, want the first read", got[1])
	}
}

// A round that went through ends its transaction with a commit.
func TestReadOnlyTxCommits(t *testing.T) {
	srv := newRecordingServer(t)
	err := readOnlyTx(context.Background(), srv.connect(t), func(r reads) error {
		rows, err := r.Query(context.Background(), "SELECT 1")
		if err != nil {
			return err
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"begin read only", "SELECT 1", "commit"}
	got := srv.statements()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the server received %q, want %q", got, want)
	}
}
