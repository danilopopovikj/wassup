package electric

import (
	"context"
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
	mu     sync.Mutex
	to     []string
	opened int
	closed int
}

// tunnelTo registers an opener under scheme whose tunnels end at the
// servers, in order.
func tunnelTo(scheme string, servers ...*httptest.Server) *tunnels {
	f := &tunnels{}
	for _, s := range servers {
		f.to = append(f.to, s.Listener.Addr().String())
	}
	probe.RegisterTunnel(scheme, func(context.Context, string, map[string]any) (*probe.Tunnel, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		addr := f.to[min(f.opened, len(f.to)-1)]
		f.opened++
		return &probe.Tunnel{Addr: addr, Close: func() {
			f.mu.Lock()
			f.closed++
			f.mu.Unlock()
		}}, nil
	})
	return f
}

func (f *tunnels) counts() (opened, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened, f.closed
}

// through returns a probe set up the way Start does it for spec, with a
// short live poll.
func through(t *testing.T, spec map[string]any) (*Sync, config) {
	t.Helper()
	p := &Sync{}
	if err := p.Validate(spec); err != nil {
		t.Fatal(err)
	}
	c, err := parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	p.tunnel = probe.NewVia(spec)
	p.client = newClient(c.timeout, p.tunnel)
	p.live = newClient(300*time.Millisecond, p.tunnel)
	t.Cleanup(p.tunnel.Close)
	return p, c
}

// With via naming a tunnel the requests go to the tunnel and name Electric
// as the cluster does; a url is not needed.
func TestRequestsGoThroughTheTunnel(t *testing.T) {
	var hosts sync.Map
	e := &electricServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts.Store(r.Host, true)
		e.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	f := tunnelTo("test.electric.service", srv)
	const via = "test.electric.service/shop/electric:3000"

	p, c := through(t, map[string]any{"via": via, "table": "public.issues", "_target": "electric"})
	if opened, _ := f.counts(); opened != 0 {
		t.Fatalf("%d tunnels were opened before the first request", opened)
	}
	o := p.poll(context.Background(), c)
	if o.Err != "" {
		t.Fatalf("err = %q", o.Err)
	}
	if o.Metrics["ready"] != 1 || o.Metrics["up_to_date"] != 1 {
		t.Errorf("metrics = %v", o.Metrics)
	}
	if o.Detail["via"] != via || o.Detail["url"] != "http://electric.shop.svc:3000" {
		t.Errorf("detail = %v", o.Detail)
	}
	if _, ok := hosts.Load("electric.shop.svc:3000"); !ok {
		t.Error("Electric was not asked under the name it has in the cluster")
	}
	if e.shapes.Load() != 1 || e.lives.Load() != 1 {
		t.Errorf("shape/live requests = %d/%d", e.shapes.Load(), e.lives.Load())
	}
	if opened, closed := f.counts(); opened != 1 || closed != 0 {
		t.Errorf("opened %d, closed %d; want one tunnel for the three requests, still open", opened, closed)
	}
	p.tunnel.Close()
	if _, closed := f.counts(); closed != 1 {
		t.Errorf("the tunnel was closed %d times, want once", closed)
	}
}

// A url next to via keeps its path and is what the request names.
func TestURLThroughTheTunnelKeepsItsPath(t *testing.T) {
	var asked atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Store(r.Host + r.URL.Path)
		writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
	}))
	defer srv.Close()
	tunnelTo("test.electricpath.service", srv)
	p, c := through(t, map[string]any{"via": "test.electricpath.service/shop/electric:3000", "url": "http://sync.bookstore.example/electric/"})
	if o := p.poll(context.Background(), c); o.Err != "" || o.Metrics["ready"] != 1 {
		t.Fatalf("err %q, metrics %v", o.Err, o.Metrics)
	}
	if got, _ := asked.Load().(string); got != "sync.bookstore.example/electric/v1/health" {
		t.Errorf("the server was asked for %q", got)
	}
}

