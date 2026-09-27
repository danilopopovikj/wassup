package netprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// KindDNS is the probe kind of the DNS record check.
const KindDNS = "dns.record"

func init() {
	probe.Register(probe.Access{
		Kind:        KindDNS,
		Source:      "the system resolver",
		Delivers:    "resolves (1/0), addresses count, the CNAME and whether the record matches the expected target",
		SpecFields:  []string{"host", "expect", "interval"},
		Needs:       "DNS resolution from the machine running wassup; no credentials",
		Implemented: true,
	}, func() probe.Probe { return &DNS{} })
}

// resolver is the subset of net.Resolver the probe uses, for tests.
type resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// DNS is the dns.record probe. It resolves host and reports whether it
// resolves at all and, when expect is set, whether it resolves to the
// expected IP or to the same addresses as the expected hostname. A
// non-existent name (NXDOMAIN) is data (resolves 0), not a probe error; any
// other lookup failure is reported through Err and degraded health.
type DNS struct {
	h probe.Health

	r resolver // overridable for tests
}

// Kind implements probe.Probe.
func (d *DNS) Kind() string { return KindDNS }

// Validate implements probe.Probe.
func (d *DNS) Validate(spec map[string]any) error {
	return probe.RequireString(spec, "host")
}

// Health implements probe.Probe.
func (d *DNS) Health() probe.ProbeHealth { return d.h.Get() }

// Start implements probe.Probe.
func (d *DNS) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := d.Validate(spec); err != nil {
		d.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	if d.r == nil {
		d.r = net.DefaultResolver
	}
	target := targetOf(spec)
	host := strings.TrimSuffix(probe.Str(spec, "host", ""), ".")
	expect := strings.TrimSuffix(probe.Str(spec, "expect", ""), ".")
	tick := tickOf(spec)
	interval := probe.Dur(spec, "interval", 30*time.Second)
	d.h.Set(probe.HealthOK, "resolving "+host)
	go loop(ctx, tick, interval, out, func(ctx context.Context) probe.Observation {
		return d.check(ctx, target, host, expect)
	})
	return nil
}

// check performs one lookup round.
func (d *DNS) check(ctx context.Context, target, host, expect string) probe.Observation {
	o := probe.Observation{
		Target: target,
		Probe:  KindDNS,
		At:     time.Now(),
		Detail: map[string]any{"host": host},
	}
	if expect != "" {
		o.Detail["expect"] = expect
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	addrs, err := d.r.LookupHost(lctx, host)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			o.Metrics = map[string]float64{"resolves": 0, "addresses": 0}
			o.Detail["reason"] = "NXDOMAIN: " + dnsErr.Error()
			o.Detail["matched"] = false
			d.h.Set(probe.HealthOK, host+" does not exist")
			return o
		}
		o.Err = fmt.Sprintf("lookup %s: %v", host, err)
		d.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	sort.Strings(addrs)
	o.Detail["addresses"] = addrs

	// A CNAME lookup that fails is fine: many records have none, and the
	// resolver returns the name itself when it is an A/AAAA record.
	cname := ""
	if c, err := d.r.LookupCNAME(lctx, host); err == nil {
		c = strings.TrimSuffix(c, ".")
		if c != "" && !strings.EqualFold(c, host) {
			cname = c
		}
	}
	if cname != "" {
		o.Detail["cname"] = cname
	}

	matched := true
	if expect != "" {
		matched = d.matches(lctx, expect, cname, addrs)
		o.Detail["matched"] = matched
	}
	resolves := 0.0
	if len(addrs) > 0 && matched {
		resolves = 1
	}
	o.Metrics = map[string]float64{
		"resolves":  resolves,
		"addresses": float64(len(addrs)),
	}
	d.h.Set(probe.HealthOK, fmt.Sprintf("%s resolves to %d address(es)", host, len(addrs)))
	return o
}

// matches reports whether the record points at expect: an IP that is among
// the resolved addresses, the CNAME target itself, or a hostname that
// resolves to at least one address in common.
func (d *DNS) matches(ctx context.Context, expect, cname string, addrs []string) bool {
	if ip := net.ParseIP(expect); ip != nil {
		for _, a := range addrs {
			if got := net.ParseIP(a); got != nil && got.Equal(ip) {
				return true
			}
		}
		return false
	}
	if cname != "" && strings.EqualFold(cname, expect) {
		return true
	}
	want, err := d.r.LookupHost(ctx, expect)
	if err != nil {
		return false
	}
	have := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		have[a] = true
	}
	for _, w := range want {
		if have[w] {
			return true
		}
	}
	return false
}
