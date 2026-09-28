package layout

import (
	"testing"

	"github.com/danilopopovikj/wassup/internal/model"
)

// shop is a system of the usual shape: a name and a load balancer, two
// machines that run the application, one that holds the database, a queue
// that runs on no machine, and two services of others.
func shop() *model.Topology {
	return &model.Topology{Name: "t",
		Groups: []model.Group{{ID: "cloud", Kind: "cloud", Label: "Cloud"}, {ID: "k", Kind: "cluster", Parent: "cloud"}},
		Components: []model.Component{
			{ID: "name", Type: "dns", Group: "cloud"},
			{ID: "lb", Type: "loadbalancer", Group: "cloud"},
			{ID: "n1", Type: "node", Group: "k"},
			{ID: "n2", Type: "node", Group: "k"},
			{ID: "store", Type: "node", Group: "k"},
			{ID: "router", Type: "ingress", Group: "k", RunsOn: []string{"n1", "n2"}},
			{ID: "api", Type: "workload", Group: "k", RunsOn: []string{"n1", "n2"}},
			{ID: "worker", Type: "backgroundworker", Group: "k", RunsOn: []string{"n2"}},
			{ID: "jobs", Type: "queue", Group: "k"},
			{ID: "db", Type: "database", Group: "k", RunsOn: []string{"store"}},
			{ID: "watch", Type: "observability"},
			{ID: "nightly", Type: "scheduledjob", Lane: "side"},
			{ID: "mail", Type: "external"},
			{ID: "pay", Type: "external"},
		},
		Edges: []model.Edge{
			{From: "name", To: "lb", Kind: "tcp"}, {From: "lb", To: "router", Kind: "http"},
			{From: "router", To: "api", Kind: "http"}, {From: "api", To: "db", Kind: "sql"},
			{From: "api", To: "jobs", Kind: "queue"}, {From: "jobs", To: "worker", Kind: "queue"},
			{From: "worker", To: "db", Kind: "sql"},
			{From: "api", To: "mail", Kind: "external"}, {From: "api", To: "pay", Kind: "external"},
			{From: "worker", To: "mail", Kind: "external"},
		}}
}

// bends counts the corners of a route.
func bends(r *Route) int {
	n := 0
	for i := 2; i < len(r.Cells); i++ {
		if (r.Cells[i].X == r.Cells[i-1].X) != (r.Cells[i-1].X == r.Cells[i-2].X) {
			n++
		}
	}
	return n
}

// The picture has one shape: the way in on top, a name above the load
// balancer it points at, the machines that run the application below, what
// runs on no machine below them, and what holds data at the bottom.
func TestThePictureHasOneShape(t *testing.T) {
	g := Compute(shop(), model.Layout{}, DefaultOptions())
	name, lb, n1, n2, store, jobs := g.Boxes["name"], g.Boxes["lb"], g.Boxes["n1"], g.Boxes["n2"], g.Boxes["store"], g.Boxes["jobs"]
	if !(name.Bottom() <= lb.Y && lb.Bottom() <= n1.Y && n1.Bottom() <= jobs.Y && jobs.Bottom() <= store.Y) {
		t.Errorf("top to bottom: name %d lb %d machines %d queue %d data %d", name.Y, lb.Y, n1.Y, jobs.Y, store.Y)
	}
	if n1.Y != n2.Y || store.Y == n1.Y {
		t.Errorf("the machine that holds only data stands with the data: n1 %d n2 %d store %d", n1.Y, n2.Y, store.Y)
	}
	if db := g.Boxes["db@store"]; db == nil || !store.Inside(db) {
		t.Errorf("the database is drawn inside its machine: %+v", db)
	}
	// a machine that runs the application and holds data stays in the middle
	mixed := shop()
	for i := range mixed.Components {
		if mixed.Components[i].ID == "db" {
			mixed.Components[i].RunsOn = []string{"n2"}
		}
	}
	g = Compute(mixed, model.Layout{}, DefaultOptions())
	if g.Boxes["n2"].Y != g.Boxes["n1"].Y {
		t.Error("a machine with the application on it is no data machine")
	}
	if db, w := g.Boxes["db@n2"], g.Boxes["worker@n2"]; db == nil || db.Y <= w.Y {
		t.Errorf("on a machine, what holds data stands at the bottom: db %+v worker %+v", db, w)
	}
}

