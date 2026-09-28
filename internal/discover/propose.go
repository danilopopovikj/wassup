package discover

import (
	"fmt"
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
}

// ProposeOptions tune the proposal.
type ProposeOptions struct {
	Name       string // system name; default: repo dir name
	Namespace  string // default namespace for k8s probes when a candidate has none
	Kubeconfig string
	Context    string
}

// Propose turns findings into a topology and bindings.
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
	nodeIDs := []string{}
	for _, c := range f.Candidates {
		if c.Type == "" || c.Type == "custom" && c.Image == "" && len(c.Env) == 0 {
			continue // a Service with no workload behind it in the repo
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
		if typ == "node" {
			nodeIDs = append(nodeIDs, c.ID)
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
		if specs := bindingsFor(c, opts); len(specs) > 0 {
			b.Components[c.ID] = specs
		}
		if c.Roles != nil && c.Extra["cnpg"] == "true" {
			ns := c.Namespace
			b.Components[c.Roles.Primary] = []model.ProbeSpec{{"probe": "cnpg.instance", "namespace": ns, "cluster": c.Name, "role": "primary"}}
			for i, r := range c.Roles.Replicas {
				b.Components[r] = []model.ProbeSpec{{"probe": "cnpg.instance", "namespace": ns, "cluster": c.Name, "instance": fmt.Sprintf("%s-%d", c.Name, i+2)}}
			}
		}
	}
	// Workloads run on every node when nothing says otherwise.
	if len(nodeIDs) > 0 {
		sort.Strings(nodeIDs)
		for i := range t.Components {
			if t.Components[i].Type == "workload" || t.Components[i].Type == "ingress" || t.Components[i].Type == "syncengine" {
				t.Components[i].RunsOn = append([]string(nil), nodeIDs...)
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
	p.Notes = append(p.Notes, f.Notes...)
	return p
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

// bindingsFor proposes probes for a candidate from what was seen.
func bindingsFor(c Candidate, opts ProposeOptions) []model.ProbeSpec {
	ns := c.Namespace
	if ns == "" {
		ns = opts.Namespace
	}
	var out []model.ProbeSpec
	k8s := func(kind string, extra map[string]any) model.ProbeSpec {
		s := model.ProbeSpec{"probe": kind, "namespace": ns}
		for k, v := range extra {
			s[k] = v
		}
		return s
	}
	svc := firstAddress(c.Addresses)
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
			out = append(out, model.ProbeSpec{"probe": "hatchet.health", "url": "http://" + orDefault(svc, "hatchet-api."+ns) + ":8080", "token_env": "HATCHET_CLIENT_TOKEN"})
		case c.Extra["hatchet_worker"] == "true":
			out = append(out, model.ProbeSpec{"probe": "hatchet.workers", "url": "http://hatchet-api." + ns + ":8080", "token_env": "HATCHET_CLIENT_TOKEN"})
		case c.Extra["celery_worker"] == "true":
			out = append(out, model.ProbeSpec{"probe": "celery.worker", "flower_url": "http://flower." + ns + ":5555"})
		}
	case "scheduledjob":
		if wf := c.Extra["hatchet_workflow"]; wf != "" {
			out = append(out, model.ProbeSpec{"probe": "hatchet.workflow", "url": "http://hatchet-api." + orDefault(ns, "default") + ":8080", "token_env": "HATCHET_CLIENT_TOKEN", "workflow": wf})
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
		spec := model.ProbeSpec{"probe": "redis.info", "addr": orDefault(svc, c.ID) + ":6379"}
		if c.Env["REDIS_PASSWORD"] != "" || strings.Contains(strings.ToLower(c.Image), "redis") {
			spec["password_env"] = "REDIS_PASSWORD"
		}
		out = append(out, spec)
	case "queue":
		switch {
		case c.Extra["celery"] == "true":
			out = append(out, model.ProbeSpec{"probe": "celery.queue", "broker": "redis://redis." + orDefault(ns, "default") + ":6379/0", "queue": c.Extra["queue"], "flower_url": "http://flower." + orDefault(ns, "default") + ":5555"})
		case strings.Contains(strings.ToLower(c.Image), "rabbitmq"):
			out = append(out, model.ProbeSpec{"probe": "amqp.queue", "management_url": "http://" + orDefault(svc, c.ID) + ":15672", "queue": "celery", "password_env": "RABBITMQ_PASSWORD"})
		default:
			out = append(out, model.ProbeSpec{"probe": "hatchet.queue", "url": "http://hatchet-api." + orDefault(ns, "default") + ":8080", "token_env": "HATCHET_CLIENT_TOKEN"})
		}
	case "syncengine":
		spec := model.ProbeSpec{"probe": "electric.sync", "url": "http://" + orDefault(svc, "electric."+orDefault(ns, "default")) + ":3000", "secret_env": "ELECTRIC_SECRET"}
		if tables := strings.Fields(c.Extra["tables"]); len(tables) > 0 {
			spec["table"] = tables[0]
		}
		out = append(out, spec)
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
		if c.Name != "" {
			out = append(out, k8s("k8s.ingress", map[string]any{"name": c.Name}))
		}
	case "dns":
		if len(c.Addresses) > 0 {
			out = append(out, model.ProbeSpec{"probe": "dns.record", "host": c.Addresses[0]})
		}
	case "storage":
		if c.Kind == "PersistentVolumeClaim" {
			out = append(out, k8s("k8s.pvc", map[string]any{"name": c.Name}))
		}
	case "observability":
		lower := strings.ToLower(c.Image + " " + c.Name)
		if strings.Contains(lower, "signoz") {
			out = append(out, model.ProbeSpec{"probe": "signoz.health", "url": "http://" + orDefault(svc, "signoz."+ns) + ":8080"})
		} else if svc != "" {
			out = append(out, model.ProbeSpec{"probe": "http.ping", "url": "http://" + svc + "/-/healthy"})
		}
	case "external":
		if len(c.Addresses) > 0 {
			out = append(out, model.ProbeSpec{"probe": "http.ping", "url": "https://" + c.Addresses[0]})
		}
	}
	return out
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
