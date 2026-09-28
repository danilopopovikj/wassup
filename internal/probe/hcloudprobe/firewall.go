package hcloudprobe

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// PortInRange reports whether port falls in a rule port spec: "4317",
// "1-65535" or "" / "any" (every port).
func PortInRange(spec string, port int) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "any" {
		return true
	}
	if lo, hi, ok := strings.Cut(spec, "-"); ok {
		l, err1 := strconv.Atoi(strings.TrimSpace(lo))
		h, err2 := strconv.Atoi(strings.TrimSpace(hi))
		return err1 == nil && err2 == nil && port >= l && port <= h
	}
	p, err := strconv.Atoi(spec)
	return err == nil && p == port
}

// RuleAllows reports whether an inbound rule lets protocol traffic to port
// through from at least one source. Hetzner firewalls are allow-lists, so
// any matching rule allows.
func RuleAllows(r FirewallRule, port int, protocol string) bool {
	if r.Direction != "in" || !strings.EqualFold(r.Protocol, protocol) {
		return false
	}
	if len(r.SourceIPs) == 0 {
		return false
	}
	spec := ""
	if r.Port != nil {
		spec = *r.Port
	}
	return PortInRange(spec, port)
}

// Allowed reports whether any inbound rule allows protocol/port. When none
// does it also returns the description of the closest rule (same protocol,
// inbound) to name in the condition, or "" when there is none.
func Allowed(rules []FirewallRule, port int, protocol string) (bool, string) {
	closest := ""
	for _, r := range rules {
		if RuleAllows(r, port, protocol) {
			return true, ruleName(r)
		}
		if closest == "" && r.Direction == "in" && strings.EqualFold(r.Protocol, protocol) {
			closest = ruleName(r)
		}
	}
	return false, closest
}

// ruleName is the rule description or a synthesized "in tcp 80 from ...".
func ruleName(r FirewallRule) string {
	if r.Description != nil && *r.Description != "" {
		return *r.Description
	}
	s := r.Direction + " " + r.Protocol
	if r.Port != nil && *r.Port != "" {
		s += " " + *r.Port
	}
	ips := r.SourceIPs
	if r.Direction == "out" {
		ips = r.DestinationIPs
	}
	if len(ips) > 0 {
		s += " " + strings.Join(ips, ",")
	}
	return s
}

// appliedTo renders the firewall's resources: server ids and selectors.
func appliedTo(res []FirewallResource) []string {
	var out []string
	for _, r := range res {
		switch r.Type {
		case "server":
			if r.Server != nil {
				out = append(out, "server/"+strconv.FormatInt(r.Server.ID, 10))
			}
		case "label_selector":
			if r.LabelSelector != nil {
				out = append(out, "selector/"+r.LabelSelector.Selector)
			}
			for _, sub := range r.AppliedToResources {
				if sub.Server != nil {
					out = append(out, "server/"+strconv.FormatInt(sub.Server.ID, 10))
				}
			}
		}
	}
	return out
}

// firewallAccess documents hcloud.firewall.
var firewallAccess = probe.Access{
	Kind:   "hcloud.firewall",
	Source: "the Hetzner Cloud API firewall resource",
	Delivers: "rules (count); on an edge with port set: allowed (1/0) and FirewallDenied when no inbound rule lets the port through; " +
		"detail: rules (direction, protocol, port, source ips, description), applied_to. No events: terraform.state owns change markers",
	SpecFields:  []string{"name", "id", "token_env", "port", "protocol", "interval", "endpoint"},
	Needs:       "a read-only Cloud API token in the environment variable named by token_env (default HCLOUD_TOKEN)",
	Implemented: true,
	Tier:        probe.TierToken,
	Facets:      []string{facet.NameFirewall, facet.NameTraffic},
}

func init() {
	probe.Register(firewallAccess, func() probe.Probe { return &FirewallProbe{} })
}

// FirewallProbe is hcloud.firewall. Spec: name or id (required), token_env,
// port (edge bindings), protocol (default tcp), interval (default 60s).
type FirewallProbe struct {
	h probe.Health
}

