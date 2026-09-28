package redisprobe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// recordingRedis answers the reads of the probes and records every command
// it receives, the ones of the handshake included.
type recordingRedis struct {
	ln net.Listener

	mu   sync.Mutex
	seen []string
}

func newRecordingRedis(t *testing.T) *recordingRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &recordingRedis{ln: ln}
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

func (s *recordingRedis) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		cmd, err := command(r)
		if err != nil || len(cmd) == 0 {
			return
		}
		name := strings.ToLower(cmd[0])
		s.mu.Lock()
		s.seen = append(s.seen, name)
		s.mu.Unlock()
		switch name {
		case "llen":
			fmt.Fprint(conn, ":3\r\n")
		case "lindex":
			fmt.Fprint(conn, "$2\r\n{}\r\n")
		case "info":
			fmt.Fprint(conn, "$20\r\nconnected_clients:4\r\n\r\n")
		default:
			fmt.Fprint(conn, "-ERR unknown command\r\n")
		}
	}
}

func (s *recordingRedis) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *recordingRedis) client(t *testing.T) *redis.Client {
	t.Helper()
	opt, err := options(map[string]any{"addr": s.ln.Addr().String()}, "url")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(opt)
	t.Cleanup(func() { c.Close() })
	return c
}

// Every command that changes a key, the server or another client is
// refused before it is sent; the reads of the probes go through.
func TestClientRefusesWhatIsNotARead(t *testing.T) {
	srv := newRecordingRedis(t)
	c := srv.client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if n, err := c.LLen(ctx, "orders").Result(); err != nil || n != 3 {
		t.Fatalf("LLEN = %d, %v", n, err)
	}
	if _, err := c.LIndex(ctx, "orders", -1).Result(); err != nil {
		t.Fatalf("LINDEX: %v", err)
	}
	if _, err := c.Info(ctx).Result(); err != nil {
		t.Fatalf("INFO: %v", err)
	}

	writes := map[string]func() error{
		"set":           func() error { return c.Set(ctx, "k", "v", 0).Err() },
		"del":           func() error { return c.Del(ctx, "orders").Err() },
		"lpush":         func() error { return c.LPush(ctx, "orders", "x").Err() },
		"rpop":          func() error { return c.RPop(ctx, "orders").Err() },
		"expire":        func() error { return c.Expire(ctx, "orders", time.Second).Err() },
		"flushall":      func() error { return c.FlushAll(ctx).Err() },
		"config set":    func() error { return c.ConfigSet(ctx, "maxmemory", "1").Err() },
		"client kill":   func() error { return c.ClientKill(ctx, "10.0.0.1:1").Err() },
		"publish":       func() error { return c.Publish(ctx, "events", "x").Err() },
		"eval":          func() error { return c.Eval(ctx, "return 1", nil).Err() },
		"shutdown":      func() error { return c.Shutdown(ctx).Err() },
		"raw":           func() error { return c.Do(ctx, "DEL", "orders").Err() },
		"raw lowercase": func() error { return c.Do(ctx, "flushdb").Err() },
		"pipeline": func() error {
			_, err := c.Pipelined(ctx, func(p redis.Pipeliner) error {
				p.LLen(ctx, "orders")
				p.Del(ctx, "orders")
				return nil
			})
			return err
		},
		"transaction": func() error {
			_, err := c.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.Incr(ctx, "counter")
				return nil
			})
			return err
		},
	}
	for name, write := range writes {
		if err := write(); !errors.Is(err, probe.ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrReadOnly", name, err)
		}
	}

	reads := map[string]bool{"info": true, "llen": true, "lindex": true}
	handshake := map[string]bool{"hello": true, "client": true, "auth": true, "select": true, "ping": true}
	for _, name := range srv.commands() {
		if !reads[name] && !handshake[name] {
			t.Errorf("the server received %s", name)
		}
	}
}

// The probes still read through the guarded client.
func TestProbeReadsThroughTheGuard(t *testing.T) {
	srv := newRecordingRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 8)
	p := &ListProbe{}
	spec := map[string]any{"addr": srv.ln.Addr().String(), "key": "orders", "_target": "queue", "_tick": time.Second}
	if err := p.Start(ctx, spec, out); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-out:
		if o.Err != "" || o.Metrics["depth"] != 3 {
			t.Errorf("observation = %+v, want a depth of 3", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no observation")
	}
	cancel()
	<-p.Done()
}
