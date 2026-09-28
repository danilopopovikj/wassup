package redisprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// relay is the tunnel of a test: a port on this machine that passes what it
// gets on to a server, and knows the connections it carries.
type relay struct {
	ln net.Listener
	to string

	mu       sync.Mutex
	live     map[net.Conn]bool
	accepted int
}

func newRelay(t *testing.T, to string) *relay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{ln: ln, to: to, live: map[net.Conn]bool{}}
	t.Cleanup(r.cut)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			r.mu.Lock()
			r.live[conn] = true
			r.accepted++
			r.mu.Unlock()
			go r.carry(conn)
		}
	}()
	return r
}

// carry passes one connection on until either end closes it.
func (r *relay) carry(conn net.Conn) {
	defer func() {
		conn.Close()
		r.mu.Lock()
		delete(r.live, conn)
		r.mu.Unlock()
	}()
	far, err := net.Dial("tcp", r.to)
	if err != nil {
		return
	}
	defer far.Close()
	go func() {
		_, _ = io.Copy(conn, far)
		conn.Close()
	}()
	_, _ = io.Copy(far, conn)
}

// cut ends the relay the way a port-forward that lost its pod ends: the
// port is gone and the connections with it.
func (r *relay) cut() {
	r.ln.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for conn := range r.live {
		conn.Close()
	}
}

// quiet waits until the relay carries no connection.
func (r *relay) quiet(within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		r.mu.Lock()
		n := len(r.live)
		r.mu.Unlock()
		if n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tunnels is the opener of a test scheme: every tunnel is a relay of its
// own to the server.
type tunnels struct {
	t  *testing.T
	to string

	mu      sync.Mutex
	relays  []*relay
	targets []string
	closed  int
	// inOrder counts the tunnels that carried no connection any more when
	// they were closed.
	inOrder int
}

// tunnelTo registers an opener under scheme whose tunnels lead to addr.
func tunnelTo(t *testing.T, scheme, addr string) *tunnels {
	t.Helper()
	f := &tunnels{t: t, to: addr}
	probe.RegisterTunnel(scheme, f.open)
	return f
}

func (f *tunnels) open(_ context.Context, target string, _ map[string]any) (*probe.Tunnel, error) {
	r := newRelay(f.t, f.to)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relays = append(f.relays, r)
	f.targets = append(f.targets, target)
	return &probe.Tunnel{Addr: r.ln.Addr().String(), Close: func() {
		// The relay cuts nothing by itself, so one that is quiet here lost
		// its connections to the probe, before the probe let go of it.
		quiet := r.quiet(time.Second)
		r.cut()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.closed++
		if quiet {
			f.inOrder++
		}
	}}, nil
}

func (f *tunnels) counts() (opened, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.relays), f.closed
}

// last returns the tunnel that was opened last.
func (f *tunnels) last() *relay {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.relays[len(f.relays)-1]
}

// carried returns how many connections went through the tunnels.
func (f *tunnels) carried() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.relays {
		r.mu.Lock()
		n += r.accepted
		r.mu.Unlock()
	}
	return n
}

// started is a probe that runs.
type started struct {
	p      probe.Probe
	out    chan probe.Observation
	cancel context.CancelFunc
}

// start runs a probe on spec.
func start(t *testing.T, p probe.Probe, spec map[string]any) *started {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &started{p: p, out: make(chan probe.Observation, 64), cancel: cancel}
	if _, set := spec["_tick"]; !set {
		spec["_tick"] = 20 * time.Millisecond
	}
	spec["_target"] = "bound"
	if err := p.Start(ctx, spec, s.out); err != nil {
		t.Fatal(err)
	}
	return s
}

// next returns the next observation.
func (s *started) next(t *testing.T) probe.Observation {
	t.Helper()
	select {
	case o := <-s.out:
		return o
	case <-time.After(3 * time.Second):
		t.Fatalf("%s reported nothing: %+v", s.p.Kind(), s.p.Health())
		return probe.Observation{}
	}
}

// until returns the first observation that ok accepts.
func (s *started) until(t *testing.T, ok func(probe.Observation) bool) probe.Observation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		o := s.next(t)
		if ok(o) {
			return o
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the last observation was %+v", s.p.Kind(), o)
		}
	}
}

