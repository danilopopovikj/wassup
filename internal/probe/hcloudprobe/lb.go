package hcloudprobe

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// TargetHealth is the flattened health of one server target.
type TargetHealth struct {
	ServerID int64
	Healthy  bool
	// Status is a short text per service: "6443 healthy, 80 unhealthy".
	Status string
	// Via names the label selector that matched the server, if any.
	Via string
}

// flattenTargets lists every server target, descending into label selector
// targets. IP targets have no server id and are listed with ServerID 0.
func flattenTargets(targets []LoadBalancerTarget, via string) []TargetHealth {
	var out []TargetHealth
	for _, t := range targets {
		switch t.Type {
		case "label_selector":
			sel := ""
			if t.LabelSelector != nil {
				sel = t.LabelSelector.Selector
			}
			out = append(out, flattenTargets(t.Targets, sel)...)
		default:
			th := TargetHealth{Via: via}
			if t.Server != nil {
				th.ServerID = t.Server.ID
			}
			th.Healthy, th.Status = healthOf(t.HealthStatus)
			out = append(out, th)
		}
	}
	return out
}

// healthOf reports whether every service reports healthy, and a status text.
// A target without any status is unknown, hence not healthy.
func healthOf(hs []LoadBalancerTargetHealthStatus) (bool, string) {
	if len(hs) == 0 {
		return false, "no health status"
	}
	healthy := true
	parts := make([]string, 0, len(hs))
	sorted := append([]LoadBalancerTargetHealthStatus(nil), hs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ListenPort < sorted[j].ListenPort })
	for _, h := range sorted {
		if h.Status != "healthy" {
			healthy = false
		}
		parts = append(parts, fmt.Sprintf("%d %s", h.ListenPort, h.Status))
	}
	return healthy, strings.Join(parts, ", ")
}

// Summary is what one load balancer read boils down to.
type Summary struct {
	Healthy int
	Total   int
	Targets []TargetHealth
}

// Summarize counts healthy server targets across every service.
func Summarize(lb LoadBalancer) Summary {
	s := Summary{Targets: flattenTargets(lb.Targets, "")}
	for _, t := range s.Targets {
		s.Total++
		if t.Healthy {
			s.Healthy++
		}
	}
	return s
}

// LastValue returns the most recent value of a metric time series.
func LastValue(resp LoadBalancerMetricsResponse, series string) (float64, bool) {
	ts, ok := resp.Metrics.TimeSeries[series]
	if !ok || len(ts.Values) == 0 {
		return 0, false
	}
	// Walk back to the last parseable value.
	for i := len(ts.Values) - 1; i >= 0; i-- {
		switch v := ts.Values[i][1].(type) {
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f, true
			}
		case float64:
			return v, true
		}
	}
	return 0, false
}

// lbAccess documents hcloud.lb.
var lbAccess = probe.Access{
	Kind:   "hcloud.lb",
	Source: "the Hetzner Cloud API: load balancer targets' health and the load balancer metrics endpoint",
	Delivers: "connections, rate (requests per second), targets_healthy, targets_total, TargetUnhealthy when no target is healthy; " +
		"per entry of targets an observation for the edge <lb>-><component> with healthy (1/0) and HealthCheckFailing; " +
		"detail: services, targets, algorithm, location",
	SpecFields:  []string{"name", "id", "token_env", "targets", "interval", "endpoint"},
	Needs:       "a read-only Cloud API token in the environment variable named by token_env (default HCLOUD_TOKEN)",
	Implemented: true,
	Facets:      []string{facet.NameLoadBalancer},
}

func init() {
	probe.Register(lbAccess, func() probe.Probe { return &LBProbe{} })
}

// LBProbe is hcloud.lb. Spec: name or id (required), token_env, targets
// (component id -> server name or id), interval (default 30s).
type LBProbe struct {
	h probe.Health
}

// Kind implements probe.Probe.
func (p *LBProbe) Kind() string { return lbAccess.Kind }

