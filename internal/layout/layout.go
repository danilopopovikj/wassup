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

// Route is a drawn edge.
type Route struct {
	ID     string
	From   string // box id
	To     string // box id
	Cells  []Point
	Both   bool     // a merged pair of opposite edges
	Merged []string // edge ids drawn by this route
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
	return Options{MinBoxW: 18, MinBoxH: 5, LaneGap: 4, BoxGap: 4, GroupGap: 8}
}

// Graph is a complete layout.
type Graph struct {
	Boxes  map[string]*Box
	Order  []string // box ids in drawing order (lane by lane, left to right)
	Groups []GroupFrame
	Routes map[string]*Route
	W, H   int
	// BoxOf maps any element id (including db containers with roles) to the box that draws it.
	BoxOf map[string]string
}

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
	g := &Graph{Boxes: map[string]*Box{}, Routes: map[string]*Route{}, BoxOf: map[string]string{}}

	// 1. Which components are drawn as boxes: everything not inside a
	// collapsed group, plus one box per collapsed group. A db with roles is
	// drawn through its instances.
	groups := t.AllGroups()
	parentOf := map[string]string{}
	for _, gr := range groups {
		parentOf[gr.ID] = gr.Parent
	}
	collapsedAncestor := func(groupID string) string {
		for gid := groupID; gid != ""; gid = parentOf[gid] {
			if opts.Collapsed[gid] {
				top := gid
				// walk up further: an outer collapsed group wins
				for p := parentOf[gid]; p != ""; p = parentOf[p] {
					if opts.Collapsed[p] {
						top = p
					}
				}
				return top
			}
		}
		return ""
	}
	type item struct {
		id    string
		lane  model.Lane
		group string
		comp  model.Component
		gbox  bool
		mem   []string
	}
	var items []item
	seenGroupBox := map[string]bool{}
	for _, c := range t.AllComponents() {
		if c.Type == "db" && c.Roles != nil && c.Roles.Primary != "" {
			g.BoxOf[c.ID] = c.Roles.Primary
			continue // drawn as a group of instances
		}
		if ca := collapsedAncestor(c.Group); ca != "" {
			g.BoxOf[c.ID] = ca
			if !seenGroupBox[ca] {
				seenGroupBox[ca] = true
				items = append(items, item{id: ca, lane: model.LaneOf(c), group: parentOf[ca], gbox: true})
			}
			for i := range items {
				if items[i].id == ca {
					items[i].mem = append(items[i].mem, c.ID)
				}
			}
			continue
		}
		g.BoxOf[c.ID] = c.ID
		items = append(items, item{id: c.ID, lane: model.LaneOf(c), group: c.Group, comp: c})
	}

	// 2. Order within lanes by barycenter, keeping group members adjacent.
	byLane := map[model.Lane][]int{}
	laneIndex := map[model.Lane]int{}
	for i, ln := range model.AllLanes {
		laneIndex[ln] = i
	}
	for i, it := range items {
		byLane[it.lane] = append(byLane[it.lane], i)
	}
	pos := map[string]float64{} // current x-rank of each box id
	for _, ln := range model.AllLanes {
		for k, idx := range byLane[ln] {
			pos[items[idx].id] = float64(k)
		}
	}
	neighborsOf := func(id string, mem []string) []string {
		ids := []string{id}
		ids = append(ids, mem...)
		seen := map[string]bool{}
		var out []string
		for _, x := range ids {
			for _, n := range t.Neighbors(x) {
				b := g.BoxOf[n]
				if b == "" || b == id || seen[b] {
					continue
				}
				seen[b] = true
				out = append(out, b)
			}
		}
		return out
	}
	itemByID := map[string]int{}
	for i, it := range items {
		itemByID[it.id] = i
	}
	bary := func(idx int, adjacent func(model.Lane) bool) float64 {
		it := items[idx]
		sum, n := 0.0, 0
		for _, nb := range neighborsOf(it.id, it.mem) {
			ni, ok := itemByID[nb]
			if !ok || !adjacent(items[ni].lane) {
				continue
			}
			sum += pos[nb]
			n++
		}
		if n == 0 {
			return pos[it.id]
		}
		return sum / float64(n)
	}
	sweep := func(ln model.Lane, adjacent func(model.Lane) bool) {
		idxs := byLane[ln]
		if len(idxs) < 2 {
			return
		}
		b := map[int]float64{}
		for _, idx := range idxs {
			b[idx] = bary(idx, adjacent)
		}
		// group barycenter = mean of member barycenters
		gsum, gn := map[string]float64{}, map[string]int{}
		for _, idx := range idxs {
			gsum[items[idx].group] += b[idx]
			gn[items[idx].group]++
		}
		sort.SliceStable(idxs, func(x, y int) bool {
			gx, gy := items[idxs[x]].group, items[idxs[y]].group
			if gx != gy {
				bx, by := gsum[gx]/float64(gn[gx]), gsum[gy]/float64(gn[gy])
				if bx != by {
					return bx < by
				}
				return gx < gy
			}
			if b[idxs[x]] != b[idxs[y]] {
				return b[idxs[x]] < b[idxs[y]]
			}
			return items[idxs[x]].id < items[idxs[y]].id
		})
		for k, idx := range idxs {
			pos[items[idx].id] = float64(k)
		}
	}
	main := []model.Lane{model.LaneEdge, model.LaneCompute, model.LaneData}
	for sweepN := 0; sweepN < 2; sweepN++ {
		for i := 1; i < len(main); i++ {
			prev := main[i-1]
			sweep(main[i], func(l model.Lane) bool { return l == prev })
		}
		for i := len(main) - 2; i >= 0; i-- {
			next := main[i+1]
			sweep(main[i], func(l model.Lane) bool { return l == next })
		}
	}
	sweep(model.LaneSide, func(l model.Lane) bool { return l != model.LaneSide })

	// 3. Size boxes from content.
	size := func(it item) (int, int) {
		w, h := opts.MinBoxW, opts.MinBoxH
		if it.gbox {
			label := it.id
			if gr, ok := t.Group(it.id); ok && gr.Label != "" {
				label = gr.Label
			}
			if lw := len([]rune(label)) + 6; lw > w {
				w = lw
			}
			return w, h
		}
		c := it.comp
		label := c.DisplayLabel()
		if lw := len([]rune(label)) + 6; lw > w {
			w = lw
		}
		spec := model.Catalog[c.Type]
		gauges := len(spec.Gauges)
		if gauges > 4 {
			gauges = 4
		}
		if w < 25 && gauges > 0 {
			w = 25
		}
		switch opts.Detail {
		case model.DetailMinimal:
			// border(2) + label + state + one note line
			h = 2 + 1 + 1 + 1
		case model.DetailFull:
			// border(2) + label + state + gauges + note lines
			h = 2 + 1 + 1 + gauges + 2
			if c.Type == "node" {
				hosted := len(t.Hosted(c.ID))
				if hosted > 4 {
					hosted = 4
				}
				h += hosted
			}
		default:
			if gauges > 3 {
				gauges = 3
			}
			// border(2) + label + state + up to three gauges + one line for
			// hosted workloads or a change stamp
			h = 2 + 1 + 1 + gauges + 1
		}
		if opts.Compact {
			h = 2 + 1 + 1 + 1
		}
		if h < opts.MinBoxH {
			h = opts.MinBoxH
		}
		return w, h
	}

	// 4. Place lanes top to bottom, centered; side lane on the right.
	laneW := map[model.Lane]int{}
	laneH := map[model.Lane]int{}
	boxes := map[string]*Box{}
	for _, ln := range main {
		x := 0
		prevGroup := ""
		for k, idx := range byLane[ln] {
			it := items[idx]
			w, h := size(it)
			if k > 0 {
				if it.group != prevGroup {
					x += opts.GroupGap
				} else {
					x += opts.BoxGap
				}
			}
			b := &Box{ID: it.id, X: x, W: w, H: h, Lane: ln, Group: it.group, GroupBox: it.gbox, Members: it.mem}
			boxes[it.id] = b
			x += w
			if h > laneH[ln] {
				laneH[ln] = h
			}
			prevGroup = it.group
		}
		laneW[ln] = x
	}
	maxW := 0
	for _, ln := range main {
		if laneW[ln] > maxW {
			maxW = laneW[ln]
		}
	}
	// group nesting depth adds padding rows between lanes
	maxDepth := 0
	depthOf := func(gid string) int {
		d := 0
		for p := parentOf[gid]; p != ""; p = parentOf[p] {
			d++
		}
		return d
	}
	for _, gr := range groups {
		if d := depthOf(gr.ID); d > maxDepth {
			maxDepth = d
		}
	}
	y := 2 + maxDepth
	for _, ln := range main {
		if len(byLane[ln]) == 0 {
			continue
		}
		off := (maxW - laneW[ln]) / 2
		for _, idx := range byLane[ln] {
			b := boxes[items[idx].id]
			b.X += off + 2 + maxDepth
			b.Y = y
		}
		y += laneH[ln] + opts.LaneGap + 2*maxDepth
	}
	sideX := maxW + 2*(2+maxDepth) + opts.GroupGap
	sy := 1
	for _, idx := range byLane[model.LaneSide] {
		it := items[idx]
		w, h := size(it)
		b := &Box{ID: it.id, X: sideX, Y: sy, W: w, H: h, Lane: model.LaneSide, Group: it.group, GroupBox: it.gbox, Members: it.mem}
		boxes[it.id] = b
		sy += h + 2
	}

	// 5. Apply saved positions; new boxes avoid saved ones.
	for id, b := range boxes {
		if p, ok := l.Components[id]; ok && !p.Auto {
			b.X, b.Y = p.X, p.Y
			if p.W >= opts.MinBoxW {
				b.W = p.W
			}
			if p.H >= 3 {
				b.H = p.H
			}
			b.Saved = true
		}
	}
	ids := make([]string, 0, len(boxes))
	for id := range boxes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b := boxes[id]
		if b.Saved {
			continue
		}
		for tries := 0; tries < 200; tries++ {
			hit := false
			for _, oid := range ids {
				o := boxes[oid]
				if o == b || !o.Saved && oid > id {
					continue
				}
				if o != b && overlaps(b, o) {
					hit = true
					break
				}
			}
			if !hit {
				break
			}
			b.X += 2
		}
		if b.X < 0 {
			b.X = 0
		}
		if b.Y < 0 {
			b.Y = 0
		}
	}
	g.Boxes = boxes
	for _, ln := range model.AllLanes {
		var laneIDs []string
		for _, idx := range byLane[ln] {
			laneIDs = append(laneIDs, items[idx].id)
		}
		sort.SliceStable(laneIDs, func(i, j int) bool {
			bi, bj := boxes[laneIDs[i]], boxes[laneIDs[j]]
			if bi.Y != bj.Y {
				return bi.Y < bj.Y
			}
			return bi.X < bj.X
		})
		g.Order = append(g.Order, laneIDs...)
	}

	// 6. Group frames: bounding boxes of descendants, padded by depth.
	g.Groups = frames(t, g, groups, parentOf, opts)

	// Canvas size.
	for _, b := range boxes {
		if b.Right()+2 > g.W {
			g.W = b.Right() + 2
		}
		if b.Bottom()+2 > g.H {
			g.H = b.Bottom() + 2
		}
	}
	for _, gf := range g.Groups {
		if gf.X+gf.W+1 > g.W {
			g.W = gf.X + gf.W + 1
		}
		if gf.Y+gf.H+1 > g.H {
			g.H = gf.Y + gf.H + 1
		}
	}

	// 7. Route edges.
	routeAll(t, g, l)
	return g
}

