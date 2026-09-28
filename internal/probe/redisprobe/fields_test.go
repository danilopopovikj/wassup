package redisprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestAddressForms(t *testing.T) {
	for name, tc := range map[string]struct {
		spec map[string]any
		addr string
		want string // part of the error, "" for a valid spec
	}{
		"addr":           {map[string]any{"addr": "cache.bookstore.example:6380"}, "cache.bookstore.example:6380", ""},
		"host and port":  {map[string]any{"host": "cache.bookstore.example", "port": 6380}, "cache.bookstore.example:6380", ""},
		"host alone":     {map[string]any{"host": "cache.bookstore.example"}, "cache.bookstore.example:6379", ""},
		"port as text":   {map[string]any{"host": "cache.bookstore.example", "port": "6380"}, "cache.bookstore.example:6380", ""},
		"ipv6":           {map[string]any{"host": "2001:db8::7", "port": 6379}, "[2001:db8::7]:6379", ""},
		"nothing":        {map[string]any{}, "", "is required"},
		"host and addr":  {map[string]any{"host": "cache.bookstore.example", "addr": "cache.bookstore.example:6379"}, "", "pick one"},
		"host and url":   {map[string]any{"host": "cache.bookstore.example", "url": "redis://cache.bookstore.example:6379"}, "", "pick one"},
		"port with addr": {map[string]any{"addr": "cache.bookstore.example:6379", "port": 6380}, "", "port goes with host"},
		"bad port":       {map[string]any{"host": "cache.bookstore.example", "port": "redis"}, "", "port must be a number"},
		"port range":     {map[string]any{"host": "cache.bookstore.example", "port": 0}, "", "between 1 and 65535"},
	} {
		err := validateOptions(tc.spec, "url")
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
		if tc.want != "" {
			continue
		}
		opt, err := options(tc.spec, "url")
		if err != nil || opt.Addr != tc.addr {
			t.Errorf("%s: addr = %v, %v, want %s", name, opt, err, tc.addr)
		}
	}
}

// The password is taken as it is in the environment: nothing to encode.
func TestOptionsFromFields(t *testing.T) {
	const hard = `p@ss:w/rd?#%20 'x'`
	t.Setenv("WASSUP_TEST_REDIS_PW", hard)
	opt, err := options(map[string]any{
		"host": "cache.bookstore.example", "port": 6380, "user": "wassup", "password_env": "WASSUP_TEST_REDIS_PW", "db": 2, "tls": true,
	}, "url")
	if err != nil {
		t.Fatal(err)
	}
	if opt.Addr != "cache.bookstore.example:6380" || opt.Username != "wassup" || opt.Password != hard || opt.DB != 2 || opt.TLSConfig == nil {
		t.Errorf("options = %+v", opt)
	}
	// Validate works where the credentials are not.
	if err := (&InfoProbe{}).Validate(map[string]any{"host": "cache.bookstore.example", "password_env": "WASSUP_TEST_NOT_SET"}); err != nil {
		t.Errorf("validate read the environment: %v", err)
	}
}

// The broker of a Celery queue is named by its URL or by its fields.
func TestCeleryBrokerFromFields(t *testing.T) {
	p := &CeleryProbe{}
	if err := p.Validate(map[string]any{"host": "broker.bookstore.example", "db": 1, "queue": "exports"}); err != nil {
		t.Errorf("fields: %v", err)
	}
	if err := p.Validate(map[string]any{"queue": "exports"}); err == nil {
		t.Error("a queue without a broker should fail")
	}
	if err := p.Validate(map[string]any{"broker": "redis://broker:6379/0", "host": "broker.bookstore.example", "queue": "exports"}); err == nil {
		t.Error("broker and host should fail")
	}
	opt, err := options(map[string]any{"host": "broker.bookstore.example", "db": 1, "queue": "exports"}, "broker")
	if err != nil || opt.Addr != "broker.bookstore.example:6379" || opt.DB != 1 {
		t.Errorf("options = %+v, %v", opt, err)
	}
}