// stop ends the probe and waits until it has released what it held.
func (s *started) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	select {
	case <-s.p.(probe.Closer).Done():
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not report done", s.p.Kind())
	}
}

// With via naming a tunnel the commands go through the tunnel and no
// address is needed; the tunnel is opened with the first command and
// closed after the connections when the probe stops.
func TestCommandsGoThroughTheTunnel(t *testing.T) {
	srv := newRecordingRedis(t)
	f := tunnelTo(t, "test.redis.service", srv.ln.Addr().String())
	const via = "test.redis.service/shop/cache:6379"

	list := start(t, &ListProbe{}, map[string]any{"via": via, "key": "orders"})
	o := list.next(t)
	if o.Err != "" || o.Metrics["depth"] != 3 {
		t.Fatalf("redis.list: %+v", o)
	}
	if o.Detail["via"] != via || o.Detail["key"] != "orders" {
		t.Errorf("redis.list: detail = %v", o.Detail)
	}
	if opened, closed := f.counts(); opened != 1 || closed != 0 || f.targets[0] != "shop/cache:6379" {
		t.Fatalf("opened %d (%v), closed %d; want one tunnel, open", opened, f.targets, closed)
	}
	if f.carried() == 0 {
		t.Error("the tunnel carried no connection")
	}
	list.stop(t)
	if opened, closed := f.counts(); opened != 1 || closed != 1 || f.inOrder != 1 {
		t.Errorf("opened %d, closed %d, %d of them after their connections; want the tunnel closed once, in order", opened, closed, f.inOrder)
	}

	info := start(t, &InfoProbe{}, map[string]any{"via": via})
	o = info.next(t)
	if o.Err != "" || o.Metrics["clients"] != 4 || o.Detail["via"] != via {
		t.Fatalf("redis.info: %+v", o)
	}
	info.stop(t)

	celery := start(t, &CeleryProbe{}, map[string]any{"via": via, "queue": "orders"})
	o = celery.next(t)
	if o.Err != "" || o.Metrics["depth"] != 3 || o.Detail["via"] != via {
		t.Fatalf("celery.queue: %+v", o)
	}
	celery.stop(t)
	if opened, closed := f.counts(); opened != 3 || closed != 3 || f.inOrder != 3 {
		t.Errorf("opened %d, closed %d, %d in order; want one tunnel per probe, one after the other", opened, closed, f.inOrder)
	}
}

// The address of a spec with via is the name of the server, for the
// messages and the certificate, and is never dialled.
func TestTheAddressBehindATunnel(t *testing.T) {
	tunnelTo(t, "test.redisaddr.service", "127.0.0.1:1")
	tunnelTo(t, "test.redisaddr.pod", "127.0.0.1:1")
	for name, tc := range map[string]struct {
		spec map[string]any
		addr string
	}{
		"the service":     {map[string]any{"via": "test.redisaddr.service/shop/cache:6380"}, "cache.shop.svc:6380"},
		"a port by name":  {map[string]any{"via": "test.redisaddr.service/shop/cache:redis"}, "cache.shop.svc:6379"},
		"a pod":           {map[string]any{"via": "test.redisaddr.pod/shop/cache-0:6379"}, "cache-0:6379"},
		"a host of a own": {map[string]any{"via": "test.redisaddr.service/shop/cache:6379", "host": "cache.bookstore.example"}, "cache.bookstore.example:6379"},
		"a url":           {map[string]any{"via": "test.redisaddr.service/shop/cache:6379", "url": "rediss://cache.bookstore.example:6380/2"}, "cache.bookstore.example:6380"},
	} {
		l, err := connect(tc.spec, "url")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if l.opt.Addr != tc.addr || l.via == nil || l.opt.Dialer == nil {
			t.Errorf("%s: addr %q, via %v; want %q through the tunnel", name, l.opt.Addr, l.via, tc.addr)
		}
		if srv := serverOf(l.opt, tc.spec); srv.via != tc.spec["via"] {
			t.Errorf("%s: the messages do not know the tunnel: %+v", name, srv)
		}
	}
}

