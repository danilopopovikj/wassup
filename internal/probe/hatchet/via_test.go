package hatchet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// tunnels is the opener of a test scheme. It hands out the addresses of
// `to` in order, the last one again and again, and counts.
type tunnels struct {
	mu      sync.Mutex
	to      []string
	targets []string
	opened  int
	closed  int
	// closing, when set, runs as a tunnel is closed, before it counts.
	closing func()
}

// tunnelTo registers an opener under scheme whose tunnels end at the
// servers, in order.
func tunnelTo(scheme string, servers ...*httptest.Server) *tunnels {
	f := &tunnels{}
	for _, s := range servers {
		f.to = append(f.to, s.Listener.Addr().String())
	}
	probe.RegisterTunnel(scheme, f.open)
	return f
}

func (f *tunnels) open(_ context.Context, target string, _ map[string]any) (*probe.Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	addr := f.to[min(f.opened, len(f.to)-1)]
	f.opened++
	f.targets = append(f.targets, target)
	return &probe.Tunnel{Addr: addr, Close: func() {
		if f.closing != nil {
			f.closing()
		}
		f.mu.Lock()
		f.closed++
		f.mu.Unlock()
	}}, nil
}

func (f *tunnels) counts() (opened, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened, f.closed
}

// viaSpec is a binding that names no address of its own, only the tunnel.
func viaSpec(t *testing.T, via string, extra map[string]any) map[string]any {
	t.Helper()
	t.Setenv("WASSUP_TEST_HATCHET_TOKEN", testToken())
	spec := map[string]any{
		"via":       via,
		"token_env": "WASSUP_TEST_HATCHET_TOKEN",
		"_target":   "hatchet",
		"_tick":     10 * time.Millisecond,
	}
	for k, v := range extra {
		spec[k] = v
	}
	return spec
}

// queueAPI is a fake API that answers what hatchet.queue reads and records
// the Host header of the requests.
func queueAPI(t *testing.T, hosts *sync.Map) *httptest.Server {
	t.Helper()
	seen := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if hosts != nil {
				hosts.Store(r.Host, true)
			}
			h(w, r)
		}
	}
	return serve(t, routes{
		"/api/v1/tenants/" + testTenant + "/queue-metrics":        seen(rawJSON(`{"queues":{"emails":3},"total":{"numQueued":3,"numPending":1,"numRunning":2}}`)),
		"/api/v1/tenants/" + testTenant + "/worker":               seen(rawJSON(`{"rows":[]}`)),
		"/api/v1/stable/tenants/" + testTenant + "/workflow-runs": seen(rawJSON(`{"rows":[]}`)),
	})
}

// With via naming a tunnel the requests go to the tunnel and name the API
// as the cluster does; nothing is opened before the first request, and the
// tunnel goes when the client is closed.
func TestRequestsGoThroughTheTunnel(t *testing.T) {
	var hosts sync.Map
	srv := queueAPI(t, &hosts)
	f := tunnelTo("test.hatchet.service", srv)
	const via = "test.hatchet.service/shop/hatchet-api:8080"

	p := &QueueProbe{}
	st, err := p.setup(viaSpec(t, via, nil))
	if err != nil {
		t.Fatal(err)
	}
	if opened, _ := f.counts(); opened != 0 {
		t.Fatalf("%d tunnels were opened before the first request", opened)
	}
	o := p.poll(context.Background(), st)
	if o.Err != "" {
		t.Fatalf("err = %q", o.Err)
	}
	if o.Metrics["depth"] != 4 {
		t.Errorf("metrics = %v", o.Metrics)
	}
	if o.Detail["via"] != via || o.Detail["url"] != "http://hatchet-api.shop.svc:8080" {
		t.Errorf("detail = %v", o.Detail)
	}
	if _, ok := hosts.Load("hatchet-api.shop.svc:8080"); !ok {
		t.Errorf("the API was not asked under the name it has in the cluster")
	}
	if opened, closed := f.counts(); opened != 1 || closed != 0 || f.targets[0] != "shop/hatchet-api:8080" {
		t.Errorf("opened %d (%v), closed %d; want one tunnel, still open", opened, f.targets, closed)
	}
	if o = p.poll(context.Background(), st); o.Err != "" {
		t.Fatalf("second round: %q", o.Err)
	}
	if opened, _ := f.counts(); opened != 1 {
		t.Errorf("the second round opened a tunnel of its own: %d", opened)
	}
	st.c.close()
	if _, closed := f.counts(); closed != 1 {
		t.Errorf("the tunnel was closed %d times, want once", closed)
	}
}

