package layout

import (
	"container/heap"
	"sort"

	"github.com/danilopopovikj/wassup/internal/model"
)

// Costs of the orthogonal A* router.
const (
	costCell     = 1
	costCross    = 8
	costAdjacent = 6
	costFrame    = 80 // crossing a machine's wall: once to leave, never as a shortcut
	costGroup    = 20 // stepping on a group frame's border: cross it, never ride it
	costTurn     = 2
)

type side int

const (
	sideTop side = iota
	sideBottom
	sideLeft
	sideRight
)

type pending struct {
	id       string
	edge     string // the logical edge this route draws
	from, to string
	both     bool
	merged   []string
	dist     int
}

// endpoints returns the boxes an element is drawn by: its instances when it
// runs inside nodes, otherwise its one box.
func (g *Graph) endpoints(id string) []string {
	if inst := g.Instances[id]; len(inst) > 0 {
		return inst
	}
	b := g.BoxOf[id]
	if b == "" || g.Boxes[b] == nil {
		return nil
	}
	return []string{b}
}

// pairsOf expands a logical edge into the box pairs that draw it. Between
// two components that both run inside nodes the edge is drawn within each
// node they share (the ingress on a node hands to the api on that node);
// otherwise every instance connects to the other end.
func (g *Graph) pairsOf(e model.Edge) [][2]string {
	F, T := g.endpoints(e.From), g.endpoints(e.To)
	if len(F) == 0 || len(T) == 0 {
		return nil
	}
	var out [][2]string
	if len(g.Instances[e.From]) > 0 && len(g.Instances[e.To]) > 0 {
		for _, f := range F {
			for _, t := range T {
				if g.Boxes[f].Node == g.Boxes[t].Node {
					out = append(out, [2]string{f, t})
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	for _, f := range F {
		for _, t := range T {
			if f != t {
				out = append(out, [2]string{f, t})
			}
		}
	}
	return out
}

// routeAll routes every edge, shortest first. Opposite and parallel edges
// between the same two boxes share one route; the instance routes of one
// logical edge share their trunk, so an api on three nodes reaches the
// database as one bundle with one arrowhead.
func routeAll(t *model.Topology, g *Graph, l model.Layout) {
	byPair := map[string]*pending{}
	var list []*pending
	for _, e := range t.Edges {
		id := e.ID()
		for _, pr := range g.pairsOf(e) {
			from, to := pr[0], pr[1]
			if p, ok := byPair[from+"|"+to]; ok {
				if !contains(p.merged, id) {
					p.merged = append(p.merged, id)
				}
				continue
			}
			if p, ok := byPair[to+"|"+from]; ok {
				p.both = true
				if !contains(p.merged, id) {
					p.merged = append(p.merged, id)
				}
				continue
			}
			rid := id
			if g.Boxes[from].Instance != "" || g.Boxes[to].Instance != "" {
				rid = id + "@" + from + ">" + to
			}
			p := &pending{id: rid, edge: id, from: from, to: to, merged: []string{id}}
			fb, tb := g.Boxes[from], g.Boxes[to]
			fx, fy := fb.Center()
			tx, ty := tb.Center()
			p.dist = abs(fx-tx) + abs(fy-ty)
			byPair[from+"|"+to] = p
			list = append(list, p)
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].dist != list[j].dist {
			return list[i].dist < list[j].dist
		}
		return list[i].id < list[j].id
	})
	grid := newGrid(g)
	// Count how many routes leave/enter each box side to spread ports; the
	// routes of one logical edge into one box share a port so they merge.
	portUse := map[string]map[side]int{}
	shared := map[string]Point{}
	portFor := func(edge string, b *Box, s side, other string) Point {
		k := edge + "|" + b.ID + "|" + string(rune('0'+int(s)))
		if p, ok := shared[k]; ok {
			return p
		}
		p := grid.port(b, s, portUse, other)
		shared[k] = p
		return p
	}
	// outward is the cell one step away from a port, so a route leaves and
	// arrives perpendicular to the box side and the arrowhead points in.
	outward := func(p Point, s side) Point {
		switch s {
		case sideTop:
			return Point{p.X, p.Y - 1}
		case sideBottom:
			return Point{p.X, p.Y + 1}
		case sideLeft:
			return Point{p.X - 1, p.Y}
		}
		return Point{p.X + 1, p.Y}
	}
	bundles := map[string]map[int]bool{} // logical edge -> cells of its routes so far
	for _, p := range list {
		fb, tb := g.Boxes[p.from], g.Boxes[p.to]
		fs, ts := sides(fb, tb)
		start := portFor(p.edge, fb, fs, p.to)
		end := portFor(p.edge, tb, ts, p.from)
		bundle := bundles[p.edge]
		var cells []Point
		s0, e0 := outward(start, fs), outward(end, ts)
		if s0 == end || e0 == start || heuristic(start, end) <= 2 {
			// neighbours: no room for a run-up
			s0, e0 = start, end
		}
		if grid.isBox(s0.X, s0.Y) {
			s0 = start
		}
		if grid.isBox(e0.X, e0.Y) {
			e0 = end
		}
		if wps, ok := l.Waypoints[p.edge]; ok && len(wps) > 0 {
			cur := s0
			for _, wp := range wps {
				seg := grid.astar(cur, Point{wp[0], wp[1]}, bundle)
				if len(seg) == 0 {
					break
				}
				if len(cells) > 0 {
					seg = seg[1:]
				}
				cells = append(cells, seg...)
				cur = Point{wp[0], wp[1]}
			}
			seg := grid.astar(cur, e0, bundle)
			if len(cells) > 0 && len(seg) > 0 {
				seg = seg[1:]
			}
			cells = append(cells, seg...)
		} else {
			cells = grid.astar(s0, e0, bundle)
		}
		if len(cells) == 0 {
			cells = []Point{s0, e0}
		}
		if s0 != start {
			cells = append([]Point{start}, cells...)
		}
		if e0 != end {
			cells = append(cells, end)
		}
		grid.occupy(cells)
		if bundles[p.edge] == nil {
			bundles[p.edge] = map[int]bool{}
		}
		for _, c := range cells {
			if grid.in(c.X, c.Y) {
				bundles[p.edge][grid.idx(c.X, c.Y)] = true
			}
		}
		g.Routes[p.id] = &Route{ID: p.id, Edge: p.edge, From: p.from, To: p.to, Cells: cells, Both: p.both, Merged: p.merged}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// sides picks the side of each box facing the other.
func sides(a, b *Box) (side, side) {
	ax, ay := a.Center()
	bx, by := b.Center()
	switch {
	case b.Y >= a.Bottom():
		return sideBottom, sideTop
	case b.Bottom() <= a.Y:
		return sideTop, sideBottom
	case bx > ax:
		return sideRight, sideLeft
	default:
		_ = by
		_ = ay
		return sideLeft, sideRight
	}
}

// headerTextW is the width a node frame's header (label, state, gauges)
// needs; the frame is wider, and routes come down beside it.
const headerTextW = 24

type grid struct {
	w, h  int
	box   []bool // cell inside a box
	solid []bool // a node's header: never drawn over
	frame []bool // the border of a node frame: crossable at a cost
	group []bool // the border of a group frame: crossable at a smaller cost
	used  []int  // number of routes through the cell
	horiz []bool // a horizontal segment passes here
	vert  []bool // a vertical segment passes here
}

// newGrid marks the obstacles: every plain box, and for a node frame its
// header rows (solid) and its border (crossable), so routes can leave the
// instances inside and rarely cut through a machine.
func newGrid(g *Graph) *grid {
	gr := &grid{w: g.W + 4, h: g.H + 4}
	n := gr.w * gr.h
	gr.box = make([]bool, n)
	gr.frame = make([]bool, n)
	gr.group = make([]bool, n)
	gr.solid = make([]bool, n)
	gr.used = make([]int, n)
	gr.horiz = make([]bool, n)
	gr.vert = make([]bool, n)
	for _, b := range g.Boxes {
		for y := b.Y; y < b.Bottom(); y++ {
			for x := b.X; x < b.Right(); x++ {
				if !gr.in(x, y) {
					continue
				}
				i := gr.idx(x, y)
				if !b.Frame {
					gr.box[i] = true
					continue
				}
				border := y == b.Y || y == b.Bottom()-1 || x == b.X || x == b.Right()-1
				switch {
				case border:
					gr.frame[i] = true
				case y <= b.Y+b.Header && x >= b.X+1 && x < b.X+2+headerTextW:
					gr.solid[i] = true
				}
			}
		}
	}
	for _, f := range g.Groups {
		for y := f.Y; y <= f.Y+f.H; y++ {
			for x := f.X; x <= f.X+f.W; x++ {
				if gr.in(x, y) && (y == f.Y || y == f.Y+f.H || x == f.X || x == f.X+f.W) {
					gr.group[gr.idx(x, y)] = true
				}
			}
		}
	}
	return gr
}

func (g *grid) in(x, y int) bool    { return x >= 0 && y >= 0 && x < g.w && y < g.h }
func (g *grid) idx(x, y int) int    { return y*g.w + x }
func (g *grid) isBox(x, y int) bool { return g.in(x, y) && g.box[g.idx(x, y)] }

// port returns the cell just outside a box side, spread so several routes on
// one side do not share a cell.
func (g *grid) port(b *Box, s side, use map[string]map[side]int, other string) Point {
	if use[b.ID] == nil {
		use[b.ID] = map[side]int{}
	}
	n := use[b.ID][s]
	use[b.ID][s]++
	// alternate offsets: 0, +3, -3, +6, -6 ... so parallel routes keep a
	// cell of air between them
	off := 0
	if n > 0 {
		k := (n + 1) / 2 * 3
		if n%2 == 0 {
			k = -k
		}
		off = k
	}
	switch s {
	case sideTop:
		x := clamp(b.X+b.W/2+off, b.X+1, b.Right()-2)
		return Point{x, b.Y - 1}
	case sideBottom:
		x := clamp(b.X+b.W/2+off, b.X+1, b.Right()-2)
		return Point{x, b.Bottom()}
	case sideLeft:
		y := clamp(b.Y+b.H/2+off/2, b.Y+1, b.Bottom()-2)
		return Point{b.X - 1, y}
	default:
		y := clamp(b.Y+b.H/2+off/2, b.Y+1, b.Bottom()-2)
		return Point{b.Right(), y}
	}
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type node struct {
	p    Point
	dir  int // 0 none, 1 horizontal, 2 vertical
	cost int
	est  int
	idx  int
}

type pq []*node

func (q pq) Len() int { return len(q) }
func (q pq) Less(i, j int) bool {
	return q[i].est < q[j].est || (q[i].est == q[j].est && q[i].cost < q[j].cost)
}
func (q pq) Swap(i, j int) { q[i], q[j] = q[j], q[i]; q[i].idx = i; q[j].idx = j }
func (q *pq) Push(x any)   { n := x.(*node); n.idx = len(*q); *q = append(*q, n) }
func (q *pq) Pop() any     { old := *q; n := old[len(old)-1]; *q = old[:len(old)-1]; return n }

// astar finds an orthogonal path from a to b. Boxes and node headers are
// walls; a node frame's border and a group frame's border can be crossed at
// a cost. Cells in bundle (the routes of the same logical edge already
// drawn) are free to ride, so sibling routes join a trunk instead of
// running beside it. With no path at all the caller draws a straight line.
func (g *grid) astar(a, b Point, bundle map[int]bool) []Point {
	if !g.in(a.X, a.Y) || !g.in(b.X, b.Y) {
		return nil
	}
	type key struct {
		p   Point
		dir int
	}
	best := map[key]int{}
	prev := map[key]key{}
	open := &pq{}
	start := &node{p: a, dir: 0, cost: 0, est: heuristic(a, b)}
	heap.Push(open, start)
	best[key{a, 0}] = 0
	dirs := []struct{ dx, dy, d int }{{1, 0, 1}, {-1, 0, 1}, {0, 1, 2}, {0, -1, 2}}
	var goal key
	found := false
	steps := 0
	for open.Len() > 0 && steps < 200000 {
		steps++
		cur := heap.Pop(open).(*node)
		if cur.p == b {
			goal = key{cur.p, cur.dir}
			found = true
			break
		}
		ck := key{cur.p, cur.dir}
		if c, ok := best[ck]; ok && c < cur.cost {
			continue
		}
		for _, d := range dirs {
			np := Point{cur.p.X + d.dx, cur.p.Y + d.dy}
			if !g.in(np.X, np.Y) {
				continue
			}
			i := g.idx(np.X, np.Y)
			if (g.box[i] || g.solid[i]) && np != b {
				continue // never through a box or over a node's gauges
			}
			var c int
			switch {
			case bundle[i]:
				// riding the trunk of a sibling route is free
				c = cur.cost
			default:
				c = cur.cost + costCell
				if g.frame[i] && np != b {
					c += costFrame
				}
				if g.group[i] && np != b {
					c += costGroup
				}
				if g.used[i] > 0 && np != b {
					// crossing a perpendicular segment costs more than riding along
					if (d.d == 1 && g.vert[i]) || (d.d == 2 && g.horiz[i]) {
						c += costCross
					} else {
						c += costCross + costAdjacent
					}
				}
				if g.adjacentToRoute(np, d.d) && np != b {
					c += costAdjacent
				}
			}
			if cur.dir != 0 && cur.dir != d.d {
				c += costTurn
			}
			nk := key{np, d.d}
			if old, ok := best[nk]; ok && old <= c {
				continue
			}
			best[nk] = c
			prev[nk] = ck
			heap.Push(open, &node{p: np, dir: d.d, cost: c, est: c + heuristic(np, b)})
		}
	}
	if !found {
		return nil
	}
	var path []Point
	for k := goal; ; {
		path = append(path, k.p)
		pk, ok := prev[k]
		if !ok {
			break
		}
		k = pk
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

func heuristic(a, b Point) int { return abs(a.X-b.X) + abs(a.Y-b.Y) }

// adjacentToRoute reports whether a parallel route runs next to the cell.
func (g *grid) adjacentToRoute(p Point, dir int) bool {
	if dir == 1 { // moving horizontally: neighbors above/below with horizontal segments
		for _, dy := range []int{-1, 1} {
			if g.in(p.X, p.Y+dy) && g.horiz[g.idx(p.X, p.Y+dy)] {
				return true
			}
		}
	} else {
		for _, dx := range []int{-1, 1} {
			if g.in(p.X+dx, p.Y) && g.vert[g.idx(p.X+dx, p.Y)] {
				return true
			}
		}
	}
	return false
}

func (g *grid) occupy(cells []Point) {
	for i, p := range cells {
		if !g.in(p.X, p.Y) {
			continue
		}
		k := g.idx(p.X, p.Y)
		g.used[k]++
		if i > 0 {
			q := cells[i-1]
			if q.Y == p.Y {
				g.horiz[k] = true
				g.horiz[g.idx(q.X, q.Y)] = true
			} else {
				g.vert[k] = true
				g.vert[g.idx(q.X, q.Y)] = true
			}
		}
	}
}
