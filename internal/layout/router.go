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
	costAdjacent = 4
	costBox      = 20
	costTurn     = 1
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
	from, to string
	both     bool
	merged   []string
	dist     int
}

// routeAll routes every edge, shortest first, merging opposite pairs.
func routeAll(t *model.Topology, g *Graph, l model.Layout) {
	seen := map[string]bool{}
	var list []pending
	for _, e := range t.Edges {
		id := e.ID()
		if seen[id] {
			continue
		}
		from, to := g.BoxOf[e.From], g.BoxOf[e.To]
		if from == "" || to == "" || from == to || g.Boxes[from] == nil || g.Boxes[to] == nil {
			seen[id] = true
			continue
		}
		p := pending{id: id, from: from, to: to, merged: []string{id}}
		rev := model.EdgeID(e.To, e.From)
		if _, ok := t.Edge(rev); ok && !seen[rev] {
			p.both = true
			p.merged = append(p.merged, rev)
			seen[rev] = true
		}
		// merge parallel edges between the same pair of boxes
		for _, o := range t.Edges {
			oid := o.ID()
			if oid == id || seen[oid] {
				continue
			}
			if g.BoxOf[o.From] == from && g.BoxOf[o.To] == to {
				p.merged = append(p.merged, oid)
				seen[oid] = true
			}
		}
		seen[id] = true
		fb, tb := g.Boxes[from], g.Boxes[to]
		fx, fy := fb.Center()
		tx, ty := tb.Center()
		p.dist = abs(fx-tx) + abs(fy-ty)
		list = append(list, p)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].dist != list[j].dist {
			return list[i].dist < list[j].dist
		}
		return list[i].id < list[j].id
	})
	grid := newGrid(g)
	// Count how many routes leave/enter each box side to spread ports.
	portUse := map[string]map[side]int{}
	for _, p := range list {
		fb, tb := g.Boxes[p.from], g.Boxes[p.to]
		fs, ts := sides(fb, tb)
		start := grid.port(fb, fs, portUse, p.to)
		end := grid.port(tb, ts, portUse, p.from)
		var cells []Point
		if wps, ok := l.Waypoints[p.id]; ok && len(wps) > 0 {
			cur := start
			for _, wp := range wps {
				seg := grid.astar(cur, Point{wp[0], wp[1]}, fb, tb)
				if len(seg) == 0 {
					break
				}
				if len(cells) > 0 {
					seg = seg[1:]
				}
				cells = append(cells, seg...)
				cur = Point{wp[0], wp[1]}
			}
			seg := grid.astar(cur, end, fb, tb)
			if len(cells) > 0 && len(seg) > 0 {
				seg = seg[1:]
			}
			cells = append(cells, seg...)
		} else {
			cells = grid.astar(start, end, fb, tb)
		}
		if len(cells) == 0 {
			cells = []Point{start, end}
		}
		grid.occupy(cells)
		g.Routes[p.id] = &Route{ID: p.id, From: p.from, To: p.to, Cells: cells, Both: p.both, Merged: p.merged}
	}
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

type grid struct {
	w, h  int
	box   []bool // cell inside a box
	used  []int  // number of routes through the cell
	horiz []bool // a horizontal segment passes here
	vert  []bool // a vertical segment passes here
}

func newGrid(g *Graph) *grid {
	gr := &grid{w: g.W + 4, h: g.H + 4}
	n := gr.w * gr.h
	gr.box = make([]bool, n)
	gr.used = make([]int, n)
	gr.horiz = make([]bool, n)
	gr.vert = make([]bool, n)
	for _, b := range g.Boxes {
		for y := b.Y; y < b.Bottom(); y++ {
			for x := b.X; x < b.Right(); x++ {
				if gr.in(x, y) {
					gr.box[gr.idx(x, y)] = true
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
	// alternate offsets: 0, +2, -2, +4, -4 ...
	off := 0
	if n > 0 {
		k := (n + 1) / 2 * 2
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

// astar finds an orthogonal path from a to b avoiding boxes (other than the
// two endpoints' own boxes, whose border cells the ports touch).
func (g *grid) astar(a, b Point, fb, tb *Box) []Point {
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
			c := cur.cost + costCell
			i := g.idx(np.X, np.Y)
			if g.box[i] && np != b {
				c += costBox
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