// A url next to via keeps its scheme and its path and is what the request
// names; it is not what is dialled. With https the certificate is checked
// against the host of the url.
func TestURLThroughTheTunnelKeepsTLS(t *testing.T) {
	var host atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host.Store(r.Host + r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	f := tunnelTo("test.hatchettls.service", srv)

	// The certificate of the test server is for example.com; nothing of
	// that name is dialled.
	_, c, err := configure(viaSpec(t, "test.hatchettls.service/shop/hatchet-api:443", map[string]any{"url": "https://example.com/hatchet/"}), requirements{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c.conns.TLSClientConfig = &tls.Config{RootCAs: roots}
	status, _, _, err := c.do(context.Background(), "/api/ready", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status %d, err %v", status, err)
	}
	if got, _ := host.Load().(string); got != "example.com/hatchet/api/ready" {
		t.Errorf("the server was asked for %q", got)
	}
	if opened, _ := f.counts(); opened != 1 {
		t.Errorf("opened %d tunnels", opened)
	}

	// Another name than the one in the certificate is refused.
	_, other, err := configure(viaSpec(t, "test.hatchettls.service/shop/hatchet-api:443", map[string]any{"url": "https://hatchet.bookstore.example"}), requirements{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	other.conns.TLSClientConfig = &tls.Config{RootCAs: roots}
	if _, _, _, err := other.do(context.Background(), "/api/ready", nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("err = %v, want the certificate to be refused for another name", err)
	}
}

// A request that gets no answer drops the tunnel, and the next round opens
// a new one.
func TestABrokenTunnelIsReopened(t *testing.T) {
	gone := queueAPI(t, nil)
	srv := queueAPI(t, nil)
	f := tunnelTo("test.hatchetbroken.service", gone, srv)
	gone.Close()

	p := &QueueProbe{}
	st, err := p.setup(viaSpec(t, "test.hatchetbroken.service/shop/hatchet-api:8080", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer st.c.close()
	o := p.poll(context.Background(), st)
	if o.Err == "" || !strings.Contains(o.Err, "via test.hatchetbroken.service/shop/hatchet-api:8080") {
		t.Fatalf("err = %q, want one that names the tunnel", o.Err)
	}
	if h := p.Health(); h.State != probe.HealthDegraded {
		t.Errorf("health = %+v, want degraded", h)
	}
	if opened, closed := f.counts(); opened != 1 || closed != 1 {
		t.Fatalf("opened %d, closed %d; want the tunnel that broke closed", opened, closed)
	}
	o = p.poll(context.Background(), st)
	if o.Err != "" || o.Metrics["depth"] != 4 {
		t.Fatalf("the round after: err %q, metrics %v", o.Err, o.Metrics)
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Errorf("health = %+v, want ok", h)
	}
	if opened, closed := f.counts(); opened != 2 || closed != 1 {
		t.Errorf("opened %d, closed %d; want a second tunnel, open", opened, closed)
	}
}

// An answer of the API, whatever its status, is not a broken tunnel.
func TestAnAnswerKeepsTheTunnel(t *testing.T) {
	srv := serve(t, routes{})
	f := tunnelTo("test.hatchet404.service", srv)
	p := &QueueProbe{}
	st, err := p.setup(viaSpec(t, "test.hatchet404.service/shop/hatchet-api:8080", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer st.c.close()
	for i := 0; i < 2; i++ {
		if o := p.poll(context.Background(), st); !strings.Contains(o.Err, "HTTP 404") {
			t.Fatalf("err = %q", o.Err)
		}
	}
	if opened, closed := f.counts(); opened != 1 || closed != 0 {
		t.Errorf("opened %d, closed %d; want the one tunnel kept", opened, closed)
	}
}

// connections counts the connections a server has open.
type connections struct {
	mu   sync.Mutex
	open map[net.Conn]bool
}

// watch makes srv, not started yet, count its connections.
func (c *connections) watch(srv *httptest.Server) {
	c.open = map[net.Conn]bool{}
	srv.Config.ConnState = func(conn net.Conn, s http.ConnState) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch s {
		case http.StateNew:
			c.open[conn] = true
		case http.StateClosed, http.StateHijacked:
			delete(c.open, conn)
		}
	}
}

// none waits until the server has no connection left.
func (c *connections) none(within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		c.mu.Lock()
		n := len(c.open)
		c.mu.Unlock()
		if n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// When the probe stops, the request in flight gets its answer, the
// connections are closed, the tunnel goes after them, and only then the
// probe reports done.
func TestStopClosesTheTunnelAfterTheConnections(t *testing.T) {
	var conns connections
	var asked, answered, cut atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ready" {
			asked.Add(1)
			time.Sleep(200 * time.Millisecond)
			if r.Context().Err() != nil {
				cut.Add(1)
			}
		}
		w.WriteHeader(http.StatusOK)
		answered.Add(1)
	}))
	conns.watch(srv)
	srv.Start()
	defer srv.Close()
	f := tunnelTo("test.hatchetstop.service", srv)
	var inOrder atomic.Bool
	// The fake tunnel cuts nothing, so a server without connections at this
	// point lost them to the probe, before it let go of the tunnel.
	f.closing = func() { inOrder.Store(conns.none(time.Second)) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 8)
	p := &HealthProbe{}
	if err := p.Start(ctx, viaSpec(t, "test.hatchetstop.service/shop/hatchet-api:8080", nil), out); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); asked.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("no request was sent: %+v", p.Health())
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := p.Health()
	cancel()
	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not report done")
	}
	if answered.Load() == 0 || cut.Load() != 0 {
		t.Errorf("the request in flight was cut: answered %d, cut %d", answered.Load(), cut.Load())
	}
	if opened, closed := f.counts(); opened != 1 || closed != 1 {
		t.Errorf("opened %d, closed %d; want the tunnel closed with the probe", opened, closed)
	}
	if !inOrder.Load() {
		t.Error("the tunnel was closed with a connection still open")
	}
	if h := p.Health(); h.State != before.State || h.Message != before.Message {
		t.Errorf("the stop changed the health from %+v to %+v", before, h)
	}
	select {
	case o := <-out:
		t.Errorf("a round cut short by the stop must not be reported: %+v", o)
	default:
	}
}

// Bindings with the same via share one tunnel, which is closed with the
// last of them.
func TestProbesShareOneTunnel(t *testing.T) {
	srv := queueAPI(t, nil)
	f := tunnelTo("test.hatchetshared.service", srv)
	const via = "test.hatchetshared.service/shop/hatchet-api:8080"

	type running struct {
		cancel context.CancelFunc
		done   <-chan struct{}
	}
	var probes []running
	start := func(p interface {
		probe.Probe
		probe.Closer
	}, extra map[string]any) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		out := make(chan probe.Observation, 64)
		if err := p.Start(ctx, viaSpec(t, via, extra), out); err != nil {
			t.Fatal(err)
		}
		select {
		case o := <-out:
			if o.Err != "" {
				t.Fatalf("%s: %s", p.Kind(), o.Err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s reported nothing", p.Kind())
		}
		probes = append(probes, running{cancel, p.Done()})
	}
	start(&QueueProbe{}, nil)
	start(&QueueProbe{}, map[string]any{"queue": "emails"})
	start(&WorkersProbe{}, nil)
	start(&HealthProbe{}, nil)
	if opened, closed := f.counts(); opened != 1 || closed != 0 {
		t.Fatalf("four bindings opened %d tunnels and closed %d, want one, open", opened, closed)
	}
	for i, r := range probes {
		if _, closed := f.counts(); closed != 0 {
			t.Fatalf("the tunnel was closed with %d bindings still running", len(probes)-i)
		}
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(3 * time.Second):
			t.Fatal("a probe did not report done")
		}
	}
	if opened, closed := f.counts(); opened != 1 || closed != 1 {
		t.Errorf("opened %d, closed %d; want the one tunnel closed with the last binding", opened, closed)
	}
}

func TestValidateVia(t *testing.T) {
	tunnelTo("test.hatchetvalid.service", queueAPI(t, nil))
	kinds := []probe.Probe{&HealthProbe{}, &QueueProbe{}, &WorkersProbe{}, &WorkflowProbe{}}
	for name, tc := range map[string]struct {
		spec map[string]any
		want string // part of the error, "" for a valid spec
	}{
		"a tunnel and no url":   {map[string]any{"via": "test.hatchetvalid.service/shop/hatchet-api:8080"}, ""},
		"a tunnel, a port name": {map[string]any{"via": "test.hatchetvalid.service/shop/hatchet-api:http"}, ""},
		"a tunnel and a url":    {map[string]any{"via": "test.hatchetvalid.service/shop/hatchet-api:8080", "url": "https://hatchet.bookstore.example/api"}, ""},
		"a label and a url":     {map[string]any{"via": "bastion", "url": "https://hatchet.bookstore.example"}, ""},
		"no namespace":          {map[string]any{"via": "test.hatchetvalid.service/hatchet-api:8080"}, "<namespace>/<name>:<port>"},
		"no port":               {map[string]any{"via": "test.hatchetvalid.service/shop/hatchet-api", "url": "https://hatchet.bookstore.example"}, "<namespace>/<name>:<port>"},
		"a label and no url":    {map[string]any{"via": "bastion"}, `"url" is required`},
		"an unknown scheme":     {map[string]any{"via": "test.unknown/shop/hatchet-api:8080"}, `"url" is required`},
		"no via and no url":     {map[string]any{}, `"url" is required`},
		"a tunnel, a bad url":   {map[string]any{"via": "test.hatchetvalid.service/shop/hatchet-api:8080", "url": "hatchet-api:8080"}, "absolute http(s) URL"},
		"a url that is no text": {map[string]any{"via": "test.hatchetvalid.service/shop/hatchet-api:8080", "url": 8080}, "url must be a string"},
	} {
		for _, p := range kinds {
			spec := map[string]any{"workflow": "nightly-report"}
			for k, v := range tc.spec {
				spec[k] = v
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
	for _, p := range kinds {
		acc, _ := probe.AccessFor(p.Kind())
		fields := strings.Join(acc.SpecFields, " ")
		for _, f := range []string{"via", "kubeconfig", "context"} {
			if !strings.Contains(" "+fields+" ", " "+f+" ") {
				t.Errorf("%s does not list %s in its spec fields", p.Kind(), f)
			}
		}
		if !strings.Contains(acc.Needs, "pods/portforward") {
			t.Errorf("%s does not say what via needs", p.Kind())
		}
	}
}

// Without a tunnel nothing changes: the url is what is dialled, on the
// transport every probe shares, and a via that is a label opens nothing.
func TestWithoutATunnelTheURLIsDialled(t *testing.T) {
	srv := queueAPI(t, nil)
	f := tunnelTo("test.hatchetunused.service", srv)
	for name, extra := range map[string]map[string]any{
		"no via":  nil,
		"a label": {"via": "bastion"},
	} {
		p := &QueueProbe{}
		st, err := p.setup(specFor(t, srv, extra))
		if err != nil {
			t.Fatal(err)
		}
		if st.c.via != nil || st.c.conns != nil {
			t.Errorf("%s: the client got a tunnel", name)
		}
		o := p.poll(context.Background(), st)
		if o.Err != "" || o.Metrics["depth"] != 4 || o.Detail["url"] != srv.URL {
			t.Errorf("%s: err %q, metrics %v, detail %v", name, o.Err, o.Metrics, o.Detail)
		}
		if via, _ := extra["via"].(string); via != "" && o.Detail["via"] != via {
			t.Errorf("%s: the detail does not name the label: %v", name, o.Detail)
		} else if _, has := o.Detail["via"]; via == "" && has {
			t.Errorf("%s: the detail names a via: %v", name, o.Detail)
		}
		st.c.close()
	}
	if opened, _ := f.counts(); opened != 0 {
		t.Errorf("%d tunnels were opened", opened)
	}
}