// A tunnel that ended is dropped by the command that found out, and a new
// one is opened: by the second try of the driver, or by the next round.
func TestABrokenTunnelIsReopened(t *testing.T) {
	srv := newRecordingRedis(t)
	f := tunnelTo(t, "test.redisbroken.service", srv.ln.Addr().String())
	const via = "test.redisbroken.service/shop/cache:6379"

	list := start(t, &ListProbe{}, map[string]any{"via": via, "key": "orders"})
	if o := list.next(t); o.Err != "" {
		t.Fatalf("the first round: %s", o.Err)
	}
	f.last().cut()
	o := list.until(t, func(o probe.Observation) bool {
		if o.Err != "" && o.Metrics != nil {
			t.Errorf("a read that failed has no numbers: %+v", o)
		}
		opened, _ := f.counts()
		return o.Err == "" && opened == 2
	})
	if o.Metrics["depth"] != 3 {
		t.Errorf("after the new tunnel: %+v", o)
	}
	if opened, closed := f.counts(); opened != 2 || closed != 1 {
		t.Errorf("opened %d, closed %d; want the broken tunnel closed and a second one open", opened, closed)
	}
	if h := list.p.Health(); h.State != probe.HealthOK {
		t.Errorf("health = %+v, want ok", h)
	}
	list.stop(t)
	if opened, closed := f.counts(); opened != 2 || closed != 2 {
		t.Errorf("opened %d, closed %d after the stop", opened, closed)
	}
}

// A command that got no answer gives up the client and the tunnel, in that
// order; what the server said, or what the guard refused, gives up neither.
func TestACommandWithoutAnAnswerGivesUpTheTunnel(t *testing.T) {
	srv := newRecordingRedis(t)
	f := tunnelTo(t, "test.redisfailed.service", srv.ln.Addr().String())
	l, err := connect(map[string]any{"via": "test.redisfailed.service/shop/cache:6379"}, "url")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := l.get()
	if _, err := client.LLen(ctx, "orders").Result(); err != nil {
		t.Fatal(err)
	}
	refused := client.Set(ctx, "orders", "1", 0).Err()
	if !errors.Is(refused, probe.ErrReadOnly) {
		t.Fatalf("SET: %v, want it refused", refused)
	}
	l.failed(refused)
	l.failed(redis.Nil)
	if _, closed := f.counts(); closed != 0 || l.get() != client {
		t.Fatal("an answer gave up the tunnel or the client")
	}
	l.failed(io.EOF)
	if opened, closed := f.counts(); opened != 1 || closed != 1 || f.inOrder != 1 {
		t.Fatalf("opened %d, closed %d, %d in order; want the tunnel closed after its connections", opened, closed, f.inOrder)
	}
	if l.get() == client {
		t.Error("the client of the tunnel that broke was kept")
	}
	if n, err := l.get().LLen(ctx, "orders").Result(); err != nil || n != 3 {
		t.Errorf("LLEN through the new tunnel = %d, %v", n, err)
	}
	if opened, _ := f.counts(); opened != 2 {
		t.Errorf("opened %d tunnels, want a second one", opened)
	}
}

// The words for a connection that broke in a tunnel name the tunnel, not
// the address of the server or a firewall.
func TestExplainThroughATunnel(t *testing.T) {
	probe.RegisterTunnel("test.redisexplain.service", func(context.Context, string, map[string]any) (*probe.Tunnel, error) { return nil, nil })
	const via = "test.redisexplain.service/shop/cache:6379"
	spec := map[string]any{"via": via}
	l, err := connect(spec, "url")
	if err != nil {
		t.Fatal(err)
	}
	srv := serverOf(l.opt, spec)
	for _, broke := range []error{io.EOF, syscall.ECONNRESET, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}} {
		msg := srv.explain("LLEN orders", broke)
		if !strings.Contains(msg, "the connection through "+via) || strings.Contains(msg, "firewall") {
			t.Errorf("%v: %q", broke, msg)
		}
	}
	if msg := srv.explain("INFO", errors.New("WRONGPASS invalid username-password pair")); !strings.Contains(msg, "authentication failed") {
		t.Errorf("an answer of the server is worded as before: %q", msg)
	}
	label := serverOf(l.opt, map[string]any{"via": "bastion"})
	if msg := label.explain("LLEN orders", syscall.ECONNRESET); strings.Contains(msg, "bastion") {
		t.Errorf("a label is no tunnel: %q", msg)
	}
}

