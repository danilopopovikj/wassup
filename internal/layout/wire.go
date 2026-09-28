package layout

import (
	"sort"
	"strings"

	"github.com/danilopopovikj/wassup/internal/model"
)

// Wires are drawn the way somebody would draw them by hand. What leaves one
// component is one net: a line beside the row of its copies that every copy
// taps, a trunk between the machines or a spine beside the side column, and
// a short last stretch into every box it reaches, which carries the
// arrowhead. Several boxes that feed one are a net as well, the other way
// around. A net takes a lane of its own wherever it runs, so two nets cross
// and never share a cell.
//
// The router of router.go finds its way around anything, and wanders while
// it does. It draws what does not fit the pattern: a box somebody dragged
// out of its band, an edge with waypoints.

// where names the room a lane lies in.
type where struct {
	kind byte // 'g' below a slot, 'c' above a band, 'v' beside a box of a band, 's' beside the side column
	a, b int
}

func (l where) key() string {
	switch l.kind {
	case 'g':
		return gapKey(l.a, l.b)
	case 'c':
		return chanKey(l.a)
	case 'v':
		return corridorKey(l.a, l.b)
	}
	return spineKey
}

// seg is a straight stretch of wire.
type seg struct{ a, b Point }

// tap is where a net meets a box.
type tap struct {
	box  *Box
	side side
	at   where
	port Point
	ok   bool
}

// wedge is a logical edge, with the edges drawn by the same line.
type wedge struct {
	id       string
	both     bool
	merged   []string
	from, to []string // box ids
	net      *wnet
	straight []Point
	legacy   bool
}

// wnet is what leaves one component, or what arrives at one.
type wnet struct {
	key   string
	edges []*wedge
	taps  map[string]*tap // by box id
	order []string        // box ids in the order they were tapped
	lane  map[where]int
	segs  []seg
}

type wirer struct {
	p    *plan
	g    *Graph
	room map[string]int
	use  map[string]int
	grid *grid
	// plain are the boxes a wire may not pass: everything but the instances,
	// which no trunk comes near.
	plain []*Box
}

