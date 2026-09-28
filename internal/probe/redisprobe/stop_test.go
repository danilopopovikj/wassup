package redisprobe

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// fakeRedis answers LLEN after a delay and refuses everything else, which
// is enough for a client to connect. It records when it answered.
type fakeRedis struct {
	ln    net.Listener
	delay time.Duration

	mu       sync.Mutex
	asked    bool
	answered time.Time
}

func newFakeRedis(t *testing.T, delay time.Duration) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeRedis{ln: ln, delay: delay}
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

// command reads one RESP array of bulk strings.
func command(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if line, err = r.ReadString('\n'); err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		out = append(out, string(buf[:size]))
	}
	return out, nil
}

func (s *fakeRedis) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		cmd, err := command(r)
		if err != nil || len(cmd) == 0 {
			return
		}
		if strings.ToUpper(cmd[0]) != "LLEN" {
			fmt.Fprint(conn, "-ERR unknown command\r\n")
			continue
		}
		s.mu.Lock()
		s.asked = true
		s.mu.Unlock()
		time.Sleep(s.delay)
		if _, err := fmt.Fprint(conn, ":3\r\n"); err != nil {
			return
		}
		s.mu.Lock()
		s.answered = time.Now()
		s.mu.Unlock()
	}
}

// A probe that is stopped while a command is in flight lets the command
// finish and closes the connection after it, and reports done only then.
// Cutting the command instead closes the connection with the answer unread.
func TestStopLetsTheCommandInFlightFinish(t *testing.T) {
	srv := newFakeRedis(t, 200*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 8)
	p := &ListProbe{}
	spec := map[string]any{"addr": srv.ln.Addr().String(), "key": "orders", "_target": "queue", "_tick": time.Second}
	if err := p.Start(ctx, spec, out); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.mu.Lock()
		asked := srv.asked
		srv.mu.Unlock()
		if asked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command was not sent: %+v", p.Health())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not report done")
	}
	srv.mu.Lock()
	answered := srv.answered
	srv.mu.Unlock()
	if answered.IsZero() {
		t.Fatal("the probe was done before the command in flight was answered")
	}
	if h := p.Health(); h.State == probe.HealthDegraded && strings.Contains(h.Message, "context") {
		t.Errorf("the stop is not a failure of the server: %+v", h)
	}
	var never InfoProbe
	select {
	case <-never.Done():
	default:
		t.Fatal("a probe that never started holds nothing and is done")
	}
}