// Validate implements probe.Probe.
func (p *LBProbe) Validate(spec map[string]any) error {
	if err := validateCommon(spec); err != nil {
		return err
	}
	if v, ok := spec["targets"]; ok {
		if _, err := targetsFromSpec(v); err != nil {
			return err
		}
	}
	return nil
}

// targetsFromSpec reads the component id -> server ref map (yaml gives
// map[string]any, code may give map[string]string).
func targetsFromSpec(v any) (map[string]resourceRef, error) {
	out := map[string]resourceRef{}
	add := func(k string, val any) error {
		var r resourceRef
		switch x := val.(type) {
		case string:
			if id, err := strconv.ParseInt(x, 10, 64); err == nil && id > 0 {
				r.ID = id
			} else {
				r.Name = x
			}
		case int:
			r.ID = int64(x)
		case int64:
			r.ID = x
		case float64:
			r.ID = int64(x)
		default:
			return fmt.Errorf("targets[%q] must be a server name or id, got %T", k, val)
		}
		if r.ID == 0 && r.Name == "" {
			return fmt.Errorf("targets[%q] is empty", k)
		}
		out[k] = r
		return nil
	}
	switch m := v.(type) {
	case map[string]any:
		for k, val := range m {
			if err := add(k, val); err != nil {
				return nil, err
			}
		}
	case map[string]string:
		for k, val := range m {
			if err := add(k, val); err != nil {
				return nil, err
			}
		}
	case nil:
	default:
		return nil, fmt.Errorf("targets must be a map of component id to server, got %T", v)
	}
	return out, nil
}

// Health implements probe.Probe.
func (p *LBProbe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *LBProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
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
	targets, err := targetsFromSpec(spec["targets"])
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	tgt := target(spec)
	every := tick(spec)
	interval := probe.Dur(spec, "interval", 30*time.Second)
	seen := since{}
	poll := func(ctx context.Context) []probe.Observation {
		return p.poll(ctx, c, &ref, targets, tgt, seen)
	}
	go run(ctx, out, every, interval, poll)
	return nil
}