// wire draws every edge and says how many lanes ran where. While the room
// does not fit, only the count is wanted and nothing is drawn, unless this
// is the last try.
func (p *plan) wire(g *Graph, room map[string]int, last bool) map[string]int {
	w := &wirer{p: p, g: g, room: room, use: map[string]int{}, grid: newGrid(g)}
	for _, id := range g.Order {
		if b := g.Boxes[id]; b.Instance == "" {
			w.plain = append(w.plain, b)
		}
	}
	edges := w.edges()
	nets := w.nets(edges)
	w.lanes(nets)
	for _, n := range nets {
		w.build(n)
	}
	for k, lanes := range w.use {
		if lanes > room[k] && !last {
			return w.use
		}
	}
	w.draw(edges, nets)
	return w.use
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

// edges lists what is to be drawn: opposite and parallel edges between the
// same boxes are one line.
func (w *wirer) edges() []*wedge {
	byEnds := map[string]*wedge{}
	var out []*wedge
	for _, e := range w.p.t.Edges {
		id := e.ID()
		from, to := w.g.endpoints(e.From), w.g.endpoints(e.To)
		if len(from) == 0 || len(to) == 0 {
			continue
		}
		f, t := strings.Join(from, ","), strings.Join(to, ",")
		if f == t {
			continue
		}
		if we, ok := byEnds[f+"|"+t]; ok {
			if !contains(we.merged, id) {
				we.merged = append(we.merged, id)
			}
			continue
		}
		if we, ok := byEnds[t+"|"+f]; ok {
			we.both = true
			if !contains(we.merged, id) {
				we.merged = append(we.merged, id)
			}
			continue
		}
		we := &wedge{id: id, from: from, to: to, merged: []string{id}}
		if wps, ok := w.p.l.Waypoints[id]; ok && len(wps) > 0 {
			we.legacy = true
		}
		byEnds[f+"|"+t] = we
		out = append(out, we)
	}
	return out
}

// level says how far down a box of the main area stands.
func (w *wirer) level(b *Box) (int, bool) {
	if b == nil || b.Lane == model.LaneSide {
		return 0, false
	}
	bi, ok := w.p.bandOf[b.ID]
	if !ok {
		return 0, false
	}
	return bi*1000 + w.p.slot[b.ID], true
}

// at says which room a tap of a box leads into.
func (w *wirer) at(b *Box, s side) (where, bool) {
	if s == sideLeft {
		return where{kind: 's'}, true
	}
	bi, ok := w.p.bandOf[b.ID]
	if !ok {
		return where{}, false
	}
	if b.Instance != "" {
		k := w.p.slot[b.ID]
		switch {
		case s == sideBottom:
			return where{'g', bi, k}, true
		case k > 0:
			return where{'g', bi, k - 1}, true
		}
		return where{'c', bi, 0}, true
	}
	if s == sideBottom {
		return where{'c', bi + 1, 0}, true
	}
	return where{'c', bi, 0}, true
}

// nets groups the edges. An edge belongs to what leaves its source, unless
// it is all that leaves it and its target is fed by others as well: then it
// belongs to what arrives at the target, and four names reach the load
// balancer through one arrow.
func (w *wirer) nets(edges []*wedge) []*wnet {
	out, in := map[string]int{}, map[string]int{}
	for _, e := range edges {
		if e.straight = w.straightLine(e); e.straight != nil || e.legacy {
			continue
		}
		out[strings.Join(e.from, ",")]++
		in[strings.Join(e.to, ",")]++
	}
	byKey := map[string]*wnet{}
	var nets []*wnet
	for _, e := range edges {
		if e.straight != nil || e.legacy {
			continue
		}
		f, t := strings.Join(e.from, ","), strings.Join(e.to, ",")
		src, dst := w.g.Boxes[e.from[0]], w.g.Boxes[e.to[0]]
		ls, sok := w.level(src)
		lt, tok := w.level(dst)
		// Down is the way things flow: a wire leaves a box at the bottom
		// and enters one at the top, unless the target stands higher.
		up := sok && tok && lt < ls
		level := sok && tok && lt == ls
		key := "out:" + f
		if out[f] == 1 && in[t] > 1 {
			key = "in:" + t
		}
		switch {
		case up:
			key += ":up"
		case level:
			key += ":level"
		}
		n := byKey[key]
		if n == nil {
			n = &wnet{key: key, taps: map[string]*tap{}, lane: map[where]int{}}
			byKey[key] = n
			nets = append(nets, n)
		}
		e.net = n
		n.edges = append(n.edges, e)
		add := func(ids []string, s side) {
			for _, id := range ids {
				b := w.g.Boxes[id]
				if _, seen := n.taps[id]; seen || b == nil {
					continue
				}
				side := s
				if b.Lane == model.LaneSide {
					side = sideLeft
				}
				at, ok := w.at(b, side)
				n.taps[id] = &tap{box: b, side: side, at: at, ok: ok}
				n.order = append(n.order, id)
			}
		}
		switch {
		case up:
			add(e.from, sideTop)
			add(e.to, sideBottom)
		case level:
			// side by side, with something between them: over the top
			add(e.from, sideTop)
			add(e.to, sideTop)
		default:
			add(e.from, sideBottom)
			add(e.to, sideTop)
		}
	}
	sort.SliceStable(nets, func(i, j int) bool { return nets[i].key < nets[j].key })
	return nets
}

// lanes gives every net its lane in every room it taps, and every tap its
// place on the side of its box. Within the room between two rows, a net
// that only reaches up lies on top and one that only reaches down at the
// bottom, so that the taps of two nets do not run into each other.
func (w *wirer) lanes(nets []*wnet) {
	type want struct {
		net      *wnet
		up, down int
	}
	rooms := map[where][]*want{}
	var order []where
	for _, n := range nets {
		seen := map[where]*want{}
		for _, id := range n.order {
			t := n.taps[id]
			if !t.ok {
				continue
			}
			wt := seen[t.at]
			if wt == nil {
				wt = &want{net: n}
				seen[t.at] = wt
				if len(rooms[t.at]) == 0 {
					order = append(order, t.at)
				}
				rooms[t.at] = append(rooms[t.at], wt)
			}
			if t.side == sideBottom {
				wt.up++ // the box is above the lane
			} else {
				wt.down++
			}
		}
	}
	for _, at := range order {
		wants := rooms[at]
		sort.SliceStable(wants, func(i, j int) bool {
			a, b := wants[i], wants[j]
			if at.kind == 's' {
				// the spine with the most boxes to reach stands next to them
				if a.down != b.down {
					return a.down > b.down
				}
				return a.net.key < b.net.key
			}
			class := func(x *want) int {
				switch {
				case x.down == 0:
					return 0
				case x.up == 0:
					return 2
				}
				return 1
			}
			if class(a) != class(b) {
				return class(a) < class(b)
			}
			return a.net.key < b.net.key
		})
		for i, wt := range wants {
			wt.net.lane[at] = i
		}
		w.use[at.key()] = len(wants)
	}
	ports := map[string]int{}
	for _, n := range nets {
		for _, id := range n.order {
			t := n.taps[id]
			k := id + "|" + string(rune('0'+int(t.side)))
			t.port = port(t.box, t.side, ports[k])
			ports[k]++
		}
	}
}

// alt spreads around a middle: 0, 1, -1, 2, -2.
func alt(i int) int {
	if i%2 == 1 {
		return (i + 1) / 2
	}
	return -i / 2
}

// port is the cell just outside a box where its i-th wire on a side begins.
// An instance is entered at the top right, beside the header of its machine,
// and left at the bottom left, so that what enters the instance below does
// not meet what leaves the one above.
func port(b *Box, s side, i int) Point {
	switch s {
	case sideLeft:
		return Point{b.X - 1, clamp(b.Y+b.H/2+alt(i), b.Y+1, b.Bottom()-2)}
	case sideTop:
		if b.Instance != "" {
			return Point{clamp(b.Right()-4-3*i, b.X+1, b.Right()-2), b.Y - 1}
		}
		return Point{clamp(b.X+b.W/2+3*alt(i), b.X+1, b.Right()-2), b.Y - 1}
	}
	if b.Instance != "" {
		return Point{clamp(b.X+3+3*i, b.X+1, b.Right()-2), b.Bottom()}
	}
	return Point{clamp(b.X+b.W/2+3*alt(i), b.X+1, b.Right()-2), b.Bottom()}
}

// laneY is the row of a lane.
func (w *wirer) laneY(at where, i int) int {
	if at.kind == 'g' {
		b := w.p.bands[at.a]
		return b.y + b.gapY[at.b] + 1 + i
	}
	return w.p.chanY[at.a] + i
}

// spineX is the column of a spine.
func (w *wirer) spineX(i int) int {
	n := max(w.use[spineKey], w.p.spines)
	return w.p.spineX + 2*(n-1-i)
}

// build lays the lines of a net: a lane wherever it taps a box, a trunk
// between the boxes that joins the lanes, and a spine beside the side column
// for the boxes it reaches there. The spine leaves from the lane of the
// component the net belongs to, so what goes out to the side goes right and
// what goes down to the data goes down.
func (w *wirer) build(n *wnet) {
	n.segs = nil
	lanes := map[int][]int{} // row of a lane -> the columns it has to reach
	var rows []int           // the rows a spine has to reach
	sumX, taps, first := 0, 0, -1
	for _, id := range n.order {
		t := n.taps[id]
		if !t.ok || t.side == sideLeft {
			continue
		}
		y := w.laneY(t.at, n.lane[t.at])
		if (t.side == sideBottom && y < t.port.Y) || (t.side == sideTop && y > t.port.Y) {
			t.ok = false // the box is not where its band is
			continue
		}
		n.segs = append(n.segs, seg{t.port, Point{t.port.X, y}})
		lanes[y] = append(lanes[y], t.port.X)
		sumX += t.port.X
		taps++
		if first < 0 {
			first = y
		}
	}
	ys := make([]int, 0, len(lanes))
	for y := range lanes {
		ys = append(ys, y)
	}
	sort.Ints(ys)
	if len(ys) > 1 {
		for _, tr := range w.trunk(n, ys[0], ys[len(ys)-1], sumX/taps) {
			n.segs = append(n.segs, tr)
			if tr.a.X != tr.b.X {
				continue
			}
			for _, y := range ys {
				if y >= tr.a.Y && y <= tr.b.Y {
					lanes[y] = append(lanes[y], tr.a.X)
				}
			}
		}
	}
	if at := (where{kind: 's'}); w.tapsSide(n) {
		sx := w.spineX(n.lane[at])
		for _, id := range n.order {
			if t := n.taps[id]; t.ok && t.side == sideLeft {
				if t.port.X < sx {
					t.ok = false
					continue
				}
				n.segs = append(n.segs, seg{Point{sx, t.port.Y}, t.port})
				rows = append(rows, t.port.Y)
			}
		}
		if first >= 0 {
			lanes[first] = append(lanes[first], sx)
			rows = append(rows, first)
		}
		if len(rows) > 0 {
			sort.Ints(rows)
			n.segs = append(n.segs, seg{Point{sx, rows[0]}, Point{sx, rows[len(rows)-1]}})
		}
	}
	for _, y := range ys {
		xs := lanes[y]
		sort.Ints(xs)
		n.segs = append(n.segs, seg{Point{xs[0], y}, Point{xs[len(xs)-1], y}})
	}
}

// tapsSide reports whether a net reaches a box of the side column.
func (w *wirer) tapsSide(n *wnet) bool {
	for _, id := range n.order {
		if t := n.taps[id]; t.ok && t.side == sideLeft {
			return true
		}
	}
	return false
}

// trunk joins the lanes of a net that lie in different rooms. From one band
// to another it runs down the street, which is at the same place in every
// band, so it is one straight line. Within a band, or where a box stands on
// the street, it runs between the boxes of every band it passes, as near to
// the middle of what the net reaches as there is room, and straight on
// through the next band where nothing is in the way.
func (w *wirer) trunk(n *wnet, top, bottom, ref int) []seg {
	var passed []int
	for bi, b := range w.p.bands {
		if max(top, b.y-1) <= min(bottom, b.y+b.h) {
			passed = append(passed, bi)
		}
	}
	if len(passed) > 1 {
		if x := w.p.streetX + 2 + 2*w.use[streetKey]; w.clear(x, top, bottom) {
			w.use[streetKey]++
			return []seg{{Point{x, top}, Point{x, bottom}}}
		}
	}
	var out []seg
	x, y, have := ref, top, false
	for _, bi := range passed {
		b := w.p.bands[bi]
		a, z := max(top, b.y-1), min(bottom, b.y+b.h)
		if have && w.clear(x, a, z) {
			continue
		}
		nx := w.corridor(bi, x, a, z)
		if have && nx != x {
			// over to the new column, in the channel above the band
			at := where{'c', bi, 0}
			i, ok := n.lane[at]
			if !ok {
				i = w.use[at.key()]
				w.use[at.key()]++
				n.lane[at] = i
			}
			jy := w.laneY(at, i)
			if jy < y {
				jy = y
			}
			out = append(out, seg{Point{x, y}, Point{x, jy}}, seg{Point{x, jy}, Point{nx, jy}})
			y = jy
		}
		x, have = nx, true
	}
	return append(out, seg{Point{x, y}, Point{x, bottom}})
}

// clear reports whether a line down column x from row a to row z passes no
// box and rides along no frame.
func (w *wirer) clear(x, a, z int) bool {
	for _, b := range w.plain {
		if x >= b.X-1 && x <= b.Right() && a < b.Bottom() && z >= b.Y {
			return false
		}
	}
	along := 0
	for y := a; y <= z; y++ {
		if w.grid.in(x, y) && w.grid.group[w.grid.idx(x, y)] {
			along++
		}
	}
	return along <= 2
}

// corridor picks the column between two boxes of a band, or beside the band,
// that is nearest to ref and free from row a to row z, and takes a lane in
// it.
func (w *wirer) corridor(bi, ref, a, z int) int {
	var boxes []*Box
	for _, idx := range w.p.bands[bi].items {
		if b := w.g.Boxes[w.p.items[idx].id]; b != nil {
			boxes = append(boxes, b)
		}
	}
	sort.SliceStable(boxes, func(i, j int) bool { return boxes[i].X < boxes[j].X })
	if len(boxes) == 0 {
		return ref
	}
	best, bestX, bestD := -1, ref, 1<<30
	for j := 0; j <= len(boxes); j++ {
		i := w.use[w.p.roomKey(bi, j)]
		x, ok := 0, false
		switch {
		case j > 0 && j < len(boxes):
			// Between two boxes the next lane is taken whether it fits or
			// not: what does not fit says how much room the next try needs.
			x, ok = boxes[j-1].Right()+2+2*i, true
		default:
			// beside the band, the first column that is free
			for try := 0; try < 8 && !ok; try++ {
				if j == 0 {
					x = boxes[0].X - 3 - 2*(i+try)
				} else {
					x = boxes[j-1].Right() + 2 + 2*(i+try)
				}
				ok = x >= 1 && w.clear(x, a, z)
			}
		}
		if !ok {
			continue
		}
		if d := abs(x - ref); d < bestD {
			best, bestX, bestD = j, x, d
		}
	}
	if best < 0 {
		return ref
	}
	w.use[w.p.roomKey(bi, best)]++
	return bestX
}

// straightLine is the wire between two boxes that stand side by side with
// nothing between them: the primary and its replica on the next machine.
func (w *wirer) straightLine(e *wedge) []Point {
	if len(e.from) != 1 || len(e.to) != 1 || e.legacy {
		return nil
	}
	a, b := w.g.Boxes[e.from[0]], w.g.Boxes[e.to[0]]
	if a == nil || b == nil || a.Frame || b.Frame {
		return nil
	}
	top, bottom := max(a.Y, b.Y)+1, min(a.Bottom(), b.Bottom())-2
	if top > bottom {
		return nil
	}
	y := (top + bottom) / 2
	var cells []Point
	switch {
	case a.Right() < b.X:
		for x := a.Right(); x < b.X; x++ {
			cells = append(cells, Point{x, y})
		}
	case b.Right() < a.X:
		for x := a.X - 1; x >= b.Right(); x-- {
			cells = append(cells, Point{x, y})
		}
	}
	if len(cells) < 2 || !w.free(cells) {
		return nil
	}
	return cells
}

// free reports whether a wire along the cells passes through no box and
// over no header of a machine.
func (w *wirer) free(cells []Point) bool {
	for _, c := range cells {
		if !w.grid.in(c.X, c.Y) {
			return false
		}
		if i := w.grid.idx(c.X, c.Y); w.grid.box[i] || w.grid.solid[i] {
			return false
		}
	}
	return true
}

// cellsOf lists the cells of a stretch, both ends included.
func cellsOf(s seg) []Point {
	dx, dy := sign(s.b.X-s.a.X), sign(s.b.Y-s.a.Y)
	out := []Point{s.a}
	for c := s.a; c != s.b; {
		c = Point{c.X + dx, c.Y + dy}
		out = append(out, c)
		if len(out) > 1<<14 {
			break
		}
	}
	return out
}

func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// links joins the cells of a net along its stretches.
func (n *wnet) links() map[Point][]Point {
	adj := map[Point][]Point{}
	link := func(a, b Point) {
		for _, c := range adj[a] {
			if c == b {
				return
			}
		}
		adj[a] = append(adj[a], b)
		adj[b] = append(adj[b], a)
	}
	for _, s := range n.segs {
		if s.a.X != s.b.X && s.a.Y != s.b.Y {
			continue
		}
		cells := cellsOf(s)
		for i := 1; i < len(cells); i++ {
			link(cells[i-1], cells[i])
		}
	}
	return adj
}

// path walks a net from one tap to another.
func path(adj map[Point][]Point, from, to Point) []Point {
	if _, ok := adj[from]; !ok {
		return nil
	}
	prev := map[Point]Point{from: from}
	queue := []Point{from}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == to {
			var out []Point
			for ; c != from; c = prev[c] {
				out = append(out, c)
			}
			out = append(out, from)
			for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
				out[i], out[j] = out[j], out[i]
			}
			return out
		}
		for _, nb := range adj[c] {
			if _, seen := prev[nb]; !seen {
				prev[nb] = c
				queue = append(queue, nb)
			}
		}
	}
	return nil
}