// What the server answers, an error included, says nothing about the
// tunnel.
func TestAnAnswerKeepsTheTunnel(t *testing.T) {
	// This server knows LLEN and answers everything else with an error.
	srv := newFakeRedis(t, 0)
	f := tunnelTo(t, "test.redisanswer.service", srv.ln.Addr().String())
	info := start(t, &InfoProbe{}, map[string]any{"via": "test.redisanswer.service/shop/cache:6379"})
	for i := 0; i < 3; i++ {
		if o := info.next(t); !strings.Contains(o.Err, "unknown command") {
			t.Fatalf("err = %q", o.Err)
		}
	}
	if opened, closed := f.counts(); opened != 1 || closed != 0 {
		t.Errorf("opened %d, closed %d; want the one tunnel kept", opened, closed)
	}
	info.stop(t)
}

// A tunnel that cannot be opened is said in the words of the tunnel: the
// server was not reached and is not blamed.
func TestATunnelThatCannotBeOpened(t *testing.T) {
	probe.RegisterTunnel("test.redisrefused.service", func(context.Context, string, map[string]any) (*probe.Tunnel, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: io.EOF}
	})
	const via = "test.redisrefused.service/shop/cache:6379"
	list := start(t, &ListProbe{}, map[string]any{"via": via, "key": "orders"})
	o := list.next(t)
	if !strings.Contains(o.Err, "via "+via) || !strings.Contains(o.Err, "could not be opened") || strings.Contains(o.Err, "TLS") {
		t.Errorf("err = %q", o.Err)
	}
	if h := list.p.Health(); h.State != probe.HealthDegraded {
		t.Errorf("health = %+v, want degraded", h)
	}
	list.stop(t)
}

// A command in flight when the probe stops gets its answer through the
// tunnel, which is closed after the connection.
func TestStopLetsTheCommandFinishThroughTheTunnel(t *testing.T) {
	srv := newFakeRedis(t, 200*time.Millisecond)
	f := tunnelTo(t, "test.redisstop.service", srv.ln.Addr().String())
	list := start(t, &ListProbe{}, map[string]any{"via": "test.redisstop.service/shop/cache:6379", "key": "orders", "_tick": time.Second})
	for deadline := time.Now().Add(2 * time.Second); ; {
		srv.mu.Lock()
		asked := srv.asked
		srv.mu.Unlock()
		if asked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command was not sent: %+v", list.p.Health())
		}
		time.Sleep(5 * time.Millisecond)
	}
	list.stop(t)
	srv.mu.Lock()
	answered := srv.answered
	srv.mu.Unlock()
	if answered.IsZero() {
		t.Fatal("the probe was done before the command in flight was answered")
	}
	if opened, closed := f.counts(); opened != 1 || closed != 1 || f.inOrder != 1 {
		t.Errorf("opened %d, closed %d, %d in order; want the tunnel closed after the connection", opened, closed, f.inOrder)
	}
	if h := list.p.Health(); h.State == probe.HealthDegraded && strings.Contains(h.Message, "context") {
		t.Errorf("the stop is not a failure of the server: %+v", h)
	}
}

// Bindings with the same via share one tunnel, which is closed with the
// last of them.
func TestProbesShareOneTunnel(t *testing.T) {
	srv := newRecordingRedis(t)
	f := tunnelTo(t, "test.redisshared.service", srv.ln.Addr().String())
	const via = "test.redisshared.service/shop/cache:6379"
	probes := []*started{
		start(t, &ListProbe{}, map[string]any{"via": via, "key": "orders"}),
		start(t, &ListProbe{}, map[string]any{"via": via, "key": "exports"}),
		start(t, &InfoProbe{}, map[string]any{"via": via}),
		start(t, &CeleryProbe{}, map[string]any{"via": via, "queue": "default"}),
	}
	for _, s := range probes {
		if o := s.next(t); o.Err != "" {
			t.Fatalf("%s: %s", s.p.Kind(), o.Err)
		}
	}
	if opened, closed := f.counts(); opened != 1 || closed != 0 {
		t.Fatalf("four bindings opened %d tunnels and closed %d, want one, open", opened, closed)
	}
	for i, s := range probes {
		if _, closed := f.counts(); closed != 0 {
			t.Fatalf("the tunnel was closed with %d bindings still running", len(probes)-i)
		}
		s.stop(t)
	}
	if opened, closed := f.counts(); opened != 1 || closed != 1 || f.inOrder != 1 {
		t.Errorf("opened %d, closed %d, %d in order; want the one tunnel closed with the last binding", opened, closed, f.inOrder)
	}
}

