package hatchet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

const testTenant = "707d0855-80ab-4e1f-a156-f1c4546cbf52"

// fakeJWT builds an unsigned JWT-shaped token with the given claims.
func fakeJWT(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + ".sig"
}

// routes maps a request path to a handler; unknown paths answer 404.
type routes map[string]http.HandlerFunc

// serve starts a fake Hatchet API that requires the test bearer token.
func serve(t *testing.T, r routes) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.URL.Path, "/api/ready") && !strings.HasPrefix(req.URL.Path, "/api/live") &&
			req.URL.Path != "/api/v1/meta" && req.Header.Get("Authorization") != "Bearer "+testToken() {
			http.Error(w, `{"errors":[{"description":"unauthorized"}]}`, http.StatusUnauthorized)
			return
		}
		h, ok := r[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		h(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testToken is the token every test spec presents.
func testToken() string { return fakeJWT(map[string]any{"sub": testTenant}) }

// jsonOK answers 200 with v.
func jsonOK(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

// rawJSON answers 200 with a literal body.
func rawJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// specFor builds a spec pointing at srv with the test token in the
// environment; the tenant is left to the JWT.
func specFor(t *testing.T, srv *httptest.Server, extra map[string]any) map[string]any {
	t.Helper()
	t.Setenv("WASSUP_TEST_HATCHET_TOKEN", testToken())
	spec := map[string]any{
		"url":       srv.URL,
		"token_env": "WASSUP_TEST_HATCHET_TOKEN",
		"_target":   "hatchet",
		"_tick":     10 * time.Millisecond,
	}
	for k, v := range extra {
		spec[k] = v
	}
	return spec
}

func tenantPath(suffix string) string { return "/api/v1/tenants/" + testTenant + suffix }
func stablePath(suffix string) string { return "/api/v1/stable/tenants/" + testTenant + suffix }

func TestTenantFromToken(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		want   string
		wantOK bool
	}{
		{"tenant_id claim", fakeJWT(map[string]any{"tenant_id": "t-1", "sub": "user"}), "t-1", true},
		{"sub fallback", fakeJWT(map[string]any{"sub": "t-2", "server_url": "https://x"}), "t-2", true},
		{"no claims", fakeJWT(map[string]any{"iss": "hatchet"}), "", false},
		{"not a jwt", "opaque-token", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tenantFromToken(tc.token)
			if tc.wantOK != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("tenant = %q, want %q", got, tc.want)
			}
		})
	}
	// A payload with standard base64 padding stripped by the caller still decodes.
	padded := fakeJWT(map[string]any{"sub": "t-4"})
	parts := strings.Split(padded, ".")
	parts[1] += "="
	if got, err := tenantFromToken(strings.Join(parts, ".")); err != nil || got != "t-4" {
		t.Fatalf("padded payload: %q, %v", got, err)
	}
}