// draw turns the nets into routes, one per pair of boxes an edge joins, and
// hands what no net could draw to the router.
func (w *wirer) draw(edges []*wedge, nets []*wnet) {
	g := w.g
	type pending struct {
		e        *wedge
		from, to string
		dist     int
	}
	routeID := func(e *wedge, from, to string) string {
		if g.Boxes[from].Instance != "" || g.Boxes[to].Instance != "" {
			return e.id + "@" + from + ">" + to
		}
		return e.id
	}
	add := func(e *wedge, from, to string, cells []Point, net string) {
		id := routeID(e, from, to)
		g.Routes[id] = &Route{ID: id, Edge: e.id, From: from, To: to, Cells: cells, Both: e.both, Merged: e.merged, Net: net}
		w.grid.occupy(cells)
		for _, c := range cells {
			if c.X+2 > g.W {
				g.W = c.X + 2
			}
			if c.Y+2 > g.H {
				g.H = c.Y + 2
			}
		}
	}
	var rest []pending
	adj := map[*wnet]map[Point][]Point{}
	for _, n := range nets {
		adj[n] = n.links()
	}
	for _, e := range edges {
		if e.straight != nil {
			add(e, e.from[0], e.to[0], e.straight, "")
			continue
		}
		for _, f := range e.from {
			for _, t := range e.to {
				if f == t {
					continue
				}
				var cells []Point
				if e.net != nil {
					if a, b := e.net.taps[f], e.net.taps[t]; a != nil && b != nil && a.ok && b.ok {
						cells = path(adj[e.net], a.port, b.port)
					}
				}
				if len(cells) >= 2 && w.free(cells) {
					add(e, f, t, cells, e.net.key)
					continue
				}
				fx, fy := g.Boxes[f].Center()
				tx, ty := g.Boxes[t].Center()
				rest = append(rest, pending{e, f, t, abs(fx-tx) + abs(fy-ty)})
			}
		}
	}

	// What is left is found by the router, the shortest first. The routes of
	// one edge share their trunk, and ride the net of the edge where it has
	// one.
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].dist != rest[j].dist {
			return rest[i].dist < rest[j].dist
		}
		return routeID(rest[i].e, rest[i].from, rest[i].to) < routeID(rest[j].e, rest[j].from, rest[j].to)
	})
	portUse := map[string]map[side]int{}
	shared := map[string]Point{}
	portFor := func(edge string, b *Box, s side, other string) Point {
		k := edge + "|" + b.ID + "|" + string(rune('0'+int(s)))
		if pt, ok := shared[k]; ok {
			return pt
		}
		pt := w.grid.port(b, s, portUse, other)
		shared[k] = pt
		return pt
	}
	bundles := map[string]map[int]bool{}
	for _, pd := range rest {
		e := pd.e
		if bundles[e.id] == nil {
			bundles[e.id] = map[int]bool{}
			if e.net != nil {
				for c := range adj[e.net] {
					if w.grid.in(c.X, c.Y) {
						bundles[e.id][w.grid.idx(c.X, c.Y)] = true
					}
				}
			}
		}
		cells := w.grid.route(g.Boxes[pd.from], g.Boxes[pd.to], w.p.l.Waypoints[e.id], bundles[e.id], func(b *Box, s side, other string) Point {
			return portFor(e.id, b, s, other)
		})
		for _, c := range cells {
			if w.grid.in(c.X, c.Y) {
				bundles[e.id][w.grid.idx(c.X, c.Y)] = true
			}
		}
		add(e, pd.from, pd.to, cells, "")
	}
}