// closedPort returns a port of this machine nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// A refused connection is reported in the first round, at once: nobody
// waits out a timeout to learn that nothing listens.
func TestRefusedConnectionIsReportedAtOnce(t *testing.T) {
	port := closedPort(t)
	for name, tc := range map[string]struct {
		p    probe.Probe
		spec map[string]any
	}{
		"redis.info":   {&InfoProbe{}, map[string]any{}},
		"redis.list":   {&ListProbe{}, map[string]any{"key": "orders"}},
		"celery.queue": {&CeleryProbe{}, map[string]any{"queue": "exports"}},
	} {
		tc.spec["host"], tc.spec["port"], tc.spec["_target"] = "127.0.0.1", port, "cache"
		ctx, cancel := context.WithCancel(context.Background())
		out := make(chan probe.Observation, 4)
		start := time.Now()
		if err := tc.p.Start(ctx, tc.spec, out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		select {
		case o := <-out:
			if took := time.Since(start); took > time.Second {
				t.Errorf("%s: the refusal took %s", name, took)
			}
			want := fmt.Sprintf("connection refused on 127.0.0.1:%d: nothing listens there. Is the port-forward to the server still running?", port)
			if !strings.HasPrefix(o.Err, want) {
				t.Errorf("%s: err = %q", name, o.Err)
			}
			if len(o.Metrics) != 0 {
				t.Errorf("%s: a failed read has no numbers: %v", name, o.Metrics)
			}
			if h := tc.p.Health(); h.State != probe.HealthDegraded {
				t.Errorf("%s: health = %+v", name, h)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("%s: no observation", name)
		}
		cancel()
		<-tc.p.(probe.Closer).Done()
	}
}

func TestExplain(t *testing.T) {
	srv := server{addr: "cache.bookstore.example:6379", user: "wassup", passwordEnv: "REDIS_PASSWORD"}
	for name, tc := range map[string]struct {
		srv  server
		err  error
		want string
	}{
		"password":    {srv, errors.New("WRONGPASS invalid username-password pair or user is disabled."), `authentication failed for user "wassup": check the password in REDIS_PASSWORD and the user name`},
		"no password": {srv, errors.New("NOAUTH Authentication required."), "the server asks for a password and none was sent: set password_env"},
		"none set":    {srv, errors.New("ERR AUTH <password> called without any password configured for the default user."), "the server has no password and one was sent: take password_env out"},
		"acl":         {srv, errors.New("NOPERM this user has no permissions to run the 'info' command"), `user "wassup" is not allowed to run INFO: its ACL needs +info, +llen and +lindex`},
		"loading":     {srv, errors.New("LOADING Redis is loading the dataset in memory"), "the server is loading its data"},
		"not tls":     {srv, errors.New("tls: first record does not look like a TLS handshake"), "does not speak TLS: set tls to false"},
		"name":        {srv, &net.DNSError{Err: "no such host", Name: "cache.bookstore.svc", IsNotFound: true}, "the name cache.bookstore.svc does not resolve on this machine"},
		"refused":     {srv, errors.New("dial tcp 203.0.113.9:6379: connect: connection refused"), "connection refused on cache.bookstore.example:6379: nothing listens there, or a firewall turns the connection down"},
		"timeout":     {srv, context.DeadlineExceeded, "no answer from cache.bookstore.example:6379 within 5s"},
		"tls only":    {srv, io.EOF, "closed the connection before it answered: a server that only takes TLS does that"},
	} {
		got := tc.srv.explain("INFO", tc.err)
		if !strings.HasPrefix(got, tc.want) && !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q in it", name, got, tc.want)
		}
		// Nothing is lost: the command and the error of the driver follow.
		if !strings.HasSuffix(got, "(INFO: "+tc.err.Error()+")") {
			t.Errorf("%s: the cause is gone from %q", name, got)
		}
	}
	// With TLS on, a closed connection is not blamed on TLS.
	withTLS := srv
	withTLS.tls = true
	if got := withTLS.explain("INFO", io.EOF); got != "INFO: EOF" {
		t.Errorf("tls on: %q", got)
	}
	// What nothing is known about stays the command and the error.
	if got := srv.explain("LLEN orders", errors.New("ERR wrong kind of value")); got != "LLEN orders: ERR wrong kind of value" {
		t.Errorf("unknown: %q", got)
	}
	// A refused command is a fault of wassup, not of the server.
	refused := fmt.Errorf("%w: redis command del was not sent", probe.ErrReadOnly)
	if got := srv.explain("DEL", refused); !strings.HasPrefix(got, "DEL: wassup is read only") {
		t.Errorf("read only: %q", got)
	}
}
