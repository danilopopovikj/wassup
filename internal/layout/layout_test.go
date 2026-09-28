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
			a, b := g1.Boxes[ids[i]], g1.Boxes[ids[j]]
			if (a.Frame && a.Inside(b)) || (b.Frame && b.Inside(a)) {
				continue // an instance sits inside its node
			}
			if overlaps(a, b) {
				t.Errorf("%s overlaps %s", ids[i], ids[j])
			}
		}
	}
	// lanes: lb above the machines above data
	api := g1.Boxes[g1.BoxOf["api"]]
	if !(g1.Boxes["lb"].Y < api.Y && api.Y < g1.Boxes["p"].Y) {
		t.Errorf("lane order wrong: lb %d api %d p %d", g1.Boxes["lb"].Y, api.Y, g1.Boxes["p"].Y)
	}
	if g1.Boxes["ext"].X <= g1.Boxes["worker"].Right() {
		t.Errorf("side lane should sit right of the main lanes")
	}
	// every edge is drawn at least once; edges to a component inside nodes
	// once per instance, sharing the logical edge id
	drawn := map[string]int{}
	for _, r := range g1.Routes {
		drawn[r.Edge]++
	}
	for _, e := range t1.Edges {
		if drawn[e.ID()] == 0 {
			t.Errorf("edge %s has no route", e.ID())
		}
	}
	if drawn["api->db"] != 2 || drawn["lb->api"] != 2 {
		t.Errorf("api runs on two nodes, its edges must be drawn twice: %v", drawn)
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

func TestDatabaseInstancesInsideTheirMachines(t *testing.T) {
	onNodes := func(nodes ...string) *model.Topology {
		tp := sample()
		for i := range tp.Components {
			if tp.Components[i].ID == "db" {
				tp.Components[i].RunsOn = nodes
			}
		}
		return tp
	}
	// one node per instance, primary first: each instance is drawn in its node
	g := Compute(onNodes("n1", "n2"), model.Layout{}, DefaultOptions())
	for inst, node := range map[string]string{"p": "n1", "r": "n2"} {
		b, n := g.Boxes[inst+"@"+node], g.Boxes[node]
		if b == nil || !n.Inside(b) || b.Instance != inst || b.Node != node {
			t.Fatalf("%s must sit inside %s: %+v in %+v", inst, node, b, n)
		}
		if g.BoxOf[inst] != b.ID {
			t.Errorf("BoxOf %s = %s", inst, g.BoxOf[inst])
		}
		if _, ok := g.Boxes[inst]; ok {
			t.Errorf("%s is drawn inside %s, not in the data lane too", inst, node)
		}
	}
	for _, id := range []string{"db", "db@n1", "db@n2"} {
		if _, ok := g.Boxes[id]; ok {
			t.Errorf("the database is drawn through its instances, not as %s", id)
		}
	}
	if g.BoxOf["db"] != "p@n1" {
		t.Errorf("edges to the database anchor on its primary: BoxOf db = %s", g.BoxOf["db"])
	}
	for _, f := range g.Groups {
		if f.ID == "db" {
			t.Error("instances inside machines need no frame of their own")
		}
	}
	drawn := map[string]int{}
	for _, r := range g.Routes {
		drawn[r.Edge]++
	}
	if drawn["p->r"] != 1 || drawn["api->db"] != 2 {
		t.Errorf("replication runs between the machines, the api reaches the primary from both: %v", drawn)
	}

	// any other count does not say which node holds which instance
	g = Compute(onNodes("n1"), model.Layout{}, DefaultOptions())
	if g.Boxes["p"] == nil || g.Boxes["r"] == nil || g.Boxes["p@n1"] != nil {
		t.Error("one node for two instances: they keep their place in the data lane")
	}
}

func TestSavedPositionsWinAndCollapse(t *testing.T) {
	l := model.Layout{Components: map[string]model.Placement{"worker": {X: 3, Y: 60}}}
	g := Compute(sample(), l, DefaultOptions())
	if b := g.Boxes["worker"]; b.X != 3 || b.Y != 60 || !b.Saved {
		t.Errorf("saved position ignored: %+v", b)
	}
	l.Collapsed = []string{"k"}
	g = Compute(sample(), l, DefaultOptions())
	if _, ok := g.Boxes["api@n1"]; ok {
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
	if !(g.Boxes["lb"].Y < n1.Y && n1.Y < g.Boxes["worker"].Y) {
		t.Errorf("machines must sit between the edge and the workloads: lb %d n1 %d worker %d", g.Boxes["lb"].Y, n1.Y, g.Boxes["worker"].Y)
	}
	if n1.Y != n2.Y || n2.Y != n0.Y {
		t.Errorf("nodes must share one row: %d %d %d", n1.Y, n2.Y, n0.Y)
	}
	if !(n1.X < n2.X && n2.X < n0.X) {
		t.Errorf("nodes must keep the topology order: n1 %d n2 %d n0 %d", n1.X, n2.X, n0.X)
	}
	// a node with residents is a frame holding one instance box per resident
	if !n1.Frame || n1.H <= DefaultOptions().MinBoxH {
		t.Errorf("n1 hosts api, it must be a frame with room: %+v", n1)
	}
	if n0.Frame || n0.H >= n1.H {
		t.Errorf("an empty node is a plain box: n0 %+v", n0)
	}
	inst := g.Boxes["api@n1"]
	if inst == nil || !n1.Inside(inst) || inst.Instance != "api" || inst.Node != "n1" || inst.Y <= n1.Y+n1.Header {
		t.Errorf("api@n1 must sit inside n1 below its header: %+v in %+v", inst, n1)
	}
	if g.BoxOf["api"] != "api@n1" || len(g.Instances["api"]) != 2 {
		t.Errorf("api maps to its instances: %s %v", g.BoxOf["api"], g.Instances["api"])
	}
	if !n2.Inside(g.Boxes["api@n2"]) {
		t.Error("each instance inside its own node")
	}
	// the namespace frame does not stretch over the machines
	for _, f := range g.Groups {
		for _, ch := range f.Children {
			if g.Boxes[ch] != nil && g.Boxes[ch].Instance != "" {
				t.Errorf("frame %s claims instance %s", f.ID, ch)
			}
		}
	}
}

// A group whose boxes all stand in the side column is framed there, below
// the side boxes that belong to no such group, so that it can be folded
// into one box with a click on its title.
func TestAGroupOnTheSideIsFramedThere(t *testing.T) {
	top := sample()
	top.Groups = append(top.Groups, model.Group{ID: "store", Kind: "cloud", Label: "Object store"},
		model.Group{ID: "watch", Kind: "namespace", Parent: "k"})
	top.Components = append(top.Components,
		// on the side as a whole, and part of the cluster: framed nowhere,
		// since its frame would stand away from the cluster's
		model.Component{ID: "traces", Type: "observability", Group: "watch"},
		model.Component{ID: "uploads", Type: "storage", Group: "store", Lane: "side"},
		model.Component{ID: "backups", Type: "storage", Group: "store", Lane: "side"},
		// on the side, in a group that has boxes elsewhere: no frame of its own
		model.Component{ID: "nightly", Type: "scheduledjob", Group: "k", Lane: "side"},
	)
	top.Edges = append(top.Edges, model.Edge{From: "api", To: "uploads", Kind: "http"})
	g := Compute(top, model.Layout{}, DefaultOptions())

	var frame *GroupFrame
	for i := range g.Groups {
		if g.Groups[i].ID == "store" {
			frame = &g.Groups[i]
		}
	}
	if frame == nil {
		t.Fatalf("the group on the side has no frame: %+v", g.Groups)
	}
	inside := func(f *GroupFrame, b *Box) bool {
		return b.X > f.X && b.Y > f.Y && b.Right() < f.X+f.W && b.Bottom() < f.Y+f.H
	}
	for _, id := range []string{"uploads", "backups"} {
		b := g.Boxes[id]
		if b == nil || b.Lane != model.LaneSide || !inside(frame, b) {
			t.Errorf("%s = %+v is not inside the frame %+v", id, b, frame)
		}
	}
	for _, f := range g.Groups {
		if f.ID == "watch" {
			t.Errorf("a group that is part of another has a frame on the side: %+v", f)
		}
	}
	for _, id := range []string{"ext", "nightly", "traces"} {
		b := g.Boxes[id]
		if b == nil {
			t.Fatalf("%s is not drawn", id)
		}
		if b.Bottom() >= frame.Y {
			t.Errorf("%s (y %d to %d) stands below the top of the frame (y %d): a framed group comes last", id, b.Y, b.Bottom(), frame.Y)
		}
		if b.X < frame.X+frame.W && frame.X < b.Right() && b.Y < frame.Y+frame.H && frame.Y < b.Bottom() {
			t.Errorf("%s overlaps the frame", id)
		}
	}
	for _, f := range g.Groups {
		if f.ID == "store" {
			continue
		}
		for _, id := range []string{"uploads", "backups", "nightly", "ext", "traces"} {
			for _, m := range f.Children {
				if m == id {
					t.Errorf("the frame of %s holds %s, which stands on the side", f.ID, id)
				}
			}
		}
		if f.X < frame.X+frame.W && frame.X < f.X+f.W && f.Y < frame.Y+frame.H && frame.Y < f.Y+f.H {
			t.Errorf("the frame of %s overlaps the one on the side", f.ID)
		}
	}
	if gf := g.GroupTitleAt(frame.X+2, frame.Y); gf == nil || gf.ID != "store" {
		t.Errorf("a click on the title finds %+v", gf)
	}
	if g.W < frame.X+frame.W || g.H < frame.Y+frame.H {
		t.Errorf("the canvas %dx%d cuts the frame %+v", g.W, g.H, frame)
	}

	// folded, the group is one box on the side, still below the others
	g = Compute(top, model.Layout{Collapsed: []string{"store"}}, DefaultOptions())
	box := g.Boxes["store"]
	if box == nil || !box.GroupBox || box.Lane != model.LaneSide || len(box.Members) != 2 {
		t.Fatalf("the folded group = %+v", box)
	}
	for _, id := range []string{"ext", "nightly"} {
		if b := g.Boxes[id]; b.Y >= box.Y {
			t.Errorf("%s (y %d) stands below the folded group (y %d)", id, b.Y, box.Y)
		}
	}
	for _, f := range g.Groups {
		if f.ID == "store" {
			t.Errorf("a folded group has no frame: %+v", f)
		}
	}
}
