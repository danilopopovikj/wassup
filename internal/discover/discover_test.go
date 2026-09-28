package discover

import (
	"strings"
	"testing"

	"github.com/danilopopovikj/wassup/internal/model"
)

func scanFixture(t *testing.T) (*Findings, *Proposal) {
	t.Helper()
	f, err := Scan(Options{Root: "testdata/repo"})
	if err != nil {
		t.Fatal(err)
	}
	p := Propose(f, ProposeOptions{Name: "bookstore"})
	return f, p
}

func hasComp(p *Proposal, id, typ string) bool {
	c, ok := findComp(p.Topology.Components, id)
	return ok && (typ == "" || c.Type == typ)
}

func hasEdge(p *Proposal, from, to, kind string) bool {
	for _, e := range p.Topology.Edges {
		if e.From == from && e.To == to && (kind == "" || e.Kind == kind) {
			return true
		}
	}
	return false
}

func TestScanFindsTheStack(t *testing.T) {
	f, p := scanFixture(t)
	want := map[string]string{
		"node-1": "node", "node-2": "node", "bookstore-lb": "loadbalancer", "bookstore": "firewall",
		"api": "workload", "worker": "backgroundworker", "hatchet-worker": "backgroundworker", "hatchet-engine": "workload",
		"redis": "cache", "electric": "syncengine", "bookstore-db": "database", "hatchet-db": "database",
		"exports-queue": "queue", "default-queue": "queue", "billing": "scheduledjob", "ingress": "ingress",
		"app-bookstore-example": "dns", "stripe": "external", "bookstore-media-bucket": "storage",
	}
	for id, typ := range want {
		if !hasComp(p, id, typ) {
			c, _ := findComp(p.Topology.Components, id)
			t.Errorf("missing %s %s (got type %q)", typ, id, c.Type)
		}
	}
	db, _ := findComp(p.Topology.Components, "bookstore-db")
	if db.Roles == nil || db.Roles.Primary != "bookstore-db-primary" || len(db.Roles.Replicas) != 2 {
		t.Errorf("CNPG cluster should get one primary and two replicas: %+v", db.Roles)
	}
	edges := [][3]string{
		{"api", "bookstore-db", "sql"},                      // PGHOST → CNPG rw service
		{"api", "redis", "cache"},                           // REDIS_URL
		{"worker", "bookstore-db", "sql"},                   // literal DSN
		{"exports-queue", "worker", "queue"},                // celery -Q exports
		{"default-queue", "worker", "queue"},                // celery -Q default
		{"api", "exports-queue", "queue"},                   // @app.task(queue="exports") under apps/api
		{"api", "electric", "http"},                         // ELECTRIC_URL
		{"bookstore-db-primary", "electric", "replication"}, // helm values DATABASE_URL on the sync service
		{"hatchet-engine", "hatchet-db", "sql"},             // engine DSN
		{"ingress", "api", "http"},                          // Ingress backend
		{"ingress", "electric", "http"},                     // Ingress backend
		{"app-bookstore-example", "ingress", "tcp"},         // host
		{"bookstore-lb", "node-1", "tcp"},                   // terraform lb target
		{"api", "stripe", "external"},                       // code URL
		{"api", "bookstore-media-bucket", "tcp"},            // S3_BUCKET in the env, Bucket= in code
	}
	for _, e := range edges {
		if !hasEdge(p, e[0], e[1], e[2]) {
			t.Errorf("missing edge %s -> %s (%s)", e[0], e[1], e[2])
		}
	}
	// Bindings follow the evidence.
	if specs := p.Bindings.Components["api"]; len(specs) == 0 || specs[0].Kind() != "k8s.workload" || specs[0].String("selector") != "app=api" {
		t.Errorf("api binding %v", specs)
	}
	if specs := p.Bindings.Components["electric"]; len(specs) == 0 || specs[0].Kind() != "electric.sync" || specs[0].String("table") != "issues" {
		t.Errorf("electric binding %v", specs)
	}
	if specs := p.Bindings.Components["bookstore-media-bucket"]; len(specs) == 0 || specs[0].Kind() != "s3.bucket" ||
		specs[0].String("bucket") != "bookstore-media" || specs[0].String("endpoint") != "https://fsn1.your-objectstorage.com" {
		t.Errorf("bucket binding %v", specs)
	}
	if specs := p.Bindings.Edges["bookstore-db-primary->electric"]; len(specs) == 0 || specs[0].Kind() != "pg.stats" || specs[0].String("replica") != "electric_slot_default" {
		t.Errorf("replication edge binding %v", specs)
	}
	if specs := p.Bindings.Components["billing"]; len(specs) == 0 || specs[0].Kind() != "hatchet.workflow" {
		t.Errorf("billing binding %v", specs)
	}
	if specs := p.Bindings.Components["bookstore-db"]; len(specs) == 0 || specs[0].Kind() != "cnpg.cluster" {
		t.Errorf("db binding %v", specs)
	}
	if specs := p.Bindings.Components["exports-queue"]; len(specs) == 0 || specs[0].Kind() != "celery.queue" {
		t.Errorf("queue binding %v", specs)
	}
	if len(p.Bindings.Components["worker"]) < 2 || p.Bindings.Components["worker"][1].Kind() != "celery.worker" {
		t.Errorf("celery worker binding %v", p.Bindings.Components["worker"])
	}
	// Evidence cites files.
	ev := p.Evidence["electric"]
	joined := ""
	for _, e := range ev {
		joined += e.String() + "\n"
	}
	if !strings.Contains(joined, "infra/main.tf") || !strings.Contains(joined, "charts/electric/values.yaml") {
		t.Errorf("electric evidence should cite terraform and helm values:\n%s", joined)
	}
	if len(f.Providers) == 0 || f.Providers[0] != "hcloud" {
		t.Errorf("providers %v", f.Providers)
	}
	// The proposal validates as a real topology.
	cfg := &model.Config{Topology: p.Topology, Bindings: p.Bindings, Layout: model.Layout{Components: map[string]model.Placement{}}}
	if issues := cfg.CrossCheck(); len(issues) > 0 {
		t.Errorf("proposal does not cross-check: %v", issues)
	}
}