// tls: true stays TLS through the tunnel, and the certificate is checked
// against the name of the spec, not against the local end of the tunnel.
func TestTLSThroughTheTunnel(t *testing.T) {
	// The certificate of the test server is for example.com.
	web := httptest.NewTLSServer(http.NotFoundHandler())
	defer web.Close()
	roots := x509.NewCertPool()
	roots.AddCert(web.Certificate())
	plain := newRecordingRedis(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: web.TLS.Certificates})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go plain.serve(conn)
		}
	}()
	tunnelTo(t, "test.redistls.service", ln.Addr().String())
	const via = "test.redistls.service/shop/cache:6379"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	l, err := connect(map[string]any{"via": via, "host": "example.com", "tls": true}, "url")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	l.opt.TLSConfig.RootCAs = roots
	if n, err := l.get().LLen(ctx, "orders").Result(); err != nil || n != 3 {
		t.Fatalf("LLEN = %d, %v", n, err)
	}

	other, err := connect(map[string]any{"via": via, "tls": true}, "url")
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	other.opt.TLSConfig.RootCAs = roots
	if _, err := other.get().LLen(ctx, "orders").Result(); err == nil || !isCertificate(err) {
		t.Errorf("err = %v, want the certificate refused for cache.shop.svc", err)
	}
}

