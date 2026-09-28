package discover

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/danilopopovikj/wassup/internal/model"
)

// Proposal is a topology and bindings built from evidence, plus what the
// scanner could not place.
type Proposal struct {
	Topology   model.Topology        `json:"topology"`
	Bindings   model.Bindings        `json:"bindings"`
	Evidence   map[string][]Evidence `json:"evidence"` // per component or edge id
	Unresolved []Link                `json:"unresolved,omitempty"`
	Notes      []string              `json:"notes,omitempty"`
	// Confidence says, per component and edge id, how far it can be trusted
	// and whether the cluster or only the repository stands behind it. It
	// is not part of the topology: it describes the draft, not the system.
	Confidence map[string]Confidence `json:"confidence,omitempty"`
}

// ProposeOptions tune the proposal.
type ProposeOptions struct {
	Name       string // system name; default: repo dir name
	Namespace  string // default namespace for k8s probes when a candidate has none
	Kubeconfig string
	Context    string
}

// idRe is the pattern the topology schema sets for ids.
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Propose turns findings into a topology and bindings. Every id it emits
// matches the pattern of the topology schema, and every value in a binding
// comes from something that was read: a namespace, a name, a host. What
// could only be guessed is left out and said in the notes.
func Propose(f *Findings, opts ProposeOptions) *Proposal {
	p := &Proposal{Evidence: map[string][]Evidence{}}
	name := opts.Name
	if name == "" {
		parts := strings.Split(strings.TrimRight(f.Root, "/"), "/")
		name = model.SlugifyID(parts[len(parts)-1])
	}
	t := model.Topology{Version: model.Version, Name: name, Settings: model.Settings{AutoLens: true}}
	b := model.Bindings{Version: model.Version, Components: map[string][]model.ProbeSpec{}, Edges: map[string][]model.ProbeSpec{}}

	// Groups: one per cloud provider seen, a cluster, and the namespaces.
	groups := map[string]model.Group{}
	cloud := ""
	for _, prov := range f.Providers {
		if lbl, ok := providerLabel[prov]; ok && cloud == "" {
			cloud = prov
			groups[prov] = model.Group{ID: prov, Kind: "cloud", Label: lbl}
		}
	}
	clusterID := "cluster"
	groups[clusterID] = model.Group{ID: clusterID, Kind: "cluster", Label: "Kubernetes", Parent: cloud}
	nsGroup := func(ns string) string {
		if ns == "" {
			return clusterID
		}
		id := model.SlugifyID("ns-" + ns)
		if _, ok := groups[id]; !ok {
			groups[id] = model.Group{ID: id, Kind: "namespace", Label: ns, Parent: clusterID}
		}
		return id
	}

	// Components.
	ids := map[string]bool{}
	var nodes []Candidate
	for _, c := range f.Candidates {
		if c.Type == "node" {
			nodes = append(nodes, c)
		}
	}
	for _, c := range f.Candidates {
		if c.Type == "" || c.Type == "custom" && c.Image == "" && len(c.Env) == 0 {
			continue // a Service with no workload behind it in the repo
		}
		if !idRe.MatchString(c.ID) || ids[c.ID] {
			p.Notes = append(p.Notes, fmt.Sprintf("left out %q: not a valid or unique id", c.ID))
			continue
		}
		if c.Roles != nil {
			c.Roles = slugRoles(c.Roles)
		}
		typ := c.Type
		if typ == "custom" {
			typ = "workload"
		}
		if _, ok := model.Catalog[typ]; !ok {
			typ = "custom"
		}
		comp := model.Component{ID: c.ID, Type: typ, Label: c.Label, Engine: c.Engine, Roles: c.Roles}
		if comp.Label == "" {
			comp.Label = labelFor(c.ID)
		}
		switch {
		case c.Group != "":
			if _, ok := groups[c.Group]; ok {
				comp.Group = c.Group
			}
		case typ == "external":
			comp.Group = ""
		case c.Namespace != "":
			comp.Group = nsGroup(c.Namespace)
		case typ == "node":
			comp.Group = clusterID
		default:
			comp.Group = clusterID
		}
		if typ != "node" {
			comp.RunsOn = runsOn(c, nodes)
		}
		var notes []string
		for _, e := range c.Evidence {
			notes = append(notes, e.String())
		}
		comp.Notes = strings.Join(uniq(notes), "; ")
		if len(comp.Notes) > 300 {
			comp.Notes = comp.Notes[:297] + "..."
		}
		t.Components = append(t.Components, comp)
		ids[c.ID] = true
		p.Evidence[c.ID] = c.Evidence
		specs, unbound := bindingsFor(f, c, opts)
		if len(specs) > 0 {
			b.Components[c.ID] = specs
		}
		p.Notes = append(p.Notes, unbound...)
		if c.Roles != nil && c.Extra["cnpg"] == "true" {
			ns := c.Namespace
			b.Components[c.Roles.Primary] = []model.ProbeSpec{{"probe": "cnpg.instance", "namespace": ns, "cluster": c.Name, "role": "primary"}}
			for i, r := range c.Roles.Replicas {
				b.Components[r] = []model.ProbeSpec{{"probe": "cnpg.instance", "namespace": ns, "cluster": c.Name, "instance": fmt.Sprintf("%s-%d", c.Name, i+2)}}
			}
		}
	}
	for _, g := range sortedGroups(groups) {
		t.Groups = append(t.Groups, g)
	}

	// Edges.
	seenEdge := map[string]bool{}
	for _, l := range f.Links {
		if l.From == "repo" || l.From == "" || l.To == "" {
			if l.Host != "" && l.To == "" {
				p.Unresolved = append(p.Unresolved, l)
			}
			continue
		}
		if !ids[l.From] || !ids[l.To] || l.From == l.To {
			continue
		}
		from, _ := findComp(t.Components, l.From)
		to, _ := findComp(t.Components, l.To)
		kind := l.Kind
		if kind == "" || kind == "tcp" {
			kind = kindFor(from, to, l.Kind)
		} else if kind == "cache" && to.Type == "queue" {
			kind = "queue"
		} else if kind == "http" && to.Type == "external" {
			kind = "external"
		}
		// A db with roles anchors its replication edges on the instances.
		fromID, toID := l.From, l.To
		if kind == "replication" && from.Type == "database" && from.Roles != nil {
			fromID = from.Roles.Primary
		}
		id := model.EdgeID(fromID, toID)
		if seenEdge[id] {
			// The same flow seen again, by the cluster after the
			// repository: it is one edge, with the evidence of both.
			for _, ev := range l.Evidence {
				p.Evidence[id] = addEvidence(p.Evidence[id], ev)
			}
			continue
		}
		seenEdge[id] = true
		t.Edges = append(t.Edges, model.Edge{From: fromID, To: toID, Kind: kind, Label: l.Label})
		p.Evidence[id] = l.Evidence
		if kind == "replication" && to.Type == "syncengine" {
			slot := "electric_slot_default"
			if c := f.candidate(l.To); c != nil && c.Extra["replication_slot"] != "" {
				slot = c.Extra["replication_slot"]
			}
			b.Edges[id] = []model.ProbeSpec{{"probe": "pg.stats", "dsn_env": dsnEnvFor(from), "replica": slot}}
		}
	}
	// A sync engine without an explicit source database: link it to the
	// only Postgres in sight, since that is what it replicates from.
	for _, c := range t.Components {
		if c.Type != "syncengine" {
			continue
		}
		hasSource := false
		for _, e := range t.Edges {
			if e.To == c.ID && e.Kind == "replication" {
				hasSource = true
			}
		}
		if hasSource {
			continue
		}
		var dbs []model.Component
		for _, d := range t.Components {
			if d.Type == "database" && (d.Engine == "postgres" || d.Engine == "") {
				dbs = append(dbs, d)
			}
		}
		if len(dbs) == 1 {
			from := dbs[0].ID
			if dbs[0].Roles != nil {
				from = dbs[0].Roles.Primary
			}
			id := model.EdgeID(from, c.ID)
			t.Edges = append(t.Edges, model.Edge{From: from, To: c.ID, Kind: "replication", Label: "slot"})
			p.Evidence[id] = []Evidence{{Source: "inference", Note: "the only Postgres is the sync source; confirm ELECTRIC_DATABASE_URL"}}
			slot := "electric_slot_default"
			if cc := f.candidate(c.ID); cc != nil && cc.Extra["replication_slot"] != "" {
				slot = cc.Extra["replication_slot"]
			}
			b.Edges[id] = []model.ProbeSpec{{"probe": "pg.stats", "dsn_env": dsnEnvFor(dbs[0]), "replica": slot}}
			b.Components[c.ID] = append(b.Components[c.ID], model.ProbeSpec{"probe": "pg.stats", "dsn_env": dsnEnvFor(dbs[0]), "replica": slot})
		}
	}
	sort.SliceStable(t.Components, func(i, j int) bool { return t.Components[i].ID < t.Components[j].ID })
	sort.SliceStable(t.Edges, func(i, j int) bool { return t.Edges[i].ID() < t.Edges[j].ID() })
	p.Topology, p.Bindings = t, b
	p.Notes = uniqInOrder(append(p.Notes, f.Notes...))
	p.rate(f)
	return p
}

