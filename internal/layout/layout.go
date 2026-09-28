// Package layout places components in lanes and routes edges. It is
// deterministic, runs only when the topology or the layout file changes,
// and never moves a box the user has placed.
package layout

import (
	"sort"

	"github.com/danilopopovikj/wassup/internal/model"
)

// Box is a placed component (or a collapsed group).
type Box struct {
	ID    string
	X, Y  int
	W, H  int
	Lane  model.Lane
	Group string
	// Saved is true when the position came from layout.json.
	Saved bool
	// GroupBox is true when the box stands for a collapsed group.
	GroupBox bool
	// Members lists the component ids inside a collapsed group box.
	Members []string
	// Frame is true for a node drawn as a container: the instances of the
	// components that run on it sit inside, below Header interior rows kept
	// for the node's own label, state and gauges.
	Frame  bool
	Header int
	// Instance names the component an instance box stands for and Node the
	// node it sits in; the box id is "<component>@<node>".
	Instance string
	Node     string
}

// Element returns the element a box stands for: the component of an
// instance, otherwise the box id itself.
func (b *Box) Element() string {
	if b.Instance != "" {
		return b.Instance
	}
	return b.ID
}

// Inside reports whether o lies within b (an instance in its node).
func (b *Box) Inside(o *Box) bool {
	return o.X >= b.X && o.Right() <= b.Right() && o.Y >= b.Y && o.Bottom() <= b.Bottom()
}

// Right and Bottom are exclusive bounds.
func (b Box) Right() int  { return b.X + b.W }
func (b Box) Bottom() int { return b.Y + b.H }

// Contains reports whether a cell is inside the box.
func (b Box) Contains(x, y int) bool {
	return x >= b.X && x < b.X+b.W && y >= b.Y && y < b.Y+b.H
}

// Center returns the middle cell.
func (b Box) Center() (int, int) { return b.X + b.W/2, b.Y + b.H/2 }

// GroupFrame is a titled border around the members of a group.
type GroupFrame struct {
	ID       string
	Label    string
	Kind     string
	X, Y     int
	W, H     int
	Depth    int // nesting depth, outermost 0
	Children []string
}

// Point is a grid cell.
type Point struct{ X, Y int }

// Route is a drawn edge. A logical edge between components drawn inside
// nodes has one route per instance pair; they share Edge and Merged.
type Route struct {
	ID     string
	Edge   string // the logical edge id
	From   string // box id
	To     string // box id
	Cells  []Point
	Both   bool     // a merged pair of opposite edges
	Merged []string // edge ids drawn by this route
	// Net names the wires this route shares its cells with: where two routes
	// of one net meet they are joined, where two nets meet they cross. A
	// route the router found on its own has none.
	Net string
}

// Options tune the layout.
type Options struct {
	MinBoxW   int
	MinBoxH   int
	LaneGap   int
	BoxGap    int
	GroupGap  int
	Collapsed map[string]bool
	// Compact reduces box height to label plus state, for small terminals.
	Compact bool
	// Detail is minimal, normal or full (see model.DetailLevel). Normal shows
	// at most three gauges and one line of notes; full shows everything.
	Detail string
}

// DefaultOptions are the documented defaults.
func DefaultOptions() Options {
	return Options{MinBoxW: 18, MinBoxH: 5, LaneGap: 6, BoxGap: 6, GroupGap: 10}
}

// Graph is a complete layout.
type Graph struct {
	Boxes  map[string]*Box
	Order  []string // box ids in drawing order (lane by lane, left to right)
	Groups []GroupFrame
	Routes map[string]*Route
	W, H   int
	// BoxOf maps any element id (including db containers with roles) to the box that draws it.
	// A component drawn inside nodes maps to its first instance.
	BoxOf map[string]string
	// Instances lists, per component drawn inside nodes, its instance box ids
	// in node order.
	Instances map[string][]string
}

// ElementOf returns the element id a box id stands for (see Box.Element).
func (g *Graph) ElementOf(boxID string) string {
	if b, ok := g.Boxes[boxID]; ok {
		return b.Element()
	}
	return boxID
}

// Layout constants of the machine view.
const (
	// instanceGap is the rows between two instances in a node, room for an
	// arrow from one to the next.
	instanceGap = 2
	// frameTop and framePad are the interior rows above the first and below
	// the last instance: room for a port and its run-up.
	frameTop = 2
	framePad = 1
)

// sideFramePad is the room between the frame of a group on the side and the
// boxes it holds.
const sideFramePad = 2

// BoxAt returns the box under a cell, preferring the smallest.
func (g *Graph) BoxAt(x, y int) *Box {
	var best *Box
	for _, id := range g.Order {
		b := g.Boxes[id]
		if b.Contains(x, y) && (best == nil || b.W*b.H < best.W*best.H) {
			best = b
		}
	}
	return best
}

// GroupTitleAt returns the group whose title row holds the cell.
func (g *Graph) GroupTitleAt(x, y int) *GroupFrame {
	for i := range g.Groups {
		gf := &g.Groups[i]
		if y == gf.Y && x >= gf.X && x < gf.X+gf.W {
			return gf
		}
	}
	return nil
}

