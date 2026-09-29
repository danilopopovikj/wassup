// Package measure finds a counter for every edge of the diagram that has
// none. It reads what the live sources hold (the services a router counts,
// the calls SigNoz counted) and what the cluster says routes where, and for
// each edge without a binding says one of three things: the binding that
// counts it, with the evidence that it does; why nothing can, and what
// would change that; or that it needs nothing. It also lists what the
// sources saw that no box on the diagram stands for.
//
// Plan is a pure function over the configuration and the sources, so every
// decision it takes is tested offline; cmd/wassup gathers the sources.
package measure

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
	"github.com/danilopopovikj/wassup/internal/probe/signoz"
)

// The metrics a router binding reads. They are Traefik's, the ingress
// controller of k3s and of most small clusters.
const (
	RouterServiceMetric = "traefik_service_requests_total"
	RouterRouterMetric  = "traefik_router_requests_total"
)

// Router is what the ingress controller counts: a k8s.scrape binding that
// reads its pods, and the values of the labels its counters carry.
type Router struct {
	// Spec is a k8s.scrape binding without metric and match: namespace,
	// selector, port.
	Spec model.ProbeSpec
	// Services holds the values of "service" on RouterServiceMetric.
	Services map[string]bool
	// Routers holds the values of "router" on RouterRouterMetric; empty
	// when the router does not label its counters by router.
	Routers map[string]bool
}

// Sources is what the live systems said. A source that is nil was not
// there, and Notes say why.
type Sources struct {
	Inventory *k8s.Inventory
	Router    *Router
	Survey    *signoz.Survey
	// SigNoz is the url and credentials of a signoz.edge binding.
	SigNoz model.ProbeSpec
	Notes  []string
}

// Status says what measure found for an edge.
type Status string

const (
	// Counted is an edge that has a binding already; measure leaves it.
	Counted Status = "counted"
	// Found is an edge measure has a binding for, with evidence.
	Found Status = "found"
	// Missing is an edge nothing can count yet; Reason says why.
	Missing Status = "missing"
	// Skipped is an edge whose kind carries no rate worth a label.
	Skipped Status = "skipped"
)

// Edge is the verdict on one edge.
type Edge struct {
	ID       string          `json:"id"`
	Status   Status          `json:"status"`
	Binding  model.ProbeSpec `json:"binding,omitempty"`
	Evidence string          `json:"evidence,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Fix      string          `json:"fix,omitempty"`
}

// Unplaced is something a source counted that no box stands for.
type Unplaced struct {
	From    string    `json:"from"`              // the component that did it
	Service string    `json:"service"`           // as the source names it
	Address string    `json:"address,omitempty"` // what it called
	Count   float64   `json:"count"`
	Last    time.Time `json:"last"`
	Suggest string    `json:"suggest"`
}

// Report is what Plan found.
type Report struct {
	Edges    []Edge     `json:"edges"`
	Unplaced []Unplaced `json:"unplaced,omitempty"`
	// Unknown lists the services a source knows that no component is.
	Unknown []string `json:"unknown_services,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// Counts sums the report by status.
func (r Report) Counts() map[Status]int {
	out := map[Status]int{}
	for _, e := range r.Edges {
		out[e.Status]++
	}
	return out
}

// Plan decides for every edge of the topology.
func Plan(cfg *model.Config, src Sources) Report {
	p := planner{cfg: cfg, src: src, used: map[string]bool{}}
	p.index()
	var rep Report
	rep.Notes = append(rep.Notes, src.Notes...)
	for _, e := range cfg.Topology.Edges {
		rep.Edges = append(rep.Edges, p.edge(e))
	}
	rep.Unplaced, rep.Unknown = p.unplaced()
	return rep
}

// planner carries what Plan works out once.
type planner struct {
	cfg *model.Config
	src Sources
	// services maps a component id to the names a source may know it by.
	services map[string][]string
	// used marks the survey's calls some edge accounts for: "service|address".
	used map[string]bool
}

func (p *planner) index() {
	p.services = map[string][]string{}
	for _, c := range p.cfg.Topology.AllComponents() {
		p.services[c.ID] = p.namesOf(c)
	}
	// What the bindings there are count already is accounted for.
	for _, specs := range p.cfg.Bindings.Edges {
		for _, s := range specs {
			if s.Kind() != "signoz.edge" {
				continue
			}
			for _, call := range p.surveyCalls() {
				if matchesCall(s, call) {
					p.used[call.Service+"|"+call.Address] = true
				}
			}
		}
	}
}

// namesOf lists the names a component may carry in a trace: its id, the
// values of the selector of its workload binding (app=api is "api"), and
// the name of the workload that selector finds in the cluster.
func (p *planner) namesOf(c model.Component) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	add(c.ID)
	for _, s := range p.cfg.Bindings.Components[c.ID] {
		if s.Kind() != "k8s.workload" {
			continue
		}
		sel := s.String("selector")
		for _, pair := range strings.Split(sel, ",") {
			if _, v, ok := strings.Cut(pair, "="); ok {
				add(v)
			}
		}
		if inv := p.src.Inventory; inv != nil {
			for _, w := range inv.Workloads {
				if w.Namespace == s.String("namespace") && selects(sel, w.Labels) {
					add(w.Name)
				}
			}
		}
	}
	return out
}

