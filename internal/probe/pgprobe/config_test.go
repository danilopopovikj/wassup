package pgprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestValidateConn(t *testing.T) {
	probe.RegisterTunnel("test.tunnel", func(context.Context, string, map[string]any) (*probe.Tunnel, error) {
		return nil, fmt.Errorf("not opened by validate")
	})
	for name, tc := range map[string]struct {
		spec map[string]any
		want string // part of the error, "" for a valid spec
	}{
		"dsn":             {map[string]any{"dsn_env": "PG_DSN"}, ""},
		"fields":          {map[string]any{"host": "db.bookstore.example", "port": 5432, "user": "wassup", "database": "bookstore", "sslmode": "require", "password_env": "PG_PASSWORD"}, ""},
		"port as text":    {map[string]any{"host": "db.bookstore.example", "port": "6432", "user": "wassup"}, ""},
		"no password":     {map[string]any{"host": "db.bookstore.example", "user": "wassup"}, ""},
		"tunnel, no host": {map[string]any{"user": "wassup", "password_env": "PG_PASSWORD", "via": "test.tunnel/bookstore/db-rw:5432"}, ""},
		"nothing":         {map[string]any{}, `"dsn_env" is required`},
		"both":            {map[string]any{"dsn_env": "PG_DSN", "host": "db.bookstore.example", "user": "wassup"}, "pick one"},
		"both, password":  {map[string]any{"dsn_env": "PG_DSN", "password_env": "PG_PASSWORD"}, "pick one"},
		"no user":         {map[string]any{"host": "db.bookstore.example"}, `"user" is required`},
		"no host":         {map[string]any{"user": "wassup"}, `"host" is required`},
		"label, no host":  {map[string]any{"user": "wassup", "via": "bastion"}, `"host" is required`},
		"bad port":        {map[string]any{"host": "db.bookstore.example", "user": "wassup", "port": "postgres"}, "port must be a number"},
		"port range":      {map[string]any{"host": "db.bookstore.example", "user": "wassup", "port": 70000}, "between 1 and 65535"},
		"bad sslmode":     {map[string]any{"host": "db.bookstore.example", "user": "wassup", "sslmode": "on"}, "sslmode"},
		"user not a word": {map[string]any{"host": "db.bookstore.example", "user": 12}, "user must be a string"},
	} {
		err := validateConn(tc.spec, "dsn_env")
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
}

// Validate works where the credentials are not: it never reads the
// environment.
func TestValidateDoesNotReadTheEnvironment(t *testing.T) {
	spec := map[string]any{"host": "db.bookstore.example", "user": "wassup", "password_env": "WASSUP_TEST_NOT_SET"}
	if err := (&StatsProbe{}).Validate(spec); err != nil {
		t.Errorf("pg.stats: %v", err)
	}
	if err := (&PoolProbe{}).Validate(spec); err != nil {
		t.Errorf("pg.pool: %v", err)
	}
	if err := (&PoolProbe{}).Validate(map[string]any{"host": "pooler.bookstore.example", "user": "stats", "database": "bookstore"}); err == nil {
		t.Error("pg.pool reads the admin console, not another database")
	}
	if _, _, err := configFor(spec, "dsn_env", ""); err == nil || !strings.Contains(err.Error(), "WASSUP_TEST_NOT_SET") {
		t.Errorf("a password that is not set should fail the start and name the variable, got %v", err)
	}
}

// hardPassword holds what has to be encoded in a URL.
const hardPassword = `p@ss:w/rd?#%20 'x'\y`

func TestConfigFromFields(t *testing.T) {
	t.Setenv("WASSUP_TEST_PG_PASSWORD", hardPassword)
	cfg, src, err := configFor(map[string]any{
		"host": "db.bookstore.example", "port": 6543, "user": "book keeper", "database": "book'store",
		"sslmode": "disable", "password_env": "WASSUP_TEST_PG_PASSWORD",
	}, "dsn_env", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.bookstore.example" || cfg.Port != 6543 || cfg.User != "book keeper" || cfg.Database != "book'store" {
		t.Errorf("config = %s:%d user %q database %q", cfg.Host, cfg.Port, cfg.User, cfg.Database)
	}
	if cfg.Password != hardPassword {
		t.Errorf("password = %q, want it as it is in the environment", cfg.Password)
	}
	if cfg.TLSConfig != nil {
		t.Error("sslmode disable should leave TLS off")
	}
	if src.passwordEnv != "WASSUP_TEST_PG_PASSWORD" || src.dsnEnv != "" {
		t.Errorf("source = %+v", src)
	}

	// Defaults: the port of PostgreSQL, and the database the caller names.
	cfg, _, err = configFor(map[string]any{"host": "pooler.bookstore.example", "user": "stats"}, "dsn_env", adminDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != defaultPort || cfg.Database != adminDatabase {
		t.Errorf("defaults: port %d database %q", cfg.Port, cfg.Database)
	}
}

func TestConfigFromDSNIsUnchanged(t *testing.T) {
	t.Setenv("WASSUP_TEST_DSN", "postgres://wassup:s3cret@db.bookstore.example:5433/bookstore?sslmode=disable")
	cfg, src, err := configFor(map[string]any{"dsn_env": "WASSUP_TEST_DSN"}, "dsn_env", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.bookstore.example" || cfg.Port != 5433 || cfg.User != "wassup" || cfg.Password != "s3cret" || cfg.Database != "bookstore" {
		t.Errorf("config = %s:%d user %q database %q", cfg.Host, cfg.Port, cfg.User, cfg.Database)
	}
	if src.dsnEnv != "WASSUP_TEST_DSN" {
		t.Errorf("source = %+v", src)
	}
}

// passwordServer asks for the password in clear text and keeps what came.
func passwordServer(t *testing.T) (addr string, got <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	passwords := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		be := pgproto3.NewBackend(conn, conn)
		if _, err := be.ReceiveStartupMessage(); err != nil {
			return
		}
		be.Send(&pgproto3.AuthenticationCleartextPassword{})
		if err := be.Flush(); err != nil {
			return
		}
		if err := be.SetAuthType(pgproto3.AuthTypeCleartextPassword); err != nil {
			return
		}
		msg, err := be.Receive()
		if err != nil {
			return
		}
		if pw, ok := msg.(*pgproto3.PasswordMessage); ok {
			passwords <- pw.Password
		}
		be.Send(&pgproto3.AuthenticationOk{})
		be.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
		be.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
		be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		_ = be.Flush()
		// until the goodbye
		for {
			if _, err := be.Receive(); err != nil {
				return
			}
		}
	}()
	return ln.Addr().String(), passwords
}

// The password reaches the server as it is in the environment, through a
// tunnel that gives the address: the spec names neither host nor port.
func TestFieldsConnectThroughTheTunnel(t *testing.T) {
	addr, got := passwordServer(t)
	probe.RegisterTunnel("test.fields", func(ctx context.Context, target string, spec map[string]any) (*probe.Tunnel, error) {
		return &probe.Tunnel{Addr: addr, Close: func() {}}, nil
	})
	t.Setenv("WASSUP_TEST_PG_PASSWORD", hardPassword)
	c, err := newConnector(map[string]any{
		"user": "wassup", "database": "bookstore", "sslmode": "disable",
		"password_env": "WASSUP_TEST_PG_PASSWORD", "via": "test.fields/bookstore/db-rw:5432",
	}, "dsn_env", "wassup", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Host != "db-rw" || c.cfg.Port != 5432 {
		t.Errorf("the server goes by %s:%d, want the name and port of via", c.cfg.Host, c.cfg.Port)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.acquire(ctx); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.close()
	select {
	case pw := <-got:
		if pw != hardPassword {
			t.Errorf("the server got %q, want %q", pw, hardPassword)
		}
	case <-time.After(time.Second):
		t.Fatal("the server saw no password")
	}
}

func TestTunnelName(t *testing.T) {
	probe.RegisterTunnel("test.service", func(context.Context, string, map[string]any) (*probe.Tunnel, error) { return nil, nil })
	if host, port := tunnelName("test.service/bookstore/db-rw:5432"); host != "db-rw.bookstore.svc" || port != 5432 {
		t.Errorf("service: %s %d", host, port)
	}
	// A named port says nothing about the number.
	if host, port := tunnelName("test.service/bookstore/db-rw:postgres"); host != "db-rw.bookstore.svc" || port != 0 {
		t.Errorf("named port: %s %d", host, port)
	}
	if host, _ := tunnelName("bastion"); host != "" {
		t.Errorf("a label names no server, got %q", host)
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
	for name, p := range map[string]probe.Probe{"pg.stats": &StatsProbe{}, "pg.pool": &PoolProbe{}} {
		ctx, cancel := context.WithCancel(context.Background())
		out := make(chan probe.Observation, 4)
		start := time.Now()
		if err := p.Start(ctx, map[string]any{"host": "127.0.0.1", "port": port, "user": "wassup", "_target": "db"}, out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		select {
		case o := <-out:
			if took := time.Since(start); took > time.Second {
				t.Errorf("%s: the refusal took %s", name, took)
			}
			want := fmt.Sprintf("connection refused on 127.0.0.1:%d: nothing listens there", port)
			if !strings.Contains(o.Err, want) || !strings.Contains(o.Err, viaHint) {
				t.Errorf("%s: err = %q", name, o.Err)
			}
			if strings.Contains(o.Err, "\n") {
				t.Errorf("%s: the error should be one line: %q", name, o.Err)
			}
			if len(o.Metrics) != 0 {
				t.Errorf("%s: a failed read has no numbers: %v", name, o.Metrics)
			}
			if h := p.Health(); h.State != probe.HealthDegraded {
				t.Errorf("%s: health = %+v", name, h)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("%s: no observation", name)
		}
		cancel()
		<-p.(probe.Closer).Done()
	}
}

func TestExplain(t *testing.T) {
	t.Setenv("WASSUP_TEST_PG_PASSWORD", "secret")
	c, err := newConnector(map[string]any{"host": "db.bookstore.example", "user": "app", "database": "bookstore", "password_env": "WASSUP_TEST_PG_PASSWORD"}, "dsn_env", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		err       error
		connected bool
		want      string
	}{
		"password":    {&pgconn.PgError{Code: "28P01", Message: "password authentication failed"}, false, `authentication failed for user "app": check the password in WASSUP_TEST_PG_PASSWORD and the user name`},
		"database":    {&pgconn.PgError{Code: "3D000", Message: `database "bookstore" does not exist`}, false, `database "bookstore" does not exist on this server: check the database name`},
		"tls wanted":  {&pgconn.PgError{Code: "28000", Message: `no pg_hba.conf entry for host "10.0.0.7", user "app", database "bookstore", no encryption`}, false, "the server only takes encrypted connections: set sslmode to require"},
		"not let in":  {&pgconn.PgError{Code: "28000", Message: `no pg_hba.conf entry for host "10.0.0.7", user "app", database "bookstore", SSL encryption`}, false, `the server does not let user "app" in from this address`},
		"tls refused": {errors.New("server refused TLS connection"), false, "the server does not speak TLS: set sslmode to disable or prefer"},
		"name":        {&net.DNSError{Err: "no such host", Name: "db-rw.bookstore.svc", IsNotFound: true}, false, "the name db-rw.bookstore.svc does not resolve on this machine"},
		"refused":     {errors.New("dial tcp 203.0.113.9:5432: connect: connection refused"), false, "connection refused on db.bookstore.example:5432: nothing listens there, or a firewall turns the connection down"},
		"unreachable": {context.DeadlineExceeded, false, "no answer from db.bookstore.example:5432 within 5s"},
		"slow":        {context.DeadlineExceeded, true, "the server did not answer within 5s"},
	} {
		got := c.explain(tc.err, tc.connected)
		if !strings.Contains(got.Error(), tc.want) {
			t.Errorf("%s: %q, want %q in it", name, got, tc.want)
		}
		// Nothing is lost: the error of the driver is in the message and
		// can still be asked for.
		if !strings.Contains(got.Error(), tc.err.Error()) || !errors.Is(got, tc.err) {
			t.Errorf("%s: the cause is gone from %q", name, got)
		}
		if again := c.explain(got, tc.connected); again.Error() != got.Error() {
			t.Errorf("%s: explained twice: %q", name, again)
		}
	}
	// A wrong password will not come right by itself.
	if !isMisconfigured(c.explain(&pgconn.PgError{Code: "28P01"}, false)) {
		t.Error("an explained error keeps what kind of error it is")
	}
	// What nothing is known about stays as it is.
	plain := errors.New("unexpected message")
	if got := c.explain(plain, true); got != plain {
		t.Errorf("unknown error became %q", got)
	}
	// Through a DSN the password is named by the variable of the DSN.
	t.Setenv("WASSUP_TEST_DSN", "postgres://app:pw@db.bookstore.example/bookstore")
	c, err = newConnector(map[string]any{"dsn_env": "WASSUP_TEST_DSN"}, "dsn_env", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.explain(&pgconn.PgError{Code: "28P01"}, false).Error(); !strings.Contains(got, "the password in the DSN of WASSUP_TEST_DSN") {
		t.Errorf("dsn: %q", got)
	}
}

func TestOneLine(t *testing.T) {
	err := errors.New("failed to connect:\n\t127.0.0.1:5432: connection refused\n\t127.0.0.1:5432: connection refused")
	if got := oneLine(err); got != "failed to connect:; 127.0.0.1:5432: connection refused" {
		t.Errorf("oneLine = %q", got)
	}
}

// A role without pg_read_all_stats is told how to get it; what it may not
// see stays out of the numbers.
func TestObserveSaysHowToGrant(t *testing.T) {
	s := Stats{
		MaxConnections: 100, Connections: 12, ActivityHidden: true,
		Replication: []ReplicationRow{replicationRow(nil, "bookstore-db-2", nil, nil)},
	}
	m, _, detail := Observe(s, StatsOptions{Role: "app"})
	for _, k := range []string{"active_connections", "waiters", "lag_bytes"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s was hidden and must not be a number: %v", k, m)
		}
	}
	note, _ := detail["grant_note"].(string)
	want := `role "app" lacks pg_read_all_stats, replication detail and what other sessions do are hidden. To grant it, a superuser runs: GRANT pg_read_all_stats TO "app";`
	if !strings.HasPrefix(note, want) {
		t.Errorf("note = %q", note)
	}
	grant, _ := detail["grant"].(map[string]any)
	if grant["sql"] != `GRANT pg_read_all_stats TO "app";` {
		t.Errorf("sql = %v", grant["sql"])
	}
	yaml, _ := grant["cloudnativepg"].(string)
	for _, line := range []string{"  managed:", "    roles:", "      - name: app", "        login: true", "        inRoles:", "          - pg_read_all_stats"} {
		if !strings.Contains(yaml, line+"\n") {
			t.Errorf("snippet lacks %q:\n%s", line, yaml)
		}
	}

	// On a replication edge only the replication detail is named.
	_, _, detail = Observe(s, StatsOptions{Role: "app", Replica: "bookstore-db-2"})
	if note, _ := detail["grant_note"].(string); !strings.Contains(note, "replication detail is hidden") {
		t.Errorf("edge note = %q", note)
	}

	// A role that sees everything is told nothing.
	_, _, detail = Observe(Stats{Replication: []ReplicationRow{{ApplicationName: "bookstore-db-2", State: "streaming"}}}, StatsOptions{Role: "monitor"})
	if _, ok := detail["grant"]; ok {
		t.Errorf("nothing was hidden: %v", detail["grant"])
	}
	if _, ok := detail["grant_note"]; ok {
		t.Errorf("nothing was hidden: %v", detail["grant_note"])
	}
}

// A tunnel that does not open is the cluster's answer, not the database's.
func TestATunnelThatDoesNotOpenIsNotTheDatabaseRefusing(t *testing.T) {
	probe.RegisterTunnel("test.closed", func(context.Context, string, map[string]any) (*probe.Tunnel, error) {
		return nil, errors.New(`read service shop/bookstore-db-rw: dial tcp 127.0.0.1:1: connect: connection refused`)
	})
	t.Setenv("BOOKSTORE_DB_PASSWORD", "secret")
	spec := map[string]any{"via": "test.closed/shop/bookstore-db-rw:5432", "user": "app", "database": "bookstore", "password_env": "BOOKSTORE_DB_PASSWORD"}
	c, err := newConnector(spec, "dsn_env", "wassup", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.acquire(t.Context())
	if err == nil {
		t.Fatal("no error")
	}
	got := c.explain(err, false).Error()
	if !strings.Contains(got, "the tunnel test.closed/shop/bookstore-db-rw:5432 could not be opened") || !strings.Contains(got, "read service shop/bookstore-db-rw") {
		t.Errorf("error %q must name the tunnel and keep the cause", got)
	}
	if strings.Contains(got, "nothing listens there") {
		t.Errorf("error %q blames the database", got)
	}
}