// A request that gets no answer drops the tunnel, and the next round opens
// a new one.
func TestABrokenTunnelIsReopened(t *testing.T) {
	gone := httptest.NewServer((&electricServer{}).handler())
	srv := httptest.NewServer((&electricServer{}).handler())
	defer srv.Close()
	f := tunnelTo("test.electricbroken.service", gone, srv)
	gone.Close()

	p, c := through(t, map[string]any{"via": "test.electricbroken.service/shop/electric:3000"})
	o := p.poll(context.Background(), c)
	if o.Err == "" || !strings.Contains(o.Err, "via test.electricbroken.service/shop/electric:3000") {
		t.Fatalf("err = %q, want one that names the tunnel", o.Err)
	}
	if o.Metrics != nil {
		t.Errorf("a read that failed has no numbers: %v", o.Metrics)
	}
	if opened, closed := f.counts(); opened != 1 || closed != 1 {
		t.Fatalf("opened %d, closed %d; want the tunnel that broke closed", opened, closed)
	}
	o = p.poll(context.Background(), c)
	if o.Err != "" || o.Metrics["ready"] != 1 {
		t.Fatalf("the round after: err %q, metrics %v", o.Err, o.Metrics)
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Errorf("health = %+v, want ok", h)
	}
	if opened, closed := f.counts(); opened != 2 || closed != 1 {
		t.Errorf("opened %d, closed %d; want a second tunnel, open", opened, closed)
	}
}

// A live poll that holds until the probe stops waiting is Electric being up
// to date, not a tunnel that broke.
func TestAQuietLivePollKeepsTheTunnel(t *testing.T) {
	e := &electricServer{liveWait: 600 * time.Millisecond}
	srv := httptest.NewServer(e.handler())
	defer srv.Close()
	f := tunnelTo("test.electriclive.service", srv)
	p, c := through(t, map[string]any{"via": "test.electriclive.service/shop/electric:3000", "table": "public.issues"})
	for i := 0; i < 2; i++ {
		o := p.poll(context.Background(), c)
		if o.Err != "" || o.Metrics["up_to_date"] != 1 {
			t.Fatalf("round %d: err %q, metrics %v", i, o.Err, o.Metrics)
		}
	}
	if opened, closed := f.counts(); opened != 1 || closed != 0 {
		t.Errorf("opened %d, closed %d; want the one tunnel kept", opened, closed)
	}
}

// When the probe stops, the request in flight gets its answer, the tunnel
// is closed after it, and only then the probe reports done.
func TestStopClosesTheTunnel(t *testing.T) {
	var asked, answered, cut atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		time.Sleep(200 * time.Millisecond)
		if r.Context().Err() != nil {
			cut.Add(1)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
		answered.Add(1)
	}))
	defer srv.Close()
	f := tunnelTo("test.electricstop.service", srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 8)
	p := &Sync{}
	spec := map[string]any{"via": "test.electricstop.service/shop/electric:3000", "_target": "electric", "_tick": time.Second}
	if err := p.Start(ctx, spec, out); err != nil {
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
	if answered.Load() != 1 || cut.Load() != 0 {
		t.Errorf("the request in flight was cut: answered %d, cut %d", answered.Load(), cut.Load())
	}
	if opened, closed := f.counts(); opened != 1 || closed != 1 {
		t.Errorf("opened %d, closed %d; want the tunnel closed with the probe", opened, closed)
	}
	if h := p.Health(); h.State != before.State || h.Message != before.Message {
		t.Errorf("the stop changed the health from %+v to %+v", before, h)
	}
	select {
	case o := <-out:
		t.Errorf("a round cut short by the stop must not be reported: %+v", o)
	default:
	}
	var never Sync
	select {
	case <-never.Done():
	default:
		t.Fatal("a probe that never started holds nothing and is done")
	}
}

