package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodTopology = `version: 1
name: t
groups:
  - id: cloud
    kind: cloud
components:
  - id: lb
    type: lb
    group: cloud
  - id: api
    type: workload
    runs_on: [n1]
  - id: n1
    type: node
  - id: db
    type: db
    roles: { primary: db-p, replicas: [db-r] }
edges:
  - { from: lb, to: api, kind: http }
  - { from: api, to: db, kind: sql }
  - { from: db-p, to: db-r, kind: replication }
`

func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadGood(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"topology.yaml": goodTopology,
		"bindings.yaml": "version: 1\ncomponents:\n  api:\n    - probe: k8s.workload\n      namespace: x\nedges:\n  api->db:\n    - probe: pg.pool\n",
	})
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Topology.AllComponents()) != 6 {
		t.Errorf("want 6 components including role instances, got %d", len(cfg.Topology.AllComponents()))
	}
	if cfg.Topology.BoxFor("db") != "db-p" {
		t.Errorf("db with roles should anchor on its primary")
	}
	if got := cfg.Topology.EntryPoints(); len(got) != 1 || got[0] != "lb" {
		t.Errorf("entry points = %v", got)
	}
}

func TestLoadSchemaAndCrossCheck(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"topology.yaml": strings.Replace(goodTopology, "type: lb", "type: balancer", 1) + "  - { from: api, to: nope, kind: sql }\n",
		"bindings.yaml": "version: 1\ncomponents:\n  ghost:\n    - probe: k8s.node\n",
	})
	_, err := Load(dir)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want ValidationError, got %v", err)
	}
	joined := ve.Error()
	if !strings.Contains(joined, "/components/0/type") {
		t.Errorf("schema issue missing: %s", joined)
	}
	// Cross checks only run when the schemas pass, so fix the type and retry.
	dir2 := writeDir(t, map[string]string{
		"topology.yaml": goodTopology + "  - { from: api, to: nope, kind: sql }\n",
		"bindings.yaml": "version: 1\ncomponents:\n  ghost:\n    - probe: k8s.node\n",
	})
	_, err = Load(dir2)
	ve, ok = err.(*ValidationError)
	if !ok {
		t.Fatalf("want ValidationError, got %v", err)
	}
	joined = ve.Error()
	for _, want := range []string{"unknown component nope", "no such component in topology.yaml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cross-check %q missing in:\n%s", want, joined)
		}
	}
}

func TestRefs(t *testing.T) {
	r, err := ParseRef("wassup://workload/api@2026-09-27T09:52:00Z")
	if err != nil || r.Kind != "workload" || r.ID != "api" || r.At.IsZero() {
		t.Fatalf("parse: %+v %v", r, err)
	}
	if got := r.String(); got != "wassup://workload/api@2026-09-27T09:52:00Z" {
		t.Errorf("round trip: %s", got)
	}
	e, _ := ParseRef("wassup://edge/api->db")
	if !e.IsEdge() || e.ID != "api->db" {
		t.Errorf("edge ref: %+v", e)
	}
	bare, _ := ParseRef("api")
	var topo Topology
	topo.Components = []Component{{ID: "api", Type: "workload"}}
	res, err := topo.Resolve(bare)
	if err != nil || res.Kind != "workload" {
		t.Errorf("resolve bare id: %+v %v", res, err)
	}
	if _, err := ParseRef("wassup://nothing"); err == nil {
		t.Error("want error for ref without id")
	}
}

func TestRemap(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"topology.yaml": goodTopology,
		"bindings.yaml": "version: 1\ncomponents:\n  api:\n    - probe: k8s.workload\n      namespace: x\nedges:\n  api->db:\n    - probe: pg.pool\n",
		"findings.yaml": "version: 1\nfindings:\n  - id: f1\n    severity: low\n    title: t\n    component: api\n",
		"layout.json":   `{"version":1,"components":{"api":{"x":1,"y":2}},"waypoints":{"api->db":[[3,4]]}}`,
	})
	if err := SaveAnnotations(dir, Annotations{Version: 1, Annotations: []Annotation{{ID: "a", Path: []string{"wassup://workload/api", "wassup://edge/api->db"}, Note: "n"}}}); err != nil {
		t.Fatal(err)
	}
	touched, err := Remap(dir, "api", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != 5 {
		t.Errorf("touched %v", touched)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("reload after remap: %v", err)
	}
	if _, ok := cfg.Topology.Component("backend"); !ok {
		t.Error("component not renamed")
	}
	if _, ok := cfg.Bindings.Edges["backend->db"]; !ok {
		t.Errorf("edge binding not renamed: %v", cfg.Bindings.Edges)
	}
	if cfg.Findings.Findings[0].Component != "backend" {
		t.Error("finding not renamed")
	}
	if _, ok := cfg.Layout.Components["backend"]; !ok {
		t.Error("layout not renamed")
	}
	if cfg.Annotations.Annotations[0].Path[1] != "wassup://edge/backend->db" {
		t.Errorf("annotation not renamed: %v", cfg.Annotations.Annotations[0].Path)
	}
	if _, err := Remap(dir, "backend", "Bad Id"); err == nil {
		t.Error("want slug error")
	}
}

func TestThresholds(t *testing.T) {
	th := Thresholds{Defaults: ThresholdSet{GaugeRedPct: 95}, Components: map[string]ThresholdSet{"q": {QueueDepth: 3}}}
	if got := th.For("q"); got.QueueDepth != 3 || got.GaugeRedPct != 95 || got.GaugeAmberPct != 80 || got.StaleTicks != 3 {
		t.Errorf("merge: %+v", got)
	}
}

func TestWriteAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state", "snapshot.json")
	if err := WriteAtomic(p, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}
