package pgprobe

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// fakeServer speaks enough of the PostgreSQL protocol for a connection to
// open, get an empty answer to every query and close. It records how each
// connection ended: "terminate" when the client said goodbye, "eof" when the
// connection was cut, "cancel" for a cancel request.
type fakeServer struct {
	ln    net.Listener
	delay time.Duration // before answering a query

	mu      sync.Mutex
	ends    []string
	queries int
	open    sync.WaitGroup
}

func newFakeServer(t *testing.T, delay time.Duration) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, delay: delay}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.open.Add(1)
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeServer) dsn() string {
	return fmt.Sprintf("postgres://wassup@%s/bookstore?sslmode=disable", s.ln.Addr())
}

func (s *fakeServer) end(how string) {
	s.mu.Lock()
	s.ends = append(s.ends, how)
	s.mu.Unlock()
}

func (s *fakeServer) serve(conn net.Conn) {
	defer s.open.Done()
	defer conn.Close()
	be := pgproto3.NewBackend(conn, conn)
	startup, err := be.ReceiveStartupMessage()
	if err != nil {
		s.end("eof")
		return
	}
	if _, ok := startup.(*pgproto3.CancelRequest); ok {
		s.end("cancel")
		return
	}
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := be.Flush(); err != nil {
		s.end("eof")
		return
	}
	for {
		msg, err := be.Receive()
		if err != nil {
			s.end("eof")
			return
		}
		switch msg.(type) {
		case *pgproto3.Terminate:
			s.end("terminate")
			return
		case *pgproto3.Query:
			s.mu.Lock()
			s.queries++
			s.mu.Unlock()
			time.Sleep(s.delay)
			be.Send(&pgproto3.EmptyQueryResponse{})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := be.Flush(); err != nil {
				s.end("eof")
				return
			}
		}
	}
}

// endings waits until every connection is over and returns how each ended.
func (s *fakeServer) endings(t *testing.T) []string {
	t.Helper()
	done := make(chan struct{})
	go func() { s.open.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a connection to the server is still open")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ends...)
}

func (s *fakeServer) connector(t *testing.T) *connector {
	t.Helper()
	cfg, err := pgx.ParseConfig(s.dsn())
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	return &connector{cfg: cfg}
}

// pinger is a poll that opens the connection and sends one query.
func pinger(c *connector, started chan<- struct{}, errs chan<- error) func(context.Context) probe.Observation {
	return func(ctx context.Context) probe.Observation {
		o := probe.Observation{Target: "db", At: time.Now()}
		conn, err := c.acquire(ctx)
		if err == nil {
			if started != nil {
				started <- struct{}{}
			}
			err = conn.Ping(ctx)
		}
		if err != nil {
			c.drop()
			o.Err = err.Error()
		}
		if errs != nil {
			errs <- err
		}
		return o
	}
}

func TestRunSaysGoodbyeOnStop(t *testing.T) {
	srv := newFakeServer(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 8)
	done := make(chan struct{})
	c := srv.connector(t)
	go func() {
		run(ctx, out, 10*time.Millisecond, 10*time.Millisecond, c, pinger(c, nil, nil))
		close(done)
	}()
	select {
	case o := <-out:
		if o.Err != "" {
			t.Fatalf("first round failed: %s", o.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no observation")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop on ctx cancel")
	}
	if ends := srv.endings(t); len(ends) != 1 || ends[0] != "terminate" {
		t.Errorf("the connection should end with a terminate message, got %v", ends)
	}
}

// A probe that is stopped in the middle of a round lets the query finish and
// then says goodbye. Cancelling the query instead makes the driver cut the
// connection and open a second one for the cancel request, which is what
// takes a port-forward down with it.
func TestStopInTheMiddleOfARoundStillSaysGoodbye(t *testing.T) {
	srv := newFakeServer(t, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 8)
	started := make(chan struct{}, 8)
	errs := make(chan error, 8)
	done := make(chan struct{})
	c := srv.connector(t)
	go func() {
		run(ctx, out, time.Second, time.Second, c, pinger(c, started, errs))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the round did not start")
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop on ctx cancel")
	}
	if err := <-errs; err != nil {
		t.Errorf("the round in flight should finish, got %v", err)
	}
	if ends := srv.endings(t); len(ends) != 1 || ends[0] != "terminate" {
		t.Errorf("the connection should end with a terminate message, got %v", ends)
	}
}

// A server that does not answer must not hold the shutdown for the whole
// round: the round gets stopGrace and the connection is released after it.
func TestStopDoesNotWaitForAHungRound(t *testing.T) {
	srv := newFakeServer(t, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 8)
	started := make(chan struct{}, 8)
	done := make(chan struct{})
	c := srv.connector(t)
	go func() {
		run(ctx, out, time.Second, time.Second, c, pinger(c, started, nil))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the round did not start")
	}
	begin := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(stopGrace + closeTimeout + time.Second):
		t.Fatal("run waited for the hung round")
	}
	if took := time.Since(begin); took < stopGrace {
		t.Errorf("the round in flight should get %s, run stopped after %s", stopGrace, took)
	}
}

func TestProbeIsDoneWhenTheConnectionIsReleased(t *testing.T) {
	srv := newFakeServer(t, 0)
	t.Setenv("WASSUP_TEST_DSN", srv.dsn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 8)
	p := &StatsProbe{}
	spec := map[string]any{"dsn_env": "WASSUP_TEST_DSN", "_target": "db", "_tick": 10 * time.Millisecond}
	if err := p.Start(ctx, spec, out); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("no observation")
	}
	select {
	case <-p.Done():
		t.Fatal("done before the probe was stopped")
	default:
	}
	cancel()
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the probe did not report done")
	}
	for _, e := range srv.endings(t) {
		if e != "terminate" {
			t.Errorf("every connection should end with a terminate message, got %q", e)
		}
	}
	var never StatsProbe
	select {
	case <-never.Done():
	case <-time.After(time.Second):
		t.Fatal("a probe that never started holds nothing and is done")
	}
}