// celery.worker reads Flower through the tunnel, under the name Flower has
// in the cluster, and needs no flower_url.
func TestFlowerThroughTheTunnel(t *testing.T) {
	var hosts sync.Map
	flower := &fakeFlower{
		workers: map[string]any{"celery@w": flowerWorker(4, []string{"default"})},
		status:  map[string]bool{"celery@w": true},
		tasks:   map[string]any{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts.Store(r.Host, true)
		flower.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	f := tunnelTo(t, "test.flower.service", srv.Listener.Addr().String())
	const via = "test.flower.service/shop/flower:5555"

	worker := start(t, &WorkerProbe{}, map[string]any{"via": via})
	o := worker.next(t)
	if o.Err != "" || o.Metrics["workers_online"] != 1 {
		t.Fatalf("observation = %+v", o)
	}
	if o.Detail["via"] != via || o.Detail["flower_url"] != "http://flower.shop.svc:5555" {
		t.Errorf("detail = %v", o.Detail)
	}
	if _, ok := hosts.Load("flower.shop.svc:5555"); !ok {
		t.Error("Flower was not asked under the name it has in the cluster")
	}
	second := start(t, &WorkerProbe{}, map[string]any{"via": via, "name": "w"})
	if o := second.next(t); o.Err != "" {
		t.Fatalf("the second binding: %s", o.Err)
	}
	if opened, closed := f.counts(); opened != 1 || closed != 0 {
		t.Fatalf("opened %d, closed %d; want one tunnel for both bindings, open", opened, closed)
	}
	second.stop(t)

	// The tunnel ends: the round that finds out drops it, the one after
	// opens a new one.
	f.last().cut()
	p := worker.p.(*WorkerProbe)
	c := parseWorkerSpec(map[string]any{"via": via, "timeout": "2s"})
	fc := &flowerClient{base: c.flower, http: p.client, unanswered: p.unanswered}
	if o := p.round(context.Background(), fc, c, firstSeen{}); o.Err == "" {
		t.Fatalf("a round through a tunnel that ended: %+v", o)
	}
	if o := p.round(context.Background(), fc, c, firstSeen{}); o.Err != "" || o.Metrics["workers_online"] != 1 {
		t.Fatalf("the round after: %+v", o)
	}
	if opened, closed := f.counts(); opened != 2 || closed != 1 {
		t.Errorf("opened %d, closed %d; want the broken tunnel closed and a second one open", opened, closed)
	}
	worker.stop(t)
	if opened, closed := f.counts(); opened != 2 || closed != 2 || f.inOrder != 2 {
		t.Errorf("opened %d, closed %d, %d in order; want both closed after their connections", opened, closed, f.inOrder)
	}
}

func TestValidateVia(t *testing.T) {
	probe.RegisterTunnel("test.redisvalid.service", func(context.Context, string, map[string]any) (*probe.Tunnel, error) {
		t.Error("validate opened a tunnel")
		return nil, nil
	})
	const via = "test.redisvalid.service/shop/cache:6379"
	// address is the field that names the server of each kind.
	kinds := map[probe.Probe]string{&InfoProbe{}: "addr", &ListProbe{}: "addr", &CeleryProbe{}: "broker", &WorkerProbe{}: "flower_url"}
	addresses := map[string]string{"addr": "cache.bookstore.example:6379", "broker": "redis://broker.bookstore.example:6379/0", "flower_url": "http://flower.bookstore.example:5555"}
	for name, tc := range map[string]struct {
		via     string
		address bool
		want    string // part of the error, "" for a valid spec
	}{
		"a tunnel and no address": {via, false, ""},
		"a tunnel, a port name":   {"test.redisvalid.service/shop/cache:redis", false, ""},
		"a tunnel and an address": {via, true, ""},
		"a label and an address":  {"bastion", true, ""},
		"no via and an address":   {"", true, ""},
		"no namespace":            {"test.redisvalid.service/cache:6379", true, "<namespace>/<name>:<port>"},
		"no port":                 {"test.redisvalid.service/shop/cache", false, "<namespace>/<name>:<port>"},
		"a label and no address":  {"bastion", false, "is required"},
		"an unknown scheme":       {"test.unknown/shop/cache:6379", false, "is required"},
		"no via and no address":   {"", false, "is required"},
	} {
		for p, field := range kinds {
			spec := map[string]any{"key": "orders", "queue": "orders"}
			if tc.via != "" {
				spec["via"] = tc.via
			}
			if tc.address {
				spec[field] = addresses[field]
			}
			err := p.Validate(spec)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("%s, %s: %v", p.Kind(), name, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("%s, %s: got %v, want an error with %q", p.Kind(), name, err, tc.want)
			}
		}
	}
	if err := (&InfoProbe{}).Validate(map[string]any{"via": via, "port": 6380}); err == nil || !strings.Contains(err.Error(), "via already names the port") {
		t.Errorf("a port next to via: %v", err)
	}
	if err := (&WorkerProbe{}).Validate(map[string]any{"via": via, "flower_url": 5555}); err == nil || !strings.Contains(err.Error(), "flower_url must be a string") {
		t.Errorf("a flower_url that is no text: %v", err)
	}
	for p := range kinds {
		acc, _ := probe.AccessFor(p.Kind())
		fields := " " + strings.Join(acc.SpecFields, " ") + " "
		for _, f := range []string{"via", "kubeconfig", "context"} {
			if !strings.Contains(fields, " "+f+" ") {
				t.Errorf("%s does not list %s in its spec fields", p.Kind(), f)
			}
		}
		if !strings.Contains(acc.Needs, "pods/portforward") {
			t.Errorf("%s does not say what via needs", p.Kind())
		}
	}
}

// Without a tunnel nothing changes: the address of the spec is dialled by
// the driver itself, the client lives as long as the probe, and a via that
// is a label opens nothing.
func TestWithoutATunnelTheAddressIsDialled(t *testing.T) {
	srv := newRecordingRedis(t)
	f := tunnelTo(t, "test.redisunused.service", srv.ln.Addr().String())
	for name, via := range map[string]string{"no via": "", "a label": "bastion"} {
		spec := map[string]any{"addr": srv.ln.Addr().String(), "key": "orders"}
		if via != "" {
			spec["via"] = via
		}
		l, err := connect(spec, "url")
		if err != nil {
			t.Fatal(err)
		}
		if l.via != nil || l.opt.Dialer != nil || l.opt.Addr != srv.ln.Addr().String() {
			t.Errorf("%s: the connection got a tunnel: %+v", name, l)
		}
		client := l.get()
		l.failed(io.EOF)
		if l.get() != client {
			t.Errorf("%s: a failed command gave up the client", name)
		}
		l.close()

		list := start(t, &ListProbe{}, spec)
		o := list.next(t)
		if o.Err != "" || o.Metrics["depth"] != 3 {
			t.Errorf("%s: %+v", name, o)
		}
		if got, has := o.Detail["via"]; (via == "") == has || (has && got != via) {
			t.Errorf("%s: detail = %v", name, o.Detail)
		}
		list.stop(t)
	}
	if opened, _ := f.counts(); opened != 0 {
		t.Errorf("%d tunnels were opened", opened)
	}
}