// Bindings with the same via share one tunnel, which is closed with the
// last of them.
func TestProbesShareOneTunnel(t *testing.T) {
	srv := httptest.NewServer((&electricServer{}).handler())
	defer srv.Close()
	f := tunnelTo("test.electricshared.service", srv)
	spec := func() map[string]any {
		return map[string]any{"via": "test.electricshared.service/shop/electric:3000", "_target": "electric", "_tick": 10 * time.Millisecond}
	}
	var cancels []context.CancelFunc
	var probes []*Sync
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := make(chan probe.Observation, 64)
		p := &Sync{}
		if err := p.Start(ctx, spec(), out); err != nil {
			t.Fatal(err)
		}
		select {
		case o := <-out:
			if o.Err != "" {
				t.Fatalf("binding %d: %s", i, o.Err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("binding %d reported nothing", i)
		}
		cancels, probes = append(cancels, cancel), append(probes, p)
	}
	if opened, closed := f.counts(); opened != 1 || closed != 0 {
		t.Fatalf("three bindings opened %d tunnels and closed %d, want one, open", opened, closed)
	}
	for i, p := range probes {
		if _, closed := f.counts(); closed != 0 {
			t.Fatalf("the tunnel was closed with %d bindings still running", len(probes)-i)
		}
		cancels[i]()
		select {
		case <-p.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("a probe did not report done")
		}
	}
	if opened, closed := f.counts(); opened != 1 || closed != 1 {
		t.Errorf("opened %d, closed %d; want the one tunnel closed with the last binding", opened, closed)
	}
}

func TestValidateVia(t *testing.T) {
	probe.RegisterTunnel("test.electricvalid.service", func(context.Context, string, map[string]any) (*probe.Tunnel, error) {
		t.Error("validate opened a tunnel")
		return nil, nil
	})
	p := &Sync{}
	for name, tc := range map[string]struct {
		spec map[string]any
		want string // part of the error, "" for a valid spec
	}{
		"a tunnel and no url":   {map[string]any{"via": "test.electricvalid.service/shop/electric:3000"}, ""},
		"a tunnel, a port name": {map[string]any{"via": "test.electricvalid.service/shop/electric:http"}, ""},
		"a tunnel and a url":    {map[string]any{"via": "test.electricvalid.service/shop/electric:3000", "url": "https://sync.bookstore.example"}, ""},
		"a label and a url":     {map[string]any{"via": "bastion", "url": "https://sync.bookstore.example"}, ""},
		"no namespace":          {map[string]any{"via": "test.electricvalid.service/electric:3000"}, "<namespace>/<name>:<port>"},
		"no port":               {map[string]any{"via": "test.electricvalid.service/shop/electric", "url": "https://sync.bookstore.example"}, "<namespace>/<name>:<port>"},
		"a label and no url":    {map[string]any{"via": "bastion"}, `"url" is required`},
		"no via and no url":     {map[string]any{}, `"url" is required`},
		"a tunnel, a bad url":   {map[string]any{"via": "test.electricvalid.service/shop/electric:3000", "url": "electric:3000"}, "absolute http(s) URL"},
		"a url that is no text": {map[string]any{"via": "test.electricvalid.service/shop/electric:3000", "url": 3000}, "url must be a string"},
	} {
		err := p.Validate(tc.spec)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
	acc, _ := probe.AccessFor(KindSync)
	fields := " " + strings.Join(acc.SpecFields, " ") + " "
	for _, f := range []string{"via", "kubeconfig", "context"} {
		if !strings.Contains(fields, " "+f+" ") {
			t.Errorf("%s does not list %s in its spec fields", KindSync, f)
		}
	}
	if !strings.Contains(acc.Needs, "pods/portforward") {
		t.Errorf("%s does not say what via needs", KindSync)
	}
}

// Without a tunnel nothing changes: the url is what is dialled, and a via
// that is a label opens nothing.
func TestWithoutATunnelTheURLIsDialled(t *testing.T) {
	srv := httptest.NewServer((&electricServer{}).handler())
	defer srv.Close()
	f := tunnelTo("test.electricunused.service", srv)
	for name, via := range map[string]string{"no via": "", "a label": "bastion"} {
		spec := map[string]any{"url": srv.URL + "/", "_target": "electric"}
		if via != "" {
			spec["via"] = via
		}
		p, c := through(t, spec)
		if p.tunnel != nil {
			t.Errorf("%s: the probe got a tunnel", name)
		}
		o := p.poll(context.Background(), c)
		if o.Err != "" || o.Metrics["ready"] != 1 || o.Detail["url"] != srv.URL {
			t.Errorf("%s: err %q, metrics %v, detail %v", name, o.Err, o.Metrics, o.Detail)
		}
		if got, has := o.Detail["via"]; (via == "") == has || (has && got != via) {
			t.Errorf("%s: detail = %v", name, o.Detail)
		}
	}
	if opened, _ := f.counts(); opened != 0 {
		t.Errorf("%d tunnels were opened", opened)
	}
}