// selects reports whether a selector ("a=b,c=d") picks labels.
func selects(sel string, labels map[string]string) bool {
	if sel == "" {
		return false
	}
	for _, pair := range strings.Split(sel, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || labels[k] != v {
			return false
		}
	}
	return true
}

func (p *planner) surveyCalls() []signoz.Call {
	if p.src.Survey == nil {
		return nil
	}
	return p.src.Survey.External
}

// edge decides for one edge.
func (p *planner) edge(e model.Edge) Edge {
	out := Edge{ID: e.ID()}
	if len(p.cfg.Bindings.Edges[e.ID()]) > 0 {
		out.Status = Counted
		return out
	}
	from, _ := p.cfg.Topology.Component(e.From)
	to, _ := p.cfg.Topology.Component(e.To)
	switch {
	case e.Kind == "replication":
		out.Status, out.Reason = Missing, "a replication edge is read by pg.stats on the primary, with replica: the slot or the instance name"
		return out
	case e.Kind == "tcp":
		out.Status, out.Reason = Skipped, "a connection edge carries no rate worth a label"
		return out
	case from.Type == "scheduledjob":
		out.Status = Skipped
		out.Reason = "it takes the rate of " + from.DisplayLabel() + "'s runs, which the job's own binding reads"
		return out
	case from.Type == "dns":
		return p.byHost(out, from)
	case from.Type == "ingress":
		return p.byRouter(out, e, to)
	case to.Type == "external":
		return p.byCalls(out, e, from, to)
	case e.Kind == "sql":
		return p.byQueries(out, e, from)
	case e.Kind == "http" || e.Kind == "grpc":
		return p.byCalls(out, e, from, to)
	}
	out.Status = Missing
	out.Reason = fmt.Sprintf("no source wassup reads counts a %s edge; a queue or cache edge takes the rate of the broker or cache it names", e.Kind)
	return out
}