func overlaps(a, b *Box) bool {
	return a.X < b.Right()+1 && b.X < a.Right()+1 && a.Y < b.Bottom()+1 && b.Y < a.Bottom()+1
}

func frames(t *model.Topology, g *Graph, groups []model.Group, parentOf map[string]string, opts Options) []GroupFrame {
	var out []GroupFrame
	depthOf := func(gid string) int {
		d := 0
		for p := parentOf[gid]; p != ""; p = parentOf[p] {
			d++
		}
		return d
	}
	maxDepth := 0
	for _, gr := range groups {
		if d := depthOf(gr.ID); d > maxDepth {
			maxDepth = d
		}
	}
	for _, gr := range groups {
		if opts.Collapsed[gr.ID] {
			// drawn as a box, unless an ancestor is also collapsed
			continue
		}
		var members []string
		for _, id := range t.GroupDescendants(gr.ID) {
			if b := g.BoxOf[id]; b != "" {
				if bx, ok := g.Boxes[b]; ok && bx.Lane == model.LaneSide {
					continue // the side column stands outside the frames
				}
				members = append(members, b)
			}
		}
		if len(members) == 0 {
			continue
		}
		minX, minY, maxX, maxY := 1<<30, 1<<30, -1, -1
		seen := map[string]bool{}
		for _, m := range members {
			b, ok := g.Boxes[m]
			if !ok || seen[m] {
				continue
			}
			seen[m] = true
			if b.X < minX {
				minX = b.X
			}
			if b.Y < minY {
				minY = b.Y
			}
			if b.Right() > maxX {
				maxX = b.Right()
			}
			if b.Bottom() > maxY {
				maxY = b.Bottom()
			}
		}
		if maxX < 0 {
			continue
		}
		pad := 2 + (maxDepth - depthOf(gr.ID))
		label := gr.Label
		if label == "" {
			label = gr.ID
		}
		f := GroupFrame{ID: gr.ID, Label: label, Kind: gr.Kind, Depth: depthOf(gr.ID),
			X: minX - pad, Y: minY - pad, W: maxX - minX + 2*pad, H: maxY - minY + 2*pad}
		if f.X < 0 {
			f.X = 0
		}
		if f.Y < 0 {
			f.Y = 0
		}
		f.Children = members
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].ID < out[j].ID
	})
	return out
}