func TestConfigureTenantFromSpecOrToken(t *testing.T) {
	t.Setenv("HATCHET_CLIENT_TOKEN", fakeJWT(map[string]any{"sub": "from-token"}))
	cfg, c, err := configure(map[string]any{"url": "https://hatchet.example/api/"}, requirements{token: true, tenant: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.tenant != "from-token" || c.tenant != "from-token" {
		t.Fatalf("tenant = %q", cfg.tenant)
	}
	if cfg.base != "https://hatchet.example" {
		t.Fatalf("base = %q", cfg.base)
	}
	if cfg.interval != defaultInterval || cfg.timeout != defaultTimeout {
		t.Fatalf("interval/timeout = %s/%s", cfg.interval, cfg.timeout)
	}

	cfg, _, err = configure(map[string]any{"url": "https://hatchet.example", "tenant": "explicit", "interval": "1s"}, requirements{token: true, tenant: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.tenant != "explicit" {
		t.Fatalf("tenant = %q, want explicit", cfg.tenant)
	}
	if cfg.interval != minInterval {
		t.Fatalf("interval = %s, want clamped to %s", cfg.interval, minInterval)
	}

	t.Setenv("HATCHET_CLIENT_TOKEN", "opaque")
	if _, _, err := configure(map[string]any{"url": "https://hatchet.example"}, requirements{token: true, tenant: true}); err == nil {
		t.Fatal("expected an error for an opaque token without tenant")
	} else if !strings.Contains(err.Error(), "tenant is not set") {
		t.Fatalf("error = %v", err)
	}

	t.Setenv("HATCHET_CLIENT_TOKEN", "")
	if _, _, err := configure(map[string]any{"url": "https://hatchet.example"}, requirements{token: true}); err == nil {
		t.Fatal("expected an error for a missing token")
	}
	if _, _, err := configure(map[string]any{"url": "https://hatchet.example"}, requirements{}); err != nil {
		t.Fatalf("health probe must not need a token: %v", err)
	}
}

func TestValidate(t *testing.T) {
	probes := []probe.Probe{&QueueProbe{}, &WorkersProbe{}, &WorkflowProbe{}, &HealthProbe{}}
	for _, p := range probes {
		if err := p.Validate(map[string]any{}); err == nil {
			t.Errorf("%s: missing url accepted", p.Kind())
		}
		if err := p.Validate(map[string]any{"url": "not a url", "workflow": "x"}); err == nil {
			t.Errorf("%s: bad url accepted", p.Kind())
		}
		if err := p.Validate(map[string]any{"url": "https://h.example", "workflow": "x", "interval": "soon"}); err == nil {
			t.Errorf("%s: bad interval accepted", p.Kind())
		}
		if err := p.Validate(map[string]any{"url": "https://h.example", "workflow": "x", "interval": "30s", "timeout": "5s"}); err != nil {
			t.Errorf("%s: valid spec rejected: %v", p.Kind(), err)
		}
	}
	if err := (&WorkersProbe{}).Validate(map[string]any{"url": "https://h.example", "long_task": "10 minutes"}); err == nil {
		t.Error("workers: bad long_task accepted")
	}
	if err := (&WorkflowProbe{}).Validate(map[string]any{"url": "https://h.example"}); err == nil {
		t.Error("workflow: missing workflow accepted")
	}
	if err := (&QueueProbe{}).Validate(map[string]any{"url": "https://h.example", "queue": "a", "workflow": "b"}); err == nil {
		t.Error("queue: both filters accepted")
	}
}

func TestRegistered(t *testing.T) {
	for _, k := range []string{KindQueue, KindWorkers, KindWorkflow, KindHealth} {
		if !probe.Known(k) {
			t.Errorf("%s not registered", k)
		}
		a, _ := probe.AccessFor(k)
		if !a.Implemented || a.Needs == "" || len(a.SpecFields) < 5 {
			t.Errorf("%s: access incomplete: %+v", k, a)
		}
	}
}

func TestStartLoopReemitsAndStops(t *testing.T) {
	srv := serve(t, routes{"/api/ready": jsonOK("ok"), "/api/live": jsonOK("ok")})
	spec := specFor(t, srv, map[string]any{"interval": "1h"})
	p := &HealthProbe{}
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan probe.Observation)
	if err := p.Start(ctx, spec, out); err != nil {
		t.Fatal(err)
	}
	var got []probe.Observation
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case o := <-out:
			got = append(got, o)
		case <-deadline:
			t.Fatalf("got %d observations, want 3", len(got))
		}
	}
	cancel()
	for _, o := range got {
		if o.Target != "hatchet" || o.Probe != KindHealth || o.Metrics["ready"] != 1 {
			t.Fatalf("observation = %+v", o)
		}
	}
	if !got[2].At.After(got[0].At) {
		t.Fatal("re-emitted observation did not get a fresh At")
	}
	// After cancel the goroutine stops: a blocked send would leak otherwise.
	select {
	case <-out:
	case <-time.After(200 * time.Millisecond):
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}
}

func TestListAllFollowsPages(t *testing.T) {
	var offsets []string
	srv := serve(t, routes{
		tenantPath("/workflows"): func(w http.ResponseWriter, r *http.Request) {
			offsets = append(offsets, r.URL.Query().Get("offset"))
			switch r.URL.Query().Get("offset") {
			case "0":
				rawJSON(`{"rows":[{"name":"a","metadata":{"id":"1"}}],"pagination":{"current_page":1,"next_page":2,"num_pages":2}}`)(w, r)
			default:
				rawJSON(`{"rows":[{"name":"b","metadata":{"id":"2"}}],"pagination":{"current_page":2,"num_pages":2}}`)(w, r)
			}
		},
	})
	_, c, err := configure(specFor(t, srv, nil), requirements{token: true, tenant: true})
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.workflowID(context.Background(), "b")
	if err != nil || id != "2" {
		t.Fatalf("id = %q, err = %v", id, err)
	}
	if len(offsets) != 2 || offsets[1] != "1" {
		t.Fatalf("offsets = %v", offsets)
	}
	// Cached: no further request.
	if _, err := c.workflowID(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if len(offsets) != 2 {
		t.Fatalf("cache miss: %d requests", len(offsets))
	}
	if _, err := c.workflowID(context.Background(), "zzz"); err == nil {
		t.Fatal("unknown workflow resolved")
	}
}

func TestPercentile(t *testing.T) {
	vs := make([]float64, 0, 100)
	for i := 1; i <= 100; i++ {
		vs = append(vs, float64(i))
	}
	if got := percentile(vs, 95); got != 95 {
		t.Fatalf("p95 = %v", got)
	}
	if got := percentile([]float64{5, 1, 3}, 50); got != 3 {
		t.Fatalf("p50 = %v", got)
	}
}