// The same component stands on the same row of every machine, and a machine
// that does not run it leaves the row empty.
func TestAComponentStandsOnOneRowOfEveryMachine(t *testing.T) {
	g := Compute(shop(), model.Layout{}, DefaultOptions())
	for _, c := range []string{"router", "api"} {
		a, b := g.Boxes[c+"@n1"], g.Boxes[c+"@n2"]
		if a == nil || b == nil || a.Y != b.Y {
			t.Errorf("%s on n1 and n2: %+v %+v", c, a, b)
		}
	}
	if g.Boxes["n1"].H != g.Boxes["n2"].H {
		t.Errorf("the machines of a band are one height: %d %d", g.Boxes["n1"].H, g.Boxes["n2"].H)
	}
	if w, api := g.Boxes["worker@n2"], g.Boxes["api@n2"]; w.Y <= api.Bottom() {
		t.Errorf("the worker stands below the api: %+v %+v", w, api)
	}
	if _, ok := g.Boxes["worker@n1"]; ok {
		t.Error("n1 runs no worker")
	}
}

// On the side stand the services of others first, then what watches the
// system, then the rest.
func TestTheSideColumnBeginsWithTheServicesOfOthers(t *testing.T) {
	g := Compute(shop(), model.Layout{}, DefaultOptions())
	mail, pay, watch, nightly := g.Boxes["mail"], g.Boxes["pay"], g.Boxes["watch"], g.Boxes["nightly"]
	if !(mail.Y < pay.Y && pay.Y < watch.Y && watch.Y < nightly.Y) {
		t.Errorf("mail %d pay %d watch %d nightly %d", mail.Y, pay.Y, watch.Y, nightly.Y)
	}
	for _, b := range []*Box{mail, pay, watch, nightly} {
		if b.X <= g.Boxes["n2"].Right() || b.X != mail.X {
			t.Errorf("%s stands at %d, right of the machines and in one column", b.ID, b.X)
		}
	}
}

// What leaves a component is one net: its routes share their cells up to
// the last stretch. No route passes through a box or over the header of a
// machine, and none takes more than a few corners.
func TestWiresAreDrawnAsNets(t *testing.T) {
	g := Compute(shop(), model.Layout{}, DefaultOptions())
	gr := newGrid(g)
	drawn := map[string]int{}
	for id, r := range g.Routes {
		drawn[r.Edge]++
		if r.Net == "" {
			t.Errorf("%s was left to the router", id)
		}
		if len(r.Cells) < 2 {
			t.Fatalf("%s has no cells", id)
		}
		for i, c := range r.Cells {
			if k := gr.idx(c.X, c.Y); !gr.in(c.X, c.Y) || gr.box[k] || gr.solid[k] {
				t.Errorf("%s passes through a box at %v", id, c)
			}
			if i > 0 && abs(c.X-r.Cells[i-1].X)+abs(c.Y-r.Cells[i-1].Y) != 1 {
				t.Errorf("%s jumps from %v to %v", id, r.Cells[i-1], c)
			}
		}
		if n := bends(r); n > 4 {
			t.Errorf("%s takes %d corners", id, n)
		}
		from, to := g.Boxes[r.From], g.Boxes[r.To]
		if first, last := r.Cells[0], r.Cells[len(r.Cells)-1]; !touches(from, first) || !touches(to, last) {
			t.Errorf("%s runs from %v to %v, its boxes are %+v and %+v", id, first, last, from, to)
		}
	}
	for _, e := range shop().Edges {
		if drawn[e.ID()] == 0 {
			t.Errorf("%s is not drawn", e.ID())
		}
	}
	// every copy of the api reaches every box the api calls
	if drawn["api->mail"] != 2 || drawn["api->db"] != 2 || drawn["router->api"] != 4 {
		t.Errorf("routes per edge: %v", drawn)
	}
	a, b := g.Routes["api->mail@api@n1>mail"], g.Routes["api->pay@api@n1>pay"]
	if a == nil || b == nil || a.Net != b.Net {
		t.Fatalf("the calls of the api are one net: %+v %+v", a, b)
	}
	shared := 0
	for i := range a.Cells {
		if i < len(b.Cells) && a.Cells[i] == b.Cells[i] {
			shared++
		}
	}
	if shared < 10 {
		t.Errorf("the two routes share %d cells", shared)
	}
	if w := g.Routes["worker->mail@worker@n2>mail"]; w == nil || w.Net == a.Net {
		t.Errorf("what the worker calls is a net of its own: %+v", w)
	}
	// the wire to the side goes right, the one to the data goes down
	if last := a.Cells[len(a.Cells)-1]; last.X != g.Boxes["mail"].X-1 {
		t.Errorf("a box on the side is entered from the left: %v", last)
	}
	d := g.Routes["api->db@api@n1>db@store"]
	if last := d.Cells[len(d.Cells)-1]; last.Y != g.Boxes["db@store"].Y-1 {
		t.Errorf("a box below is entered from the top: %v", last)
	}
}