// poll reads the load balancer once and builds the observations.
func (p *LBProbe) poll(ctx context.Context, c *client, ref *resourceRef, targets map[string]resourceRef, tgt string, seen since) []probe.Observation {
	now := time.Now()
	fail := func(err error) []probe.Observation {
		msg := err.Error()
		if misconfigured(err) {
			p.h.Set(probe.HealthFailed, msg)
		} else {
			p.h.Set(probe.HealthDegraded, msg)
		}
		obs := []probe.Observation{{Target: tgt, Probe: p.Kind(), At: now, Err: msg}}
		for comp := range targets {
			obs = append(obs, probe.Observation{Target: model.EdgeID(tgt, comp), Probe: p.Kind(), At: now, Err: msg})
		}
		return obs
	}
	if err := c.lookup(ctx, "load_balancers", ref); err != nil {
		return fail(err)
	}
	var resp struct {
		LoadBalancer LoadBalancer `json:"load_balancer"`
	}
	if err := c.get(ctx, "/load_balancers/"+strconv.FormatInt(ref.ID, 10), nil, &resp); err != nil {
		return fail(err)
	}
	lb := resp.LoadBalancer
	// Resolve the configured server names once; ids stay cached in targets.
	var unresolved []string
	for comp, r := range targets {
		if r.ID != 0 {
			continue
		}
		if err := c.lookup(ctx, "servers", &r); err != nil {
			unresolved = append(unresolved, comp+": "+err.Error())
			continue
		}
		targets[comp] = r
	}

	sum := Summarize(lb)
	o := probe.Observation{Target: tgt, Probe: p.Kind(), At: now}
	// The counts come from every target of the balancer, named or not; the
	// named ones below only drive the per-target edges.
	lbf := facet.LoadBalancerFacet{TargetsHealthy: facet.NI(sum.Healthy), TargetsTotal: facet.NI(sum.Total)}
	health, msg := probe.HealthOK, ""
	var metrics LoadBalancerMetricsResponse
	q := url.Values{
		"type":  {"open_connections,requests_per_second"},
		"start": {now.Add(-time.Minute).UTC().Format(time.RFC3339)},
		"end":   {now.UTC().Format(time.RFC3339)},
		"step":  {"60"},
	}
	if err := c.get(ctx, "/load_balancers/"+strconv.FormatInt(ref.ID, 10)+"/metrics", q, &metrics); err != nil {
		health, msg = probe.HealthDegraded, "metrics: "+err.Error()
	} else {
		if v, ok := LastValue(metrics, "open_connections"); ok {
			lbf.Connections = facet.N(v)
		}
		if v, ok := LastValue(metrics, "requests_per_second"); ok {
			lbf.Rate = facet.N(v)
		}
	}
	if len(unresolved) > 0 {
		health, msg = probe.HealthDegraded, strings.Join(unresolved, "; ")
	}

	services := make([]map[string]any, 0, len(lb.Services))
	for _, s := range lb.Services {
		services = append(services, map[string]any{
			"protocol": s.Protocol, "listen_port": s.ListenPort, "destination_port": s.DestinationPort,
			"health_check": fmt.Sprintf("%s:%d", s.HealthCheck.Protocol, s.HealthCheck.Port),
		})
	}
	byServer := map[int64]TargetHealth{}
	tlist := make([]map[string]any, 0, len(sum.Targets))
	for _, t := range sum.Targets {
		byServer[t.ServerID] = t
		entry := map[string]any{"server_id": t.ServerID, "healthy": t.Healthy, "status": t.Status}
		if t.Via != "" {
			entry["label_selector"] = t.Via
		}
		tlist = append(tlist, entry)
	}
	o.Detail = map[string]any{
		"name": lb.Name, "algorithm": lb.Algorithm.Type, "location": lb.Location.Name,
		"services": services, "targets": tlist,
	}

	// Named targets: one facet.Target per component the spec maps, with the
	// first-seen time of an unhealthy check kept stable between polls.
	live := map[string]bool{}
	comps := make([]string, 0, len(targets))
	for comp := range targets {
		comps = append(comps, comp)
	}
	sort.Strings(comps)
	var unresolvedObs []probe.Observation
	named := make([]facet.Target, 0, len(comps))
	edgeDetail := map[string]map[string]any{}
	for _, comp := range comps {
		r := targets[comp]
		if r.ID == 0 {
			unresolvedObs = append(unresolvedObs, probe.Observation{
				Target: model.EdgeID(tgt, comp), Probe: p.Kind(), At: now, Err: "server " + r.Name + " not found",
			})
			continue
		}
		th, ok := byServer[r.ID]
		if !ok {
			th = TargetHealth{ServerID: r.ID, Status: "not a target of " + lb.Name}
		}
		edgeDetail[model.EdgeID(tgt, comp)] = map[string]any{"server_id": r.ID, "status": th.Status}
		t := facet.Target{ID: comp, Healthy: th.Healthy, Detail: th.Status}
		if !th.Healthy {
			live[comp] = true
			t.Since = seen.mark(comp, now)
		}
		named = append(named, t)
	}
	lbf.Targets = named
	seen.keep(live)

	// The facets write the canonical form: the balancer's metrics and
	// TargetUnhealthy, then one edge observation per named target with
	// healthy 1/0 and HealthCheckFailing.
	facet.EmitLoadBalancer(&o, lbf, now)
	obs := []probe.Observation{o}
	for _, eo := range facet.LoadBalancerEdges(tgt, p.Kind(), named, now) {
		eo.Detail = edgeDetail[eo.Target]
		obs = append(obs, eo)
	}
	obs = append(obs, unresolvedObs...)
	p.h.Set(health, msg)
	return obs
}
