package netprobe

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// tlsServer starts a TLS httptest server and returns its host:port, the
// leaf certificate and a pool trusting it.
func tlsServer(t *testing.T) (string, *x509.Certificate, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	leaf := srv.Certificate()
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return strings.TrimPrefix(srv.URL, "https://"), leaf, pool
}

func TestCertHealthy(t *testing.T) {
	addr, leaf, pool := tlsServer(t)
	now := leaf.NotAfter.AddDate(0, -6, 0)
	c := &Cert{now: func() time.Time { return now }, roots: pool}

	o := c.check(context.Background(), "ingress", addr, "example.com")
	if o.Err != "" {
		t.Fatalf("err = %q", o.Err)
	}
	if o.Target != "ingress" || o.Probe != KindCert {
		t.Fatalf("target/probe = %q/%q", o.Target, o.Probe)
	}
	wantDays := float64(int(leaf.NotAfter.Sub(now).Hours() / 24))
	if o.Metrics["cert_days"] != wantDays {
		t.Fatalf("cert_days = %v, want %v", o.Metrics["cert_days"], wantDays)
	}
	if len(o.Conditions) != 0 {
		t.Fatalf("unexpected conditions %v", o.Conditions)
	}
	if o.Detail["not_after"] != leaf.NotAfter.UTC().Format(time.RFC3339) {
		t.Fatalf("not_after = %v", o.Detail["not_after"])
	}
	if o.Detail["verified"] != true {
		t.Fatalf("verified = %v (%v)", o.Detail["verified"], o.Detail["verify_error"])
	}
	sans, _ := o.Detail["sans"].([]string)
	if len(sans) == 0 || sans[0] != "example.com" {
		t.Fatalf("sans = %v", o.Detail["sans"])
	}
	for _, k := range []string{"subject", "issuer"} {
		if s, _ := o.Detail[k].(string); s == "" {
			t.Fatalf("detail %s is empty", k)
		}
	}
	if c.Health().State != probe.HealthOK {
		t.Fatalf("health = %+v", c.Health())
	}
}

func TestCertExpiring(t *testing.T) {
	addr, leaf, pool := tlsServer(t)
	now := leaf.NotAfter.Add(-3*24*time.Hour - time.Hour)
	c := &Cert{now: func() time.Time { return now }, roots: pool}

	o := c.check(context.Background(), "ingress", addr, "example.com")
	if o.Metrics["cert_days"] != 3 {
		t.Fatalf("cert_days = %v, want 3", o.Metrics["cert_days"])
	}
	cond, ok := model.HasCondition(o.Conditions, model.CondCertExpiring)
	if !ok {
		t.Fatalf("no CertExpiring: %v", o.Conditions)
	}
	// The certificate facet names the bound component, not the server name.
	if cond.Ref != "certificate/ingress" || !strings.Contains(cond.Detail, "3 days") {
		t.Fatalf("condition = %+v", cond)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondCertExpired); ok {
		t.Fatal("CertExpired raised on a live certificate")
	}
}

func TestCertExpired(t *testing.T) {
	addr, leaf, pool := tlsServer(t)
	now := leaf.NotAfter.Add(36 * time.Hour)
	c := &Cert{now: func() time.Time { return now }, roots: pool}

	o := c.check(context.Background(), "ingress", addr, "example.com")
	if o.Err != "" {
		t.Fatalf("handshake against an expired cert must still succeed: %s", o.Err)
	}
	if o.Metrics["cert_days"] != 0 {
		t.Fatalf("cert_days = %v, want clamped 0", o.Metrics["cert_days"])
	}
	cond, ok := model.HasCondition(o.Conditions, model.CondCertExpired)
	if !ok {
		t.Fatalf("no CertExpired: %v", o.Conditions)
	}
	if !cond.Since.Equal(leaf.NotAfter) {
		t.Fatalf("since = %v, want NotAfter %v", cond.Since, leaf.NotAfter)
	}
	if o.Detail["not_after"] != leaf.NotAfter.UTC().Format(time.RFC3339) {
		t.Fatalf("not_after kept in detail: %v", o.Detail["not_after"])
	}
	if o.Detail["verified"] != false {
		t.Fatal("expired certificate reported as verified")
	}
}

func TestCertDialError(t *testing.T) {
	c := &Cert{}
	// Port 1 on loopback is closed.
	o := c.check(context.Background(), "ingress", "127.0.0.1:1", "example.com")
	if o.Err == "" {
		t.Fatal("expected Err")
	}
	if c.Health().State != probe.HealthDegraded {
		t.Fatalf("health = %+v", c.Health())
	}
}

func TestCertValidate(t *testing.T) {
	c := &Cert{}
	for _, spec := range []map[string]any{{}, {"addr": "example.com"}, {"addr": ":443"}} {
		if err := c.Validate(spec); err == nil {
			t.Errorf("%v: expected error", spec)
		}
	}
	if err := c.Validate(map[string]any{"addr": "example.com:443"}); err != nil {
		t.Fatal(err)
	}
}

func TestCertStartStops(t *testing.T) {
	addr, _, pool := tlsServer(t)
	c := &Cert{roots: pool}
	out := make(chan probe.Observation, 8)
	ctx, cancel := context.WithCancel(context.Background())
	err := c.Start(ctx, map[string]any{"addr": addr, "servername": "example.com", "_target": "ingress", "_tick": 20 * time.Millisecond}, out)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-out:
		if o.Target != "ingress" || o.Metrics["cert_days"] <= 0 {
			t.Fatalf("bad observation %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no observation")
	}
	// The interval is 5m but the tick is 20ms: re-emits must arrive.
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("no re-emit on tick")
	}
	cancel()
}