// Kind implements probe.Probe.
func (p *FirewallProbe) Kind() string { return firewallAccess.Kind }

// Validate implements probe.Probe.
func (p *FirewallProbe) Validate(spec map[string]any) error {
	if err := validateCommon(spec); err != nil {
		return err
	}
	if v, ok := spec["port"]; ok {
		n, isNum := probe.Num(spec, "port")
		if !isNum || n < 1 || n > 65535 {
			return fmt.Errorf("port must be an integer between 1 and 65535, got %v", v)
		}
	}
	switch proto := strings.ToLower(probe.Str(spec, "protocol", "tcp")); proto {
	case "tcp", "udp", "icmp", "esp", "gre":
	default:
		return fmt.Errorf("protocol must be tcp, udp, icmp, esp or gre, got %q", proto)
	}
	return nil
}

// Health implements probe.Probe.
func (p *FirewallProbe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *FirewallProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	c, err := newClient(spec)
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	ref, err := refFromSpec(spec)
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	port := 0
	if n, ok := probe.Num(spec, "port"); ok {
		port = int(n)
	}
	protocol := strings.ToLower(probe.Str(spec, "protocol", "tcp"))
	tgt := target(spec)
	every := tick(spec)
	interval := probe.Dur(spec, "interval", time.Minute)
	seen := since{}
	poll := func(ctx context.Context) []probe.Observation {
		return []probe.Observation{p.poll(ctx, c, &ref, port, protocol, tgt, seen)}
	}
	go run(ctx, out, every, interval, poll)
	return nil
}

// poll reads the firewall once and builds the observation.
func (p *FirewallProbe) poll(ctx context.Context, c *client, ref *resourceRef, port int, protocol, tgt string, seen since) probe.Observation {
	o := probe.Observation{Target: tgt, Probe: p.Kind(), At: time.Now()}
	fail := func(err error) probe.Observation {
		o.Err = err.Error()
		if misconfigured(err) {
			p.h.Set(probe.HealthFailed, o.Err)
		} else {
			p.h.Set(probe.HealthDegraded, o.Err)
		}
		return o
	}
	if err := c.lookup(ctx, "firewalls", ref); err != nil {
		return fail(err)
	}
	var resp struct {
		Firewall Firewall `json:"firewall"`
	}
	if err := c.get(ctx, "/firewalls/"+strconv.FormatInt(ref.ID, 10), nil, &resp); err != nil {
		return fail(err)
	}
	fw := resp.Firewall
	f := facet.FirewallFacet{Rules: facet.NI(len(fw.Rules))}
	rules := make([]map[string]any, 0, len(fw.Rules))
	for _, r := range fw.Rules {
		entry := map[string]any{"direction": r.Direction, "protocol": r.Protocol}
		if r.Port != nil {
			entry["port"] = *r.Port
		}
		if r.Direction == "out" {
			entry["destination_ips"] = r.DestinationIPs
		} else {
			entry["source_ips"] = r.SourceIPs
		}
		if r.Description != nil && *r.Description != "" {
			entry["description"] = *r.Description
		}
		rules = append(rules, entry)
	}
	o.Detail = map[string]any{"name": fw.Name, "rules": rules, "applied_to": appliedTo(fw.AppliedTo)}
	if port > 0 {
		ok, closest := Allowed(fw.Rules, port, protocol)
		key := fmt.Sprintf("%s/%d", protocol, port)
		o.Detail["port"] = key
		f.Allowed = &ok
		if ok {
			o.Detail["allowed_by"] = closest
			seen.keep(nil)
		} else {
			// The denying rule set is the closest inbound rule of the same
			// protocol, or the firewall itself when it has none.
			f.DeniedDetail = closest
			if f.DeniedDetail == "" {
				f.DeniedDetail = fw.Name
			}
			f.DeniedSince = seen.mark(key, o.At)
		}
	}
	// The facet writes the canonical form: rules, allowed 1/0 and
	// FirewallDenied naming the rule.
	facet.EmitFirewall(&o, f, o.At)
	p.h.Set(probe.HealthOK, "")
	return o
}
