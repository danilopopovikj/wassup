package netprobe

import (
	"context"
	"net"
	"testing"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// fakeResolver answers lookups from maps.
type fakeResolver struct {
	hosts  map[string][]string
	cnames map[string]string
	err    error
}

func (f fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	a, ok := f.hosts[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return a, nil
}

func (f fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if c, ok := f.cnames[host]; ok {
		return c, nil
	}
	return host + ".", nil
}

func TestDNSResolvesAndMatches(t *testing.T) {
	d := &DNS{r: fakeResolver{
		hosts:  map[string][]string{"bookstore.example": {"10.0.0.2", "10.0.0.1"}, "lb.example.net": {"10.0.0.1"}},
		cnames: map[string]string{"bookstore.example": "lb.example.net."},
	}}
	o := d.check(context.Background(), "dns", "bookstore.example", "lb.example.net")
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if o.Metrics["resolves"] != 1 || o.Metrics["addresses"] != 2 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if o.Detail["cname"] != "lb.example.net" || o.Detail["matched"] != true {
		t.Fatalf("detail = %v", o.Detail)
	}
	addrs, _ := o.Detail["addresses"].([]string)
	if len(addrs) != 2 || addrs[0] != "10.0.0.1" {
		t.Fatalf("addresses = %v", addrs)
	}

	// Expect as an IP present in the record.
	o = d.check(context.Background(), "dns", "bookstore.example", "10.0.0.2")
	if o.Metrics["resolves"] != 1 || o.Detail["matched"] != true {
		t.Fatalf("ip expect: %v %v", o.Metrics, o.Detail)
	}
	// Expect as an IP that is not in the record: resolves but does not match.
	o = d.check(context.Background(), "dns", "bookstore.example", "192.168.1.1")
	if o.Metrics["resolves"] != 0 || o.Metrics["addresses"] != 2 || o.Detail["matched"] != false {
		t.Fatalf("mismatch: %v %v", o.Metrics, o.Detail)
	}
}

func TestDNSNXDomain(t *testing.T) {
	d := &DNS{r: fakeResolver{hosts: map[string][]string{}}}
	o := d.check(context.Background(), "dns", "missing.example", "")
	if o.Err != "" {
		t.Fatalf("NXDOMAIN must not be a probe error: %q", o.Err)
	}
	if o.Metrics["resolves"] != 0 || o.Metrics["addresses"] != 0 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if r, _ := o.Detail["reason"].(string); r == "" {
		t.Fatalf("detail = %v", o.Detail)
	}
	if len(o.Conditions) != 0 {
		t.Fatalf("no condition expected, got %v", o.Conditions)
	}
	if d.Health().State != probe.HealthOK {
		t.Fatalf("health = %+v", d.Health())
	}
}

func TestDNSLookupError(t *testing.T) {
	d := &DNS{r: fakeResolver{err: &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true}}}
	o := d.check(context.Background(), "dns", "bookstore.example", "")
	if o.Err == "" {
		t.Fatal("expected Err")
	}
	if o.Metrics != nil {
		t.Fatalf("no metrics expected on error, got %v", o.Metrics)
	}
	if d.Health().State != probe.HealthDegraded {
		t.Fatalf("health = %+v", d.Health())
	}
}