// uniqInOrder drops repeated lines and keeps the order.
func uniqInOrder(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// slugRoles makes the ids of a database's instances valid.
func slugRoles(r *model.Roles) *model.Roles {
	out := &model.Roles{Primary: model.SlugifyID(r.Primary)}
	for _, id := range r.Replicas {
		out.Replicas = append(out.Replicas, model.SlugifyID(id))
	}
	return out
}

// runsOn says on which nodes a component runs: where its pods were seen in
// the live cluster, or else the nodes its manifest selects (node selector,
// required node affinity) among those whose taints it tolerates. When
// neither says anything the answer is none: a list of every node would be a
// guess that looks like a fact.
func runsOn(c Candidate, nodes []Candidate) []string {
	known := map[string]bool{}
	for _, n := range nodes {
		known[n.ID] = true
	}
	var out []string
	if len(c.RunsOn) > 0 {
		for _, id := range c.RunsOn {
			if known[id] {
				out = append(out, id)
			}
		}
		return uniq(out)
	}
	sel := c.Extra["node_selector"]
	if sel == "" {
		return nil
	}
	for _, n := range nodes {
		if nodeMatches(n, sel) && tolerates(c.Extra["tolerations"], n.Extra["taints"]) {
			out = append(out, n.ID)
		}
	}
	return uniq(out)
}

// nodeMatches reports whether a node carries every label of a selector
// written "key=value,key=a|b". A node answers to its own name under the
// hostname label even when its labels are not known.
func nodeMatches(n Candidate, selector string) bool {
	for _, part := range strings.Split(selector, ",") {
		key, want, ok := strings.Cut(part, "=")
		if !ok {
			return false
		}
		have := n.Labels[key]
		if have == "" && key == hostnameLabel {
			have = n.Name
		}
		match := false
		for _, w := range strings.Split(want, "|") {
			if have == w || key == hostnameLabel && model.SlugifyID(w) == n.ID {
				match = true
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// tolerates reports whether pods with these tolerations can be scheduled
// on a node with these taints ("key=value:effect").
func tolerates(tolerations, taints string) bool {
	if taints == "" {
		return true
	}
	ok := map[string]bool{}
	for _, t := range strings.Split(tolerations, ",") {
		ok[t] = true
	}
	for _, taint := range strings.Split(taints, ",") {
		kv, effect, _ := strings.Cut(taint, ":")
		if effect == "PreferNoSchedule" {
			continue
		}
		key, _, _ := strings.Cut(kv, "=")
		if !ok["*"] && !ok[kv] && !ok[key] {
			return false
		}
	}
	return true
}

func sortedGroups(m map[string]model.Group) []model.Group {
	var out []model.Group
	for _, g := range m {
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		rank := map[string]int{"cloud": 0, "region": 1, "cluster": 2, "zone": 3, "namespace": 4}
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func findComp(cs []model.Component, id string) (model.Component, bool) {
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return model.Component{}, false
}

// kindFor picks an edge kind from the endpoint types when the scanner only
// knew there was a connection.
func kindFor(from, to model.Component, hint string) string {
	if from.Type == "dns" || from.Type == "firewall" {
		return "tcp"
	}
	switch to.Type {
	case "database":
		return "sql"
	case "cache":
		return "cache"
	case "queue":
		return "queue"
	case "external":
		return "external"
	case "syncengine":
		if from.Type == "database" {
			return "replication"
		}
		return "http"
	case "workload", "ingress", "loadbalancer":
		if from.Type == "loadbalancer" && to.Type == "node" {
			return "tcp"
		}
		if hint == "grpc" {
			return "grpc"
		}
		return "http"
	case "node":
		return "tcp"
	case "observability":
		return "tcp"
	}
	if hint != "" {
		return hint
	}
	return "tcp"
}

// dsnEnvFor names the env var a database probe should read its DSN from.
func dsnEnvFor(db model.Component) string {
	return strings.ToUpper(strings.ReplaceAll(db.ID, "-", "_")) + "_DSN"
}

// addressWhere returns the address of the first candidate match accepts:
// the name of its Service, or its own name in its namespace when it is a
// workload.
func (f *Findings) addressWhere(match func(Candidate) bool) string {
	for _, c := range f.Candidates {
		if !match(c) {
			continue
		}
		if a := ownAddress(c); a != "" {
			return a
		}
	}
	return ""
}

// hatchetURL is where the Hatchet API answers: the Service of the API (or
// of the all-in-one image), or else the conventional hatchet-api next to
// the engine, in the engine's namespace. It is "" when no Hatchet was seen.
func hatchetURL(f *Findings) string {
	is := func(words ...string) func(Candidate) bool {
		return func(c Candidate) bool {
			lower := strings.ToLower(c.Image + " " + c.Name)
			for _, w := range words {
				if strings.Contains(lower, w) {
					return true
				}
			}
			return false
		}
	}
	if a := f.addressWhere(is("hatchet-api", "hatchet-lite")); a != "" {
		return "http://" + a + ":8080"
	}
	for _, c := range f.Candidates {
		if is("hatchet-engine", "hatchet-api", "hatchet-lite")(c) && c.Namespace != "" {
			return "http://hatchet-api." + c.Namespace + ":8080"
		}
	}
	return ""
}

// flowerURL is where Flower answers: its Service when it was seen, or else
// the conventional flower in the namespace of the workers.
func flowerURL(f *Findings, ns string) string {
	a := f.addressWhere(func(c Candidate) bool {
		return c.Extra["flower"] == "true" || strings.Contains(strings.ToLower(c.Image), "flower")
	})
	switch {
	case a != "":
		return "http://" + a + ":5555"
	case ns != "":
		return "http://flower." + ns + ":5555"
	}
	return ""
}

// reachedAt returns the host (and the port, when it was given) through
// which another component reaches the one with this id, as read from that
// component's environment. The host of a replication flow is the source's,
// so those do not count.
func reachedAt(f *Findings, id string) (host, port string) {
	for _, l := range f.Links {
		if l.To == id && l.Host != "" && l.Kind != "replication" {
			return l.Host, l.Port
		}
	}
	return "", ""
}

// ownAddress picks the address to probe a component at: the Service named
// like the component itself when there is one (a product folded from
// several workloads answers to several), or else the first.
func ownAddress(c Candidate) string {
	if c.Name != "" && c.Namespace != "" {
		want := strings.ToLower(c.Name + "." + c.Namespace)
		for _, a := range c.Addresses {
			if a == want {
				return a
			}
		}
	}
	return firstAddress(c.Addresses)
}

// bindingsFor proposes probes for a candidate from what was seen. A probe
// whose target could only be guessed is not proposed; the second result
// says so, for the notes.
func bindingsFor(f *Findings, c Candidate, opts ProposeOptions) ([]model.ProbeSpec, []string) {
	ns := c.Namespace
	if ns == "" {
		ns = opts.Namespace
	}
	var out []model.ProbeSpec
	var notes []string
	unbound := func(probe, why string) {
		notes = append(notes, fmt.Sprintf("%s: %s not proposed, %s; bind it by hand", c.ID, probe, why))
	}
	k8s := func(kind string, extra map[string]any) model.ProbeSpec {
		s := model.ProbeSpec{"probe": kind, "namespace": ns}
		for k, v := range extra {
			s[k] = v
		}
		return s
	}
	svc := ownAddress(c)
	switch c.Type {
	case "workload", "backgroundworker":
		if c.Selector != "" {
			out = append(out, k8s("k8s.workload", map[string]any{"selector": c.Selector}))
		} else if c.Name != "" && c.Kind != "" {
			out = append(out, k8s("k8s.workload", map[string]any{"name": c.Name, "kind": c.Kind}))
		} else if c.Name != "" {
			out = append(out, k8s("k8s.workload", map[string]any{"selector": "app=" + c.Name}))
		}
		lower := strings.ToLower(c.Image + " " + c.Name)
		switch {
		case strings.Contains(lower, "hatchet-engine") || strings.Contains(lower, "hatchet-api") || strings.Contains(lower, "hatchet-lite"):
			if u := hatchetURL(f); u != "" {
				out = append(out, model.ProbeSpec{"probe": "hatchet.health", "url": u, "token_env": "HATCHET_CLIENT_TOKEN"})
			}
		case c.Extra["hatchet_worker"] == "true":
			if u := hatchetURL(f); u != "" {
				out = append(out, model.ProbeSpec{"probe": "hatchet.workers", "url": u, "token_env": "HATCHET_CLIENT_TOKEN"})
			} else {
				unbound("hatchet.workers", "no Hatchet API was found to ask")
			}
		case c.Extra["celery_worker"] == "true":
			if u := flowerURL(f, c.Namespace); u != "" {
				out = append(out, model.ProbeSpec{"probe": "celery.worker", "flower_url": u})
			} else {
				unbound("celery.worker", "no Flower was found to ask")
			}
		}
	case "scheduledjob":
		if wf := c.Extra["hatchet_workflow"]; wf != "" {
			if u := hatchetURL(f); u != "" {
				out = append(out, model.ProbeSpec{"probe": "hatchet.workflow", "url": u, "token_env": "HATCHET_CLIENT_TOKEN", "workflow": wf})
			} else {
				unbound("hatchet.workflow", "no Hatchet API was found to ask")
			}
		} else if c.Name != "" {
			out = append(out, k8s("k8s.cronjob", map[string]any{"name": c.Name}))
		}
	case "node":
		out = append(out, model.ProbeSpec{"probe": "k8s.node", "name": c.Name})
	case "database":
		if c.Extra["cnpg"] == "true" {
			out = append(out, k8s("cnpg.cluster", map[string]any{"cluster": c.Name}))
		}
		out = append(out, model.ProbeSpec{"probe": "pg.stats", "dsn_env": dsnEnvFor(model.Component{ID: c.ID})})
	case "cache":
		if svc == "" {
			unbound("redis.info", "its address is not known")
			break
		}
		spec := model.ProbeSpec{"probe": "redis.info", "addr": svc + ":6379"}
		if _, ok := c.Env["REDIS_PASSWORD"]; ok || strings.Contains(strings.ToLower(c.Image), "redis") {
			spec["password_env"] = "REDIS_PASSWORD"
		}
		out = append(out, spec)
	case "queue":
		switch {
		case c.Extra["celery"] == "true":
			broker := c.Extra["broker"]
			if broker == "" {
				if a := f.addressWhere(func(o Candidate) bool { return o.Type == "cache" }); a != "" {
					broker = "redis://" + a + ":6379/0"
				}
			}
			if broker == "" {
				unbound("celery.queue", "the broker is not known")
				break
			}
			spec := model.ProbeSpec{"probe": "celery.queue", "broker": broker, "queue": c.Extra["queue"]}
			if u := flowerURL(f, c.Namespace); u != "" {
				spec["flower_url"] = u
			}
			out = append(out, spec)
		case strings.Contains(strings.ToLower(c.Image), "rabbitmq"):
			if svc == "" {
				unbound("amqp.queue", "its address is not known")
				break
			}
			out = append(out, model.ProbeSpec{"probe": "amqp.queue", "management_url": "http://" + svc + ":15672", "queue": "celery", "password_env": "RABBITMQ_PASSWORD"})
		default:
			if u := hatchetURL(f); u != "" {
				out = append(out, model.ProbeSpec{"probe": "hatchet.queue", "url": u, "token_env": "HATCHET_CLIENT_TOKEN"})
			}
		}
	case "syncengine":
		// Never with "table": with one, the probe asks for a shape, and
		// Electric answers a new shape with a snapshot query on the
		// database. Whoever wants the handshake checked adds it by hand.
		host, port := svc, "3000"
		if host == "" {
			if h, p := reachedAt(f, c.ID); h != "" {
				host, port = h, orDefault(p, port)
			}
		}
		if host == "" {
			unbound("electric.sync", "its address is not known")
			break
		}
		out = append(out, model.ProbeSpec{"probe": "electric.sync", "url": "http://" + host + ":" + port, "secret_env": "ELECTRIC_SECRET"})
	case "loadbalancer":
		if strings.HasPrefix(c.Extra["terraform"], "hcloud_") {
			out = append(out, model.ProbeSpec{"probe": "hcloud.lb", "name": c.Name, "token_env": "HCLOUD_TOKEN"})
		}
	case "firewall":
		if strings.HasPrefix(c.Extra["terraform"], "hcloud_") {
			out = append(out, model.ProbeSpec{"probe": "hcloud.firewall", "name": c.Name, "token_env": "HCLOUD_TOKEN"})
		} else if tf := c.Extra["terraform"]; tf != "" {
			out = append(out, model.ProbeSpec{"probe": "terraform.state", "dir": ".", "resource": tf})
		}
	case "ingress":
		// One probe per Ingress object, each with its own namespace and
		// name: the component stands for all of them.
		for _, o := range c.Objects {
			kind, objNS, name := parseObjectRef(o)
			if kind != "Ingress" || name == "" {
				continue
			}
			out = append(out, model.ProbeSpec{"probe": "k8s.ingress", "namespace": orDefault(objNS, opts.Namespace), "name": name})
		}
	case "dns":
		if len(c.Addresses) > 0 {
			out = append(out, model.ProbeSpec{"probe": "dns.record", "host": c.Addresses[0]})
		}
	case "storage":
		switch {
		case c.Kind == "PersistentVolumeClaim":
			out = append(out, k8s("k8s.pvc", map[string]any{"name": c.Name}))
		case c.Extra["bucket"] != "":
			spec := model.ProbeSpec{"probe": "s3.bucket", "bucket": c.Extra["bucket"], "access_key_env": "AWS_ACCESS_KEY_ID", "secret_key_env": "AWS_SECRET_ACCESS_KEY"}
			if ep := c.Extra["endpoint"]; ep != "" {
				spec["endpoint"] = ep
			}
			if reg := c.Extra["region"]; reg != "" {
				spec["region"] = reg
			}
			out = append(out, spec)
		}
	case "observability":
		lower := strings.ToLower(c.Image + " " + c.Name)
		switch {
		case svc == "":
			if strings.Contains(lower, "signoz") {
				unbound("signoz.health", "its address is not known")
			}
		case strings.Contains(lower, "signoz"):
			out = append(out, model.ProbeSpec{"probe": "signoz.health", "url": "http://" + svc + ":8080"})
		default:
			out = append(out, model.ProbeSpec{"probe": "http.ping", "url": "http://" + svc + "/-/healthy"})
		}
	case "external":
		if len(c.Addresses) > 0 {
			out = append(out, model.ProbeSpec{"probe": "http.ping", "url": "https://" + c.Addresses[0]})
		}
	}
	return out, notes
}

func firstAddress(addrs []string) string {
	// prefer name.namespace
	for _, a := range addrs {
		if strings.Count(a, ".") == 1 {
			return a
		}
	}
	if len(addrs) > 0 {
		return addrs[0]
	}
	return ""
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