func TestDiffAndApply(t *testing.T) {
	_, p := scanFixture(t)
	// A committed topology that predates Electric and Hatchet.
	cur := &model.Config{Layout: model.Layout{Components: map[string]model.Placement{}}}
	cur.Topology = model.Topology{Version: 1, Name: "bookstore", Components: []model.Component{
		{ID: "api", Type: "workload", Label: "API"},
		{ID: "worker", Type: "workload", Label: "Workers"},
		{ID: "redis", Type: "cache", Label: "Redis"},
		{ID: "db", Type: "database", Label: "Bookstore db", Roles: &model.Roles{Primary: "db-primary", Replicas: []string{"db-r1", "db-r2"}}},
		{ID: "legacy-ftp", Type: "external", Label: "FTP drop"},
	}, Edges: []model.Edge{{From: "api", To: "db", Kind: "sql"}, {From: "api", To: "redis", Kind: "cache"}}}
	cur.Bindings = model.Bindings{Version: 1, Components: map[string][]model.ProbeSpec{"api": {{"probe": "k8s.workload", "namespace": "bookstore", "selector": "app=api"}}}}
	d := Diff(cur, p)
	ops := map[string]bool{}
	for _, ch := range d.Changes {
		ops[ch.Op+" "+ch.What+" "+ch.ID] = true
	}
	for _, want := range []string{"added component electric", "added component hatchet-engine", "added edge api->electric", "removed component legacy-ftp", "added binding worker", "added binding redis"} {
		if !ops[want] {
			t.Errorf("diff lacks %q; have %v", want, ops)
		}
	}
	if ops["added component bookstore-db"] {
		t.Error("the CNPG cluster should match the existing db by label")
	}
	if ops["added component api"] {
		t.Error("api exists and must not be re-added")
	}
	added, addedEdges := Apply(cur, p, d, false)
	if added == 0 || addedEdges == 0 {
		t.Fatalf("apply added %d components, %d edges", added, addedEdges)
	}
	if _, ok := cur.Topology.Component("legacy-ftp"); !ok {
		t.Error("apply without prune must keep components the scan did not see")
	}
	if _, ok := cur.Topology.Component("electric"); !ok {
		t.Error("electric not added")
	}
	if lbl, _ := cur.Topology.Component("worker"); lbl.Label != "Workers" {
		t.Error("existing labels must not change")
	}
	found := false
	for _, e := range cur.Topology.Edges {
		if e.From == "db-primary" && e.To == "electric" && e.Kind == "replication" {
			found = true
		}
	}
	if !found {
		t.Errorf("replication edge should be remapped onto the existing db's primary: %v", cur.Topology.Edges)
	}
	if len(cur.Bindings.Components["worker"]) == 0 {
		t.Error("unbound existing component should receive the proposed binding")
	}
	if issues := cur.CrossCheck(); len(issues) > 0 {
		t.Errorf("applied config does not cross-check: %v", issues)
	}
	d2 := Diff(cur, p)
	for _, ch := range d2.Changes {
		if ch.Op == "added" {
			t.Errorf("second sync should add nothing, got %+v", ch)
		}
	}
	Apply(cur, p, d2, true)
	if _, ok := cur.Topology.Component("legacy-ftp"); ok {
		t.Error("prune should drop components the scan no longer finds")
	}
}