// byHost finds the counter of the requests that come in on one host: the
// router's counter of the routers of that host, or of the services only
// that host routes to.
func (p *planner) byHost(out Edge, dns model.Component) Edge {
	host := p.hostOf(dns)
	if host == "" {
		out.Status, out.Reason = Missing, "the name's dns.record binding says no host"
		return out
	}
	r := p.src.Router
	if r == nil {
		out.Status, out.Reason = Missing, "no router counter was read"
		out.Fix = routerFix
		return out
	}
	if len(r.Routers) > 0 {
		slug := strings.ReplaceAll(host, ".", "-")
		var names []string
		for name := range r.Routers {
			if strings.Contains(name, "-"+slug) {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			out.Status = Found
			out.Binding = p.routerBinding(RouterRouterMetric, "router", `.*-`+regexp.QuoteMeta(slug)+`.*`)
			out.Evidence = fmt.Sprintf("the router counts %d %s of %s: %s", len(names), plural(len(names), "route", "routes"), host, strings.Join(names, ", "))
			return out
		}
	}
	inv := p.src.Inventory
	if inv == nil {
		out.Status, out.Reason = Missing, "the cluster was not read, so what the host routes to is not known"
		return out
	}
	mine, shared := map[string]bool{}, map[string][]string{}
	for _, ing := range inv.Ingresses {
		for _, svc := range routerServices(ing) {
			if contains(ing.Hosts, host) {
				mine[svc] = true
			}
		}
	}
	for _, ing := range inv.Ingresses {
		if contains(ing.Hosts, host) {
			continue
		}
		for _, svc := range routerServices(ing) {
			if mine[svc] {
				shared[svc] = append(shared[svc], ing.Hosts...)
			}
		}
	}
	if len(mine) == 0 {
		out.Status, out.Reason = Missing, "no Ingress routes "+host+" (an IngressRoute is not read)"
		return out
	}
	if len(shared) > 0 {
		var parts []string
		for svc, hosts := range shared {
			parts = append(parts, strings.TrimSuffix(svc, "@kubernetes")+" with "+strings.Join(uniq(hosts), ", "))
		}
		sort.Strings(parts)
		out.Status = Missing
		out.Reason = host + " shares " + strings.Join(parts, "; ") + ": the router counts by service, not by host"
		out.Fix = routerLabelsFix
		return out
	}
	return p.byServices(out, keys(mine), "the services only "+host+" routes to")
}

// byRouter finds the counter of what the router sends to a component: the
// services that select its pods, as the router names them; else what the
// component itself says it served.
func (p *planner) byRouter(out Edge, e model.Edge, to model.Component) Edge {
	var svcs []string
	if inv := p.src.Inventory; inv != nil {
		for _, s := range p.cfg.Bindings.Components[to.ID] {
			if s.Kind() != "k8s.workload" {
				continue
			}
			for _, w := range inv.Workloads {
				if w.Namespace != s.String("namespace") || !selects(s.String("selector"), w.Labels) {
					continue
				}
				for _, svc := range inv.Services {
					if svc.Namespace == w.Namespace && selects(svc.Selector, w.Labels) {
						for _, ing := range inv.Ingresses {
							for _, name := range routerServices(ing) {
								if strings.HasPrefix(name, svc.Namespace+"-"+svc.Name+"-") {
									svcs = append(svcs, name)
								}
							}
						}
					}
				}
			}
		}
	}
	if len(svcs) > 0 && p.src.Router != nil {
		return p.byServices(out, uniq(svcs), "the services that select the pods of "+to.DisplayLabel())
	}
	if b, ev, ok := p.served(to); ok {
		out.Status, out.Binding, out.Evidence = Found, b, ev
		return out
	}
	out.Status = Missing
	switch {
	case p.src.Router == nil:
		out.Reason, out.Fix = "no router counter was read, and "+to.DisplayLabel()+" reports no requests it served to SigNoz", routerFix
	case len(svcs) == 0:
		out.Reason = "no Ingress routes to a Service that selects the pods of " + to.DisplayLabel()
	}
	return out
}

// byServices binds an edge to the router's counter of a set of services,
// when the router counts every one of them.
func (p *planner) byServices(out Edge, names []string, what string) Edge {
	r := p.src.Router
	if r == nil {
		out.Status, out.Reason, out.Fix = Missing, "no router counter was read", routerFix
		return out
	}
	var have, lack []string
	for _, n := range names {
		if r.Services[n] {
			have = append(have, n)
		} else {
			lack = append(lack, n)
		}
	}
	sort.Strings(have)
	if len(have) == 0 {
		out.Status = Missing
		out.Reason = "the router counts none of " + what + " (" + strings.Join(names, ", ") + "): it has served none of them since it started, or names them otherwise"
		return out
	}
	quoted := make([]string, len(have))
	for i, n := range have {
		quoted[i] = regexp.QuoteMeta(n)
	}
	out.Status = Found
	out.Binding = p.routerBinding(RouterServiceMetric, "service", strings.Join(quoted, "|"))
	out.Evidence = "the router counts " + what + ": " + strings.Join(have, ", ")
	if len(lack) > 0 {
		out.Evidence += "; not yet counted: " + strings.Join(lack, ", ")
	}
	return out
}

// routerBinding is a k8s.scrape binding on the router's counter.
func (p *planner) routerBinding(metric, label, expr string) model.ProbeSpec {
	b := model.ProbeSpec{}
	for k, v := range p.src.Router.Spec {
		b[k] = v
	}
	b["probe"] = "k8s.scrape"
	b["metric"] = metric
	b["match"] = map[string]any{label: expr}
	b["errors"] = map[string]any{"code": "5.."}
	return b
}

// served binds an edge into a component to the requests SigNoz counted the
// component serve, when its service sends spans.
func (p *planner) served(to model.Component) (model.ProbeSpec, string, bool) {
	s := p.src.Survey
	if s == nil {
		return nil, "", false
	}
	for _, c := range s.Served {
		if contains(p.services[to.ID], c.Service) {
			b := p.signozBinding("http.server.duration.count", map[string]any{"service.name": regexp.QuoteMeta(c.Service)})
			b["errors"] = map[string]any{"http.status_code": "5.."}
			return b, fmt.Sprintf("SigNoz counted %s serve %s in %s", c.Service, countText(c.Count, "request"), days(s.Lookback)), true
		}
	}
	return nil, "", false
}

// byCalls finds the counter of the calls one component makes to another:
// SigNoz's count of the calls of the caller's service to the callee's
// addresses.
func (p *planner) byCalls(out Edge, e model.Edge, from, to model.Component) Edge {
	s := p.src.Survey
	if s == nil {
		out.Status, out.Reason, out.Fix = Missing, "SigNoz was not read", signozFix
		return out
	}
	svc, why := p.caller(from)
	if svc == "" {
		out.Status, out.Reason, out.Fix = Missing, why, spansFix(from)
		return out
	}
	hosts := p.addressesOf(to)
	if len(hosts) == 0 {
		out.Status = Missing
		out.Reason = "no address of " + to.DisplayLabel() + " is known: bind it with http.ping (url) or dns.record (host)"
		return out
	}
	expr := addressExpr(hosts)
	re := regexp.MustCompile("^(?:" + expr + ")$")
	var total float64
	var last time.Time
	var seen []string
	for _, c := range s.External {
		if c.Service != svc || !re.MatchString(c.Address) {
			continue
		}
		p.used[c.Service+"|"+c.Address] = true
		total += c.Count
		if c.Last.After(last) {
			last = c.Last
		}
		seen = append(seen, c.Address)
	}
	if total == 0 {
		out.Status = Missing
		out.Reason = fmt.Sprintf("SigNoz counted no call of %s to %s in %s", svc, strings.Join(hosts, " or "), days(s.Lookback))
		out.Fix = "bind it once it is called, or remove the edge if it is not"
		return out
	}
	out.Status = Found
	out.Binding = p.signozBinding("signoz_external_call_latency_count", map[string]any{"service.name": regexp.QuoteMeta(svc), "address": expr})
	out.Binding["errors"] = map[string]any{"http.status_code": "5.."}
	out.Evidence = fmt.Sprintf("SigNoz counted %s from %s to %s in %s, the last %s", countText(total, "call"), svc, strings.Join(uniq(seen), ", "), days(s.Lookback), last.UTC().Format("2006-01-02 15:04 UTC"))
	if total/s.Lookback.Hours() < 1 {
		out.Binding["window"] = "1h" // called now and then: an hour reads 3 req/h, not idle between calls
	}
	return out
}

// byQueries finds the counter of the queries a component sends to a
// database: SigNoz's count of its database calls, which says no database,
// so it counts one edge only when the component has one database edge.
func (p *planner) byQueries(out Edge, e model.Edge, from model.Component) Edge {
	s := p.src.Survey
	if s == nil {
		out.Status, out.Reason, out.Fix = Missing, "SigNoz was not read", signozFix
		return out
	}
	svc, why := p.caller(from)
	if svc == "" {
		out.Status, out.Reason, out.Fix = Missing, why, spansFix(from)
		return out
	}
	n := 0
	for _, o := range p.cfg.Topology.Outgoing(from.ID) {
		if o.Kind == "sql" {
			n++
		}
	}
	if n > 1 {
		out.Status = Missing
		out.Reason = fmt.Sprintf("%s has %d database edges and SigNoz counts its queries without saying which database", from.DisplayLabel(), n)
		return out
	}
	for _, c := range s.Queries {
		if c.Service == svc {
			out.Status = Found
			out.Binding = p.signozBinding("signoz_db_latency_count", map[string]any{"service.name": regexp.QuoteMeta(svc)})
			out.Evidence = fmt.Sprintf("SigNoz counted %s from %s in %s", countText(c.Count, "query"), svc, days(s.Lookback))
			return out
		}
	}
	out.Status = Missing
	out.Reason = fmt.Sprintf("SigNoz counted no query of %s in %s", svc, days(s.Lookback))
	return out
}

// caller is the service a component's calls are counted under, or why
// there is none.
func (p *planner) caller(c model.Component) (string, string) {
	s := p.src.Survey
	names := p.services[c.ID]
	for _, q := range s.Quiet {
		if contains(names, q) {
			return "", c.DisplayLabel() + " (" + q + ") sends metrics but no span of its calls, so where they go is not recorded"
		}
	}
	for _, sp := range s.Spans {
		if contains(names, sp.Service) {
			return sp.Service, ""
		}
	}
	return "", c.DisplayLabel() + " sends no traces to SigNoz (no service named " + strings.Join(names, ", ") + ")"
}

// signozBinding is a signoz.edge binding with the url and credentials of
// the one there is.
func (p *planner) signozBinding(metric string, match map[string]any) model.ProbeSpec {
	b := model.ProbeSpec{}
	for _, k := range []string{"url", "user_env", "password_env", "token_env"} {
		if v, ok := p.src.SigNoz[k]; ok {
			b[k] = v
		}
	}
	b["probe"] = "signoz.edge"
	b["metric"] = metric
	b["match"] = match
	return b
}

// hostOf is the host a name stands for.
func (p *planner) hostOf(c model.Component) string {
	for _, s := range p.cfg.Bindings.Components[c.ID] {
		if h := s.String("host"); h != "" {
			return h
		}
	}
	if strings.Contains(c.Label, ".") && !strings.Contains(c.Label, " ") {
		return c.Label
	}
	return ""
}

// addressesOf lists the hosts a component answers on, from its bindings:
// the url of a ping, the host of a name, the address of a certificate, and
// for a component in the cluster, the names of the Services that select it.
func (p *planner) addressesOf(c model.Component) []string {
	var out []string
	for _, s := range p.cfg.Bindings.Components[c.ID] {
		if u, err := url.Parse(s.String("url")); err == nil && u.Hostname() != "" {
			out = append(out, u.Hostname())
		}
		if h := s.String("host"); h != "" {
			out = append(out, h)
		}
		if a := s.String("addr"); a != "" {
			out = append(out, strings.Split(a, ":")[0])
		}
		if s.Kind() == "k8s.workload" && p.src.Inventory != nil {
			for _, w := range p.src.Inventory.Workloads {
				if w.Namespace != s.String("namespace") || !selects(s.String("selector"), w.Labels) {
					continue
				}
				for _, svc := range p.src.Inventory.Services {
					if svc.Namespace == w.Namespace && selects(svc.Selector, w.Labels) {
						out = append(out, svc.Name)
					}
				}
			}
		}
	}
	return uniq(out)
}

// addressExpr matches the addresses of a set of hosts: the host, any name
// under the same domain (quickvin.carfax.com is carfax.com's as much as
// servicesocket.carfax.com), and a Service's name, alone or qualified, with
// or without a port.
func addressExpr(hosts []string) string {
	var parts []string
	for _, h := range hosts {
		if !strings.Contains(h, ".") {
			parts = append(parts, regexp.QuoteMeta(h)+`(\..*)?`)
			continue
		}
		parts = append(parts, `(.*\.)?`+regexp.QuoteMeta(Domain(h)))
	}
	return "(" + strings.Join(uniq(parts), "|") + `)(:[0-9]+)?`
}

// Domain is the name under which a host was registered: the last two
// labels, or three under a country's second level (example.co.uk).
func Domain(host string) string {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	n := 2
	if len(labels) >= 3 && len(labels[len(labels)-1]) == 2 {
		switch labels[len(labels)-2] {
		case "co", "com", "org", "net", "gov", "ac", "edu":
			n = 3
		}
	}
	if len(labels) <= n {
		return strings.Join(labels, ".")
	}
	return strings.Join(labels[len(labels)-n:], ".")
}

// unplaced lists the calls the survey holds that no edge accounts for, and
// the services it knows that no component is.
func (p *planner) unplaced() ([]Unplaced, []string) {
	s := p.src.Survey
	if s == nil {
		return nil, nil
	}
	owner := map[string]string{}
	for id, names := range p.services {
		for _, n := range names {
			if _, ok := owner[n]; !ok {
				owner[n] = id
			}
		}
	}
	var out []Unplaced
	for _, c := range s.External {
		if p.used[c.Service+"|"+c.Address] || !strings.Contains(c.Address, ".") {
			continue // accounted for, or a name inside the cluster
		}
		from := owner[c.Service]
		if from == "" {
			continue // the service is listed as unknown instead
		}
		u := Unplaced{From: from, Service: c.Service, Address: c.Address, Count: c.Count, Last: c.Last}
		if box := p.externalFor(c.Address); box != "" {
			u.Suggest = fmt.Sprintf("add the edge %s -> %s", from, box)
		} else {
			u.Suggest = fmt.Sprintf("add an external component for %s and the edge from %s", strings.Split(c.Address, ":")[0], from)
		}
		out = append(out, u)
	}
	var unknown []string
	for _, c := range s.Spans {
		if owner[c.Service] == "" {
			unknown = append(unknown, c.Service)
		}
	}
	return out, uniq(unknown)
}

// externalFor is the external component whose addresses cover a host.
func (p *planner) externalFor(host string) string {
	for _, c := range p.cfg.Topology.AllComponents() {
		if c.Type != "external" {
			continue
		}
		for _, h := range p.addressesOf(c) {
			if strings.Contains(h, ".") && Domain(h) == Domain(host) {
				return c.ID
			}
		}
	}
	return ""
}

// matchesCall reports whether a signoz.edge binding on the external calls
// counts a call of the survey, the way the probe matches.
func matchesCall(s model.ProbeSpec, c signoz.Call) bool {
	if s.String("metric") != "signoz_external_call_latency_count" {
		return false
	}
	m, err := probe.Matchers(s.Plain(), "match")
	if err != nil || len(m) == 0 {
		return false
	}
	return probe.Matches(map[string]string{"service.name": c.Service, "address": c.Address}, m)
}

// routerServices names the Services an Ingress routes to the way Traefik
// names them: namespace, name and port, "@kubernetes".
func routerServices(ing k8s.IngressInfo) []string {
	var out []string
	for _, b := range ing.Backends {
		target, _, _ := strings.Cut(b, " ")
		name, port, ok := strings.Cut(target, ":")
		if !ok || name == "" || port == "" {
			continue
		}
		out = append(out, ing.Namespace+"-"+name+"-"+port+"@kubernetes")
	}
	return uniq(out)
}

const (
	routerFix       = "let the router count: Traefik's Prometheus metrics (metrics.prometheus in its Helm values, served on port 9100), then run wassup measure again"
	routerLabelsFix = "let Traefik count by route: metrics.prometheus.addRoutersLabels: true in its Helm values, then run wassup measure again"
	signozFix       = "name the SigNoz to read with --signoz, or bind one signoz.edge by hand, then run wassup measure again"
)

// spansFix says what would put a component's calls into SigNoz.
func spansFix(c model.Component) string {
	return "make " + c.DisplayLabel() + " send a span for every outgoing call (the OpenTelemetry HTTP client instrumentation), and keep spans that have no parent"
}

func countText(n float64, what string) string {
	if n == 1 {
		return "1 " + what
	}
	plur := what + "s"
	if strings.HasSuffix(what, "y") {
		plur = strings.TrimSuffix(what, "y") + "ies"
	}
	return fmt.Sprintf("%.0f %s", n, plur)
}

func days(d time.Duration) string {
	if d >= 48*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	return d.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func uniq(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range list {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