// touches reports whether a cell lies against a box.
func touches(b *Box, c Point) bool {
	return c.X >= b.X-1 && c.X <= b.Right() && c.Y >= b.Y-1 && c.Y <= b.Bottom() && !b.Contains(c.X, c.Y)
}

// A wire from one band to another runs down the street, which is at the
// same place in every band: it is one straight line.
func TestFromBandToBandAWireRunsDownTheStreet(t *testing.T) {
	g := Compute(shop(), model.Layout{}, DefaultOptions())
	r := g.Routes["api->db@api@n1>db@store"]
	if r == nil {
		t.Fatal("no route")
	}
	longest, run := 0, 1
	for i := 1; i < len(r.Cells); i++ {
		if r.Cells[i].X == r.Cells[i-1].X {
			run++
		} else {
			run = 1
		}
		longest = max(longest, run)
	}
	if down := g.Boxes["store"].Y - g.Boxes["api@n1"].Bottom(); longest < down-6 {
		t.Errorf("the longest stretch down is %d of %d rows", longest, down)
	}
	if n := bends(r); n > 4 {
		t.Errorf("%d corners", n)
	}
}

// A box somebody dragged out of its band is still reached, by the router.
func TestADraggedBoxIsStillReached(t *testing.T) {
	l := model.Layout{Components: map[string]model.Placement{"jobs": {X: 2, Y: 3}}}
	g := Compute(shop(), l, DefaultOptions())
	if b := g.Boxes["jobs"]; b.X != 2 || b.Y != 3 || !b.Saved {
		t.Fatalf("the saved place is not kept: %+v", b)
	}
	gr := newGrid(g)
	for _, id := range []string{"api->jobs@api@n1>jobs", "jobs->worker@jobs>worker@n2"} {
		r := g.Routes[id]
		if r == nil || len(r.Cells) < 2 {
			t.Fatalf("%s is not drawn: %+v", id, r)
		}
		for _, c := range r.Cells[1 : len(r.Cells)-1] {
			if gr.in(c.X, c.Y) && gr.box[gr.idx(c.X, c.Y)] {
				t.Errorf("%s passes through a box at %v", id, c)
			}
		}
	}
}

// A group with a box of another in one of its bands is framed band by band.
func TestABrokenGroupIsFramedBandByBand(t *testing.T) {
	top := shop()
	top.Groups = append(top.Groups, model.Group{ID: "app", Kind: "namespace", Parent: "k", Label: "App"})
	top.Components = append(top.Components, model.Component{ID: "cache", Type: "cache", Group: "app"})
	for i := range top.Components {
		if top.Components[i].ID == "jobs" {
			top.Components[i].Group = "app"
		}
	}
	g := Compute(top, model.Layout{}, DefaultOptions())
	var frames []GroupFrame
	for _, f := range g.Groups {
		if f.ID == "app" {
			frames = append(frames, f)
		}
	}
	if len(frames) != 2 {
		t.Fatalf("frames of the namespace: %+v", frames)
	}
	store := g.Boxes["store"]
	for _, f := range frames {
		if f.X < store.Right() && store.X < f.X+f.W && f.Y < store.Bottom() && store.Y < f.Y+f.H {
			t.Errorf("the frame %+v holds the machine %+v, which is not of the namespace", f, store)
		}
	}
}
