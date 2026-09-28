package layout

import (
	"testing"

	"github.com/danilopopovikj/wassup/internal/model"
)

func sample() *model.Topology {
	return &model.Topology{Name: "t",
		Groups: []model.Group{{ID: "cloud", Kind: "cloud", Label: "Cloud"}, {ID: "k", Kind: "cluster", Parent: "cloud"}},
		Components: []model.Component{
			{ID: "lb", Type: "loadbalancer", Group: "cloud"},
			{ID: "n1", Type: "node", Group: "k"},
			{ID: "n2", Type: "node", Group: "k"},
			{ID: "api", Type: "workload", Group: "k", RunsOn: []string{"n1", "n2"}},
			{ID: "worker", Type: "workload", Group: "k"},
			{ID: "q", Type: "queue", Group: "k"},
			{ID: "db", Type: "database", Group: "k", Roles: &model.Roles{Primary: "p", Replicas: []string{"r"}}},
			{ID: "ext", Type: "external"},
		},
		Edges: []model.Edge{
			{From: "lb", To: "api", Kind: "http"}, {From: "api", To: "db", Kind: "sql"}, {From: "api", To: "q", Kind: "queue"},
			{From: "q", To: "worker", Kind: "queue"}, {From: "worker", To: "ext", Kind: "external"}, {From: "p", To: "r", Kind: "replication"},
			{From: "lb", To: "n1", Kind: "tcp"}, {From: "lb", To: "n2", Kind: "tcp"},
		}}
}

func TestDeterministicAndNonOverlapping(t *testing.T) {
	t1 := sample()
	g1 := Compute(t1, model.Layout{}, DefaultOptions())
	g2 := Compute(sample(), model.Layout{}, DefaultOptions())
	for id, b := range g1.Boxes {
		o := g2.Boxes[id]
		if o == nil || o.X != b.X || o.Y != b.Y || o.W != b.W || o.H != b.H {
			t.Errorf("layout not deterministic for %s", id)
		}
	}
	ids := g1.Order
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if overlaps(g1.Boxes[ids[i]], g1.Boxes[ids[j]]) {
				t.Errorf("%s overlaps %s", ids[i], ids[j])
			}
		}
	}
	// lanes: lb above compute above data
	if !(g1.Boxes["lb"].Y < g1.Boxes["api"].Y && g1.Boxes["api"].Y < g1.Boxes["p"].Y) {
		t.Errorf("lane order wrong: lb %d api %d p %d", g1.Boxes["lb"].Y, g1.Boxes["api"].Y, g1.Boxes["p"].Y)
	}
	if g1.Boxes["ext"].X <= g1.Boxes["api"].Right() {
		t.Errorf("side lane should sit right of the main lanes")
	}
	if len(g1.Routes) != len(t1.Edges) {
		t.Errorf("want %d routes, got %d", len(t1.Edges), len(g1.Routes))
	}
	for id, r := range g1.Routes {
		if len(r.Cells) < 2 {
			t.Errorf("route %s has no cells", id)
		}
	}
	if _, ok := g1.Boxes["db"]; ok {
		t.Error("a db with roles is drawn through its instances, not as its own box")
	}
	if g1.BoxOf["db"] != "p" {
		t.Errorf("BoxOf db = %s", g1.BoxOf["db"])
	}
}

func TestSavedPositionsWinAndCollapse(t *testing.T) {
	l := model.Layout{Components: map[string]model.Placement{"api": {X: 3, Y: 40}}}
	g := Compute(sample(), l, DefaultOptions())
	if b := g.Boxes["api"]; b.X != 3 || b.Y != 40 || !b.Saved {
		t.Errorf("saved position ignored: %+v", b)
	}
	l.Collapsed = []string{"k"}
	g = Compute(sample(), l, DefaultOptions())
	if _, ok := g.Boxes["api"]; ok {
		t.Error("collapsed group members must not be boxes")
	}
	if b, ok := g.Boxes["k"]; !ok || !b.GroupBox || len(b.Members) < 5 {
		t.Errorf("collapsed group box missing: %+v", b)
	}
}

func TestMachinesRow(t *testing.T) {
	tp := sample()
	// declared out of order on purpose: the row must follow the topology
	tp.Components = append(tp.Components, model.Component{ID: "n0", Type: "node", Group: "k"})
	g := Compute(tp, model.Layout{}, DefaultOptions())
	n1, n2, n0 := g.Boxes["n1"], g.Boxes["n2"], g.Boxes["n0"]
	if !(g.Boxes["lb"].Y < n1.Y && n1.Y < g.Boxes["api"].Y) {
		t.Errorf("machines must sit between the edge and the workloads: lb %d n1 %d api %d", g.Boxes["lb"].Y, n1.Y, g.Boxes["api"].Y)
	}
	if n1.Y != n2.Y || n2.Y != n0.Y {
		t.Errorf("nodes must share one row: %d %d %d", n1.Y, n2.Y, n0.Y)
	}
	if !(n1.X < n2.X && n2.X < n0.X) {
		t.Errorf("nodes must keep the topology order: n1 %d n2 %d n0 %d", n1.X, n2.X, n0.X)
	}
	// a node with residents is tall enough to list them
	if n1.H < DefaultOptions().MinBoxH+1 {
		t.Errorf("n1 hosts api, its box must have a row for it: h=%d", n1.H)
	}
	if n0.H >= n1.H {
		t.Errorf("an empty node needs no rows: n0 %d n1 %d", n0.H, n1.H)
	}
}