// RouteAt returns the route that passes through a cell.
func (g *Graph) RouteAt(x, y int) *Route {
	for _, id := range sortedRouteIDs(g.Routes) {
		r := g.Routes[id]
		for _, p := range r.Cells {
			if p.X == x && p.Y == y {
				return r
			}
		}
	}
	return nil
}

func sortedRouteIDs(m map[string]*Route) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Compute lays out the topology. Saved positions in l win over the
// algorithm; new components take the nearest free auto slot in their lane.
//
// The picture has a fixed shape. The way in stands on top, the machines that
// run the application in the middle, what holds data at the bottom, and what
// is outside the system in a column on the right. Wires run in the room
// between the boxes (see wire.go); how much room that takes is only known
// once they are drawn, so the boxes are placed and wired until the room fits.
func Compute(t *model.Topology, l model.Layout, opts Options) *Graph {
	if opts.MinBoxW == 0 {
		opts = DefaultOptions()
	}
	if opts.Collapsed == nil {
		opts.Collapsed = map[string]bool{}
		for _, id := range l.Collapsed {
			opts.Collapsed[id] = true
		}
	}
	p := newPlan(t, l, opts)
	room := map[string]int{}
	var g *Graph
	for pass := 0; pass < 4; pass++ {
		g = p.place(room)
		grown := false
		for where, lanes := range p.wire(g, room, pass == 3) {
			if lanes > room[where] {
				room[where], grown = lanes, true
			}
		}
		if !grown {
			break
		}
	}
	return g
}

// roleNodes says which node draws each instance of a database with roles
// (see model.RoleNodes): the instances of a database that names one node
// per instance are drawn inside those nodes. A database that does not, or
// that is folded into a collapsed group, or whose node is not drawn, keeps
// its instances in the data lane.
func roleNodes(t *model.Topology, collapsedAncestor func(string) string) (roleNode map[string]string, pinned map[string]bool) {
	roleNode, pinned = map[string]string{}, map[string]bool{}
	held := t.RoleNodes()
	for _, c := range t.Components {
		if c.Roles == nil || held[c.Roles.Primary] == "" || collapsedAncestor(c.ID) != "" {
			continue
		}
		drawn := true
		for _, n := range c.RunsOn {
			nc, ok := t.Component(n)
			if !ok || nc.Type != "node" || collapsedAncestor(nc.Group) != "" {
				drawn = false
			}
		}
		if !drawn {
			continue
		}
		pinned[c.ID] = true
		for _, id := range append([]string{c.Roles.Primary}, c.Roles.Replicas...) {
			roleNode[id] = held[id]
		}
	}
	return roleNode, pinned
}

func overlaps(a, b *Box) bool {
	return a.X < b.Right()+1 && b.X < a.Right()+1 && a.Y < b.Bottom()+1 && b.Y < a.Bottom()+1
}

// frames draws a border around the members of every group that is not
// folded. The side column stands outside the frames of the system; a group
// that stands there as a whole (sideFramed) has a frame of its own. A group
// with a box of another among its own is framed band by band, under the
// same title, so that no frame holds what does not belong to it.
func (p *plan) frames(g *Graph) []GroupFrame {
	var out []GroupFrame
	_, _, broken := p.spans(g)
	for _, gr := range p.groups {
		if p.opts.Collapsed[gr.ID] {
			// drawn as a box, unless an ancestor is also collapsed
			continue
		}
		parts := map[int][]string{}
		seen := map[string]bool{}
		for _, id := range p.t.GroupDescendants(gr.ID) {
			b := g.BoxOf[id]
			bx, ok := g.Boxes[b]
			if !ok || seen[b] || (bx.Lane == model.LaneSide && !p.sideFramed[gr.ID]) || bx.Instance != "" {
				continue // the side column stands outside the frames; instances live in their node
			}
			seen[b] = true
			part := 0
			if broken[gr.ID] {
				part = p.bandOf[b]
			}
			parts[part] = append(parts[part], b)
		}
		pad := 2 + (p.maxDepth - p.depthOf(gr.ID))
		if p.sideFramed[gr.ID] {
			pad = sideFramePad
		}
		label := gr.Label
		if label == "" {
			label = gr.ID
		}
		keys := make([]int, 0, len(parts))
		for k := range parts {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			minX, minY, maxX, maxY := 1<<30, 1<<30, -1, -1
			for _, m := range parts[k] {
				b := g.Boxes[m]
				minX, minY = min(minX, b.X), min(minY, b.Y)
				maxX, maxY = max(maxX, b.Right()), max(maxY, b.Bottom())
			}
			f := GroupFrame{ID: gr.ID, Label: label, Kind: gr.Kind, Depth: p.depthOf(gr.ID), Children: parts[k],
				X: max(minX-pad, 0), Y: max(minY-pad, 0), W: maxX - minX + 2*pad, H: maxY - minY + 2*pad}
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].ID < out[j].ID
	})
	return out
}
