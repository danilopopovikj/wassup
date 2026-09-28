package layout

import (
	"fmt"
	"sort"

	"github.com/danilopopovikj/wassup/internal/model"
)

// item is a box of the main area or of the side column before it has a
// place: a component, or a group folded into one box.
type item struct {
	id    string
	lane  model.Lane
	group string
	comp  model.Component
	gbox  bool
	mem   []string
	// order is where the topology lists it.
	order int
}

// band is one row of the main area. The way in stands in one band per step
// (names above the load balancer they point at), the machines that run the
// application in the next, then what runs on no machine, then what holds
// data.
type band struct {
	lane  model.Lane
	items []int // indices into plan.items, left to right
	// slots is how many instances the frames of this band stack. The same
	// component stands in the same slot on every machine, so one line
	// beside the row reaches all its copies.
	slots int
	// Set by place.
	y, h  int
	slotY []int // top of slot k, from the top of a frame
	gapY  []int // first row below slot k, from the top of a frame
	// split is how many boxes stand left of the street; -1 when the band
	// stands on it, which a single box of the way in does.
	split int
}

// plan is what a layout knows before anything has a coordinate: which boxes
// there are, in which band and in which order.
type plan struct {
	t    *model.Topology
	l    model.Layout
	opts Options

	groups   []model.Group
	parentOf map[string]string
	maxDepth int

	items     []item
	hostsOf   map[string][]model.Component // node id -> what it draws
	instances map[string][]string
	boxOf     map[string]string
	bands     []*band
	side      []int
	// sideFramed are the groups framed in the side column.
	sideFramed map[string]bool
	// slot is the slot of an instance box, bandOf the band of a box of the
	// main area; an instance is in the band of its node.
	slot   map[string]int
	bandOf map[string]int

	// Set by place.
	chanY  []int // first lane row of the channel above band i; the last one is below the last band
	spineX int   // the column of the first spine
	spines int
	// streetX is the first column of the street: the room that runs down
	// the middle of the picture, through every band at the same place, so
	// that a wire from one band to another goes straight down.
	streetX int
}

// Room for wires is counted in lanes and kept by where they run.
func gapKey(band, slot int) string     { return fmt.Sprintf("gap/%d/%d", band, slot) }
func chanKey(i int) string             { return fmt.Sprintf("ch/%d", i) }
func corridorKey(band, i int) string   { return fmt.Sprintf("cor/%d/%d", band, i) }
func (p *plan) depthOf(gid string) int { return depth(p.parentOf, gid) }

const (
	spineKey  = "side"
	streetKey = "street"
)

// roomKey names the room beside box i of a band: the street where the band
// is split, a corridor of its own anywhere else.
func (p *plan) roomKey(band, i int) string {
	if p.bands[band].split == i {
		return streetKey
	}
	return corridorKey(band, i)
}

func depth(parentOf map[string]string, gid string) int {
	d := 0
	for q := parentOf[gid]; q != ""; q = parentOf[q] {
		d++
	}
	return d
}

// newPlan decides what is drawn and in which order.
func newPlan(t *model.Topology, l model.Layout, opts Options) *plan {
	p := &plan{t: t, l: l, opts: opts, parentOf: map[string]string{}, hostsOf: map[string][]model.Component{},
		instances: map[string][]string{}, boxOf: map[string]string{}, slot: map[string]int{}, bandOf: map[string]int{}}

	// 1. Which components are drawn as boxes: everything not inside a
	// collapsed group, plus one box per collapsed group. A db with roles is
	// drawn through its instances.
	p.groups = t.AllGroups()
	for _, gr := range p.groups {
		p.parentOf[gr.ID] = gr.Parent
	}
	for _, gr := range p.groups {
		if d := p.depthOf(gr.ID); d > p.maxDepth {
			p.maxDepth = d
		}
	}
	collapsedAncestor := func(groupID string) string {
		for gid := groupID; gid != ""; gid = p.parentOf[gid] {
			if opts.Collapsed[gid] {
				top := gid
				// walk up further: an outer collapsed group wins
				for q := p.parentOf[gid]; q != ""; q = p.parentOf[q] {
					if opts.Collapsed[q] {
						top = q
					}
				}
				return top
			}
		}
		return ""
	}
	order := map[string]int{}
	for i, c := range t.AllComponents() {
		if _, ok := order[c.ID]; !ok {
			order[c.ID] = i
		}
	}
	seenGroupBox := map[string]bool{}
	// A component that runs on nodes is drawn inside them, one instance per
	// node, when neither it nor the node is folded into a collapsed group.
	nodesOf := map[string][]string{} // component id -> its nodes
	roleNode, pinned := roleNodes(t, collapsedAncestor)
	for _, c := range t.AllComponents() {
		if pinned[c.ID] {
			continue // drawn through its instances, each inside its node
		}
		runsOn := c.RunsOn
		if n, ok := roleNode[c.ID]; ok {
			runsOn = []string{n}
		}
		if c.Type == "node" || len(runsOn) == 0 || collapsedAncestor(c.Group) != "" {
			continue
		}
		for _, n := range runsOn {
			nc, ok := t.Component(n)
			if !ok || nc.Type != "node" || collapsedAncestor(nc.Group) != "" {
				continue
			}
			nodesOf[c.ID] = append(nodesOf[c.ID], n)
			p.hostsOf[n] = append(p.hostsOf[n], c)
		}
	}
	for _, c := range t.AllComponents() {
		if c.Type == "database" && c.Roles != nil && c.Roles.Primary != "" {
			p.boxOf[c.ID] = c.Roles.Primary
			if n, ok := roleNode[c.Roles.Primary]; ok {
				p.boxOf[c.ID] = c.Roles.Primary + "@" + n
			}
			continue // drawn as a group of instances
		}
		if nodes := nodesOf[c.ID]; len(nodes) > 0 {
			for _, n := range nodes {
				p.instances[c.ID] = append(p.instances[c.ID], c.ID+"@"+n)
			}
			p.boxOf[c.ID] = p.instances[c.ID][0]
			continue // drawn inside its nodes
		}
		if ca := collapsedAncestor(c.Group); ca != "" {
			p.boxOf[c.ID] = ca
			if !seenGroupBox[ca] {
				seenGroupBox[ca] = true
				p.items = append(p.items, item{id: ca, lane: model.LaneOf(c), group: p.parentOf[ca], gbox: true, order: order[c.ID]})
			}
			for i := range p.items {
				if p.items[i].id == ca {
					p.items[i].mem = append(p.items[i].mem, c.ID)
				}
			}
			continue
		}
		p.boxOf[c.ID] = c.ID
		p.items = append(p.items, item{id: c.ID, lane: model.LaneOf(c), group: c.Group, comp: c, order: order[c.ID]})
	}

	// 2. Order within lanes by barycenter, keeping group members adjacent.
	items := p.items
	byLane := map[model.Lane][]int{}
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
				// a component inside nodes pulls toward those nodes
				targets := nodesOf[n]
				if len(targets) == 0 {
					targets = []string{p.boxOf[n]}
				}
				for _, b := range targets {
					if b == "" || b == id || seen[b] {
						continue
					}
					seen[b] = true
					out = append(out, b)
				}
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
	main := []model.Lane{model.LaneEdge, model.LaneMachines, model.LaneCompute, model.LaneData}
	// The machines row keeps the topology's order (node-1, node-2, ...): a
	// machine is where it is, not where its traffic pulls it. For the
	// barycenter the row is transparent: the edge and compute lanes still
	// order themselves by each other.
	adjacent := func(i, step int) func(model.Lane) bool {
		set := map[model.Lane]bool{}
		for j := i + step; j >= 0 && j < len(main); j += step {
			set[main[j]] = true
			if main[j] != model.LaneMachines {
				break
			}
		}
		return func(l model.Lane) bool { return set[l] }
	}
	for sweepN := 0; sweepN < 2; sweepN++ {
		for i := 1; i < len(main); i++ {
			if main[i] != model.LaneMachines {
				sweep(main[i], adjacent(i, -1))
			}
		}
		for i := len(main) - 2; i >= 0; i-- {
			if main[i] != model.LaneMachines {
				sweep(main[i], adjacent(i, +1))
			}
		}
	}

	// What belongs to one group stands together, and so does what belongs
	// to the groups inside it: a frame then holds its own boxes and no
	// others. The groups keep the order the sweeps gave their first box.
	pathOf := func(it item) []string {
		var path []string
		for gid := it.group; gid != ""; gid = p.parentOf[gid] {
			path = append([]string{gid}, path...)
		}
		return path
	}
	var together func(idxs []int, depth int) []int
	together = func(idxs []int, depth int) []int {
		if len(idxs) < 2 {
			return idxs
		}
		parts := map[string][]int{}
		var order []string
		deeper := false
		for _, idx := range idxs {
			key := ""
			if path := pathOf(items[idx]); depth < len(path) {
				key, deeper = path[depth], true
			}
			if len(parts[key]) == 0 {
				order = append(order, key)
			}
			parts[key] = append(parts[key], idx)
		}
		if !deeper {
			return idxs
		}
		var out []int
		for _, key := range order {
			if key == "" {
				out = append(out, parts[key]...)
				continue
			}
			out = append(out, together(parts[key], depth+1)...)
		}
		return out
	}
	for _, ln := range []model.Lane{model.LaneEdge, model.LaneCompute, model.LaneData} {
		byLane[ln] = together(byLane[ln], 0)
	}

	// 3. The bands. The way in is one band per step, so that a name stands
	// above the load balancer it points at and the arrow between them is
	// short and straight.
	rank := map[string]int{}
	for range byLane[model.LaneEdge] {
		for _, e := range t.Edges {
			a, b := p.boxOf[e.From], p.boxOf[e.To]
			ai, aok := itemByID[a]
			bi, bok := itemByID[b]
			if !aok || !bok || a == b || items[ai].lane != model.LaneEdge || items[bi].lane != model.LaneEdge {
				continue
			}
			if rank[a]+1 > rank[b] && rank[a]+1 < len(byLane[model.LaneEdge]) {
				rank[b] = rank[a] + 1
			}
		}
	}
	steps := 0
	for _, idx := range byLane[model.LaneEdge] {
		if r := rank[items[idx].id] + 1; r > steps {
			steps = r
		}
	}
	for s := 0; s < steps; s++ {
		b := &band{lane: model.LaneEdge}
		for _, idx := range byLane[model.LaneEdge] {
			if rank[items[idx].id] == s {
				b.items = append(b.items, idx)
			}
		}
		p.bands = append(p.bands, b)
	}
	// A machine that holds nothing but data stands at the bottom, with the
	// data; every other machine in the middle.
	holdsData := func(it item) bool {
		hosts := p.hostsOf[it.id]
		if it.gbox || it.comp.Type != "node" || len(hosts) == 0 {
			return false
		}
		for _, hc := range hosts {
			if model.LaneOf(hc) != model.LaneData {
				return false
			}
		}
		return true
	}
	machines, data := &band{lane: model.LaneMachines}, &band{lane: model.LaneData}
	for _, idx := range byLane[model.LaneMachines] {
		if holdsData(items[idx]) {
			data.items = append(data.items, idx)
		} else {
			machines.items = append(machines.items, idx)
		}
	}
	data.items = append(data.items, byLane[model.LaneData]...)
	for _, b := range []*band{machines, {lane: model.LaneCompute, items: byLane[model.LaneCompute]}, data} {
		if len(b.items) > 0 {
			p.bands = append(p.bands, b)
		}
	}
	for bi, b := range p.bands {
		for _, idx := range b.items {
			p.bandOf[items[idx].id] = bi
		}
		p.slots(bi, b, order)
	}

	// 4. The side column: what is outside the system. The services of
	// others come first, what watches the system next, then the rest. A
	// group that stands on the side as a whole is framed there, as is one
	// folded into a box, and comes after the boxes of its kind that belong to
	// none. Only a group of its own is framed: one that is part of another
	// (a namespace of the cluster) would be framed away from the frame it
	// belongs in.
	sideOnly := map[string]bool{}
	for _, it := range items {
		if it.group == "" || it.gbox {
			continue
		}
		if _, seen := sideOnly[it.group]; !seen {
			sideOnly[it.group] = true
		}
		sideOnly[it.group] = sideOnly[it.group] && it.lane == model.LaneSide
	}
	for _, c := range t.AllComponents() {
		// a member drawn inside a machine, or through its instances
		if p.boxOf[c.ID] != c.ID {
			sideOnly[c.Group] = false
		}
	}
	for _, gr := range p.groups {
		sideOnly[gr.Parent] = false // a group of groups is framed by what it holds
	}
	p.sideFramed = map[string]bool{}
	for gid, only := range sideOnly {
		p.sideFramed[gid] = only && gid != "" && p.parentOf[gid] == ""
	}
	kind := func(it item) int {
		switch {
		case it.gbox:
			return 3
		case it.comp.Type == "external":
			return 0
		case it.comp.Type == "observability":
			return 1
		}
		return 2
	}
	// A framed group stands where the first of its boxes would, and its boxes
	// stay together.
	type place struct{ framed, kind, first, order int }
	groupKind, groupFirst := map[string]int{}, map[string]int{}
	for _, idx := range byLane[model.LaneSide] {
		it := items[idx]
		if !p.sideFramed[it.group] || it.gbox {
			continue
		}
		if k, ok := groupKind[it.group]; !ok || kind(it) < k {
			groupKind[it.group] = kind(it)
		}
		if f, ok := groupFirst[it.group]; !ok || it.order < f {
			groupFirst[it.group] = it.order
		}
	}
	placeOf := func(it item) place {
		if p.sideFramed[it.group] && !it.gbox {
			return place{1, groupKind[it.group], groupFirst[it.group], it.order}
		}
		if it.gbox {
			return place{2, kind(it), it.order, it.order}
		}
		return place{0, kind(it), it.order, it.order}
	}
	p.side = append(p.side, byLane[model.LaneSide]...)
	sort.SliceStable(p.side, func(x, y int) bool {
		a, b := placeOf(items[p.side[x]]), placeOf(items[p.side[y]])
		// the services of others lead, framed or not; after them what is
		// framed or folded reads last
		la, lb := a.framed, b.framed
		if a.kind == 0 && a.framed < 2 {
			la = -1
		}
		if b.kind == 0 && b.framed < 2 {
			lb = -1
		}
		switch {
		case la != lb:
			return la < lb
		case a.kind != b.kind:
			return a.kind < b.kind
		case a.first != b.first:
			return a.first < b.first
		}
		return a.order < b.order
	})
	return p
}

// slots gives every instance of a band its slot. The residents of a machine
// stack in the order of the lanes (what takes requests on top, what holds
// data at the bottom), then in the order of the topology; a component takes
// the first slot that is free on every machine it runs on.
func (p *plan) slots(bi int, b *band, order map[string]int) {
	type resident struct {
		key  string
		rank int
		ord  int
		node string
		box  string
	}
	laneRank := map[model.Lane]int{}
	for i, ln := range model.AllLanes {
		laneRank[ln] = i
	}
	byKey := map[string][]resident{}
	var keys []resident
	for _, idx := range b.items {
		it := p.items[idx]
		if it.gbox || it.comp.Type != "node" {
			continue
		}
		seen := map[string]int{}
		for _, hc := range p.hostsOf[it.id] {
			key, ord := hc.ID, order[hc.ID]
			if hc.Parent != "" {
				// the instances of one database share a row
				key, ord = hc.Parent, order[hc.Parent]
			}
			if n := seen[key]; n > 0 {
				key = fmt.Sprintf("%s#%d", key, n)
			}
			seen[key]++
			r := resident{key: key, rank: laneRank[model.LaneOf(hc)], ord: ord, node: it.id, box: hc.ID + "@" + it.id}
			if len(byKey[key]) == 0 {
				keys = append(keys, r)
			}
			byKey[key] = append(byKey[key], r)
			p.bandOf[r.box] = bi
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].rank != keys[j].rank {
			return keys[i].rank < keys[j].rank
		}
		if keys[i].ord != keys[j].ord {
			return keys[i].ord < keys[j].ord
		}
		return keys[i].key < keys[j].key
	})
	last := map[string]int{}
	for _, k := range keys {
		s := 0
		for _, r := range byKey[k.key] {
			if n, ok := last[r.node]; ok && n+1 > s {
				s = n + 1
			}
		}
		for _, r := range byKey[k.key] {
			last[r.node] = s
			p.slot[r.box] = s
		}
		if s+1 > b.slots {
			b.slots = s + 1
		}
	}
}

// inGroup reports whether a box belongs to a group or to one inside it.
func (p *plan) inGroup(it item, gid string) bool {
	for g := it.group; g != ""; g = p.parentOf[g] {
		if g == gid {
			return true
		}
	}
	return false
}

// spans says in which bands a group has boxes, and whether it is broken: a
// box that is not its own stands among them, within the columns its own
// boxes take. One frame around a broken group would hold what does not
// belong to it, so it is framed band by band.
func (p *plan) spans(g *Graph) (first, last map[string]int, broken map[string]bool) {
	first, last, broken = map[string]int{}, map[string]int{}, map[string]bool{}
	left, right := map[string]int{}, map[string]int{}
	for bi, b := range p.bands {
		for _, idx := range b.items {
			bx := g.Boxes[p.items[idx].id]
			for gid := p.items[idx].group; gid != ""; gid = p.parentOf[gid] {
				if _, ok := first[gid]; !ok {
					first[gid], left[gid], right[gid] = bi, bx.X, bx.Right()
				}
				last[gid] = bi
				left[gid], right[gid] = min(left[gid], bx.X), max(right[gid], bx.Right())
			}
		}
	}
	for gid := range first {
		pad := 2 + p.maxDepth - p.depthOf(gid)
		for bi := first[gid]; bi <= last[gid] && first[gid] != last[gid]; bi++ {
			for _, idx := range p.bands[bi].items {
				bx := g.Boxes[p.items[idx].id]
				if !p.inGroup(p.items[idx], gid) && bx.X < right[gid]+pad && bx.Right() > left[gid]-pad {
					broken[gid] = true
				}
			}
		}
	}
	// what is inside a group that is framed band by band is framed so too,
	// or its frame would reach out of the one it belongs in
	for gid := range first {
		for q := p.parentOf[gid]; q != ""; q = p.parentOf[q] {
			if broken[q] && first[gid] != last[gid] {
				broken[gid] = true
			}
		}
	}
	return first, last, broken
}

// halo is the room a band keeps free above or below itself for the borders
// of the groups that begin or end with it. A group that is framed band by
// band begins and ends with every band it has a box in.
func (p *plan) halos(g *Graph) (above, below []int) {
	first, last, broken := p.spans(g)
	above, below = make([]int, len(p.bands)), make([]int, len(p.bands))
	for i := range p.bands {
		above[i], below[i] = 2, 2
	}
	for gid := range first {
		if p.opts.Collapsed[gid] {
			continue
		}
		pad := 2 + p.maxDepth - p.depthOf(gid)
		for bi := first[gid]; bi <= last[gid]; bi++ {
			if bi == first[gid] || broken[gid] {
				above[bi] = max(above[bi], pad)
			}
			if bi == last[gid] || broken[gid] {
				below[bi] = max(below[bi], pad)
			}
		}
	}
	return above, below
}

// instanceH is the height of an instance box.
func (p *plan) instanceH() int {
	switch {
	case p.opts.Compact, p.opts.Detail == model.DetailMinimal:
		return 3 // border + label
	case p.opts.Detail == model.DetailFull:
		return 6 // + state + gauges line + note
	}
	return 5 // border + label + state + gauges line
}

// frameHeader is the interior rows a node keeps for itself.
func (p *plan) frameHeader(gauges int) int {
	if p.opts.Compact || p.opts.Detail == model.DetailMinimal {
		return 2
	}
	return 2 + min(gauges, 3)
}

// minFrameW keeps room beside the header of a machine for the lines that
// come down to its first instance.
const minFrameW = 40

// size returns the width and height of a box of band b (nil on the side).
func (p *plan) size(it item, b *band) (int, int) {
	opts := p.opts
	w, h := opts.MinBoxW, opts.MinBoxH
	if it.gbox {
		label := it.id
		if gr, ok := p.t.Group(it.id); ok && gr.Label != "" {
			label = gr.Label
		}
		if lw := len([]rune(label)) + 6; lw > w {
			w = lw
		}
		return w, h
	}
	c := it.comp
	if lw := len([]rune(c.DisplayLabel())) + 6; lw > w {
		w = lw
	}
	gauges := len(model.Catalog[c.Type].Gauges)
	if gauges > 4 {
		gauges = 4
	}
	if w < 25 && gauges > 0 {
		w = 25
	}
	// A node with residents is a frame: header rows, then one slot per
	// resident of the band, with the room between them the wires take.
	if hosts := p.hostsOf[c.ID]; c.Type == "node" && len(hosts) > 0 && b != nil && b.slots > 0 {
		for _, hc := range hosts {
			// "● Hatchet workers ×3" with a margin
			if lw := len([]rune(hc.DisplayLabel())) + 12 + 4; lw > w {
				w = lw
			}
			// "● Main database   primary": an instance of a database
			// carries the database's name and its role
			if par, ok := p.t.Component(hc.Parent); ok && hc.Parent != "" {
				if lw := len([]rune(par.DisplayLabel())) + len([]rune(hc.Notes)) + 9 + 4; lw > w {
					w = lw
				}
			}
		}
		return max(w, minFrameW), b.h
	}
	switch opts.Detail {
	case model.DetailMinimal:
		// border(2) + label + state + one note line
		h = 2 + 1 + 1 + 1
	case model.DetailFull:
		// border(2) + label + state + gauges + note lines
		h = 2 + 1 + 1 + gauges + 2
	default:
		if gauges > 3 {
			gauges = 3
		}
		// border(2) + label + state + up to three gauges + one line for a
		// change stamp
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

// stubW is the room between the last spine and the side column: the last
// stretch of a wire to a box on the side, and the rate written on it.
const stubW = 16

// place gives every box its coordinates. room says how many lanes of wire
// run where.
func (p *plan) place(room map[string]int) *Graph {
	opts := p.opts
	g := &Graph{Boxes: map[string]*Box{}, Routes: map[string]*Route{}, BoxOf: map[string]string{}, Instances: map[string][]string{}}
	for k, v := range p.boxOf {
		g.BoxOf[k] = v
	}
	for k, v := range p.instances {
		g.Instances[k] = append([]string(nil), v...)
	}
	boxes := g.Boxes

	// The frames of a band are as high as its slots and the wires between
	// them take.
	header := p.frameHeader(len(model.Catalog["node"].Gauges))
	for bi, b := range p.bands {
		b.slotY, b.gapY, b.h = nil, nil, 0
		if b.slots > 0 {
			y := 1 + header + frameTop
			for k := 0; k < b.slots; k++ {
				b.slotY = append(b.slotY, y)
				y += p.instanceH()
				b.gapY = append(b.gapY, y)
				lanes := room[gapKey(bi, k)]
				switch {
				case k < b.slots-1:
					// a row to leave the box above, the lanes, a row for
					// the arrowhead into the box below
					y += max(instanceGap, lanes+2)
				case lanes > 0:
					y += lanes + 1 + framePad
				default:
					y += framePad
				}
			}
			b.h = y + 1
		}
		for _, idx := range b.items {
			if _, h := p.size(p.items[idx], b); h > b.h {
				b.h = h
			}
		}
	}

	// Left to right within a band. Every band is split in two halves of
	// about the same width, and the halves stand left and right of the
	// street; a band of one box stands in the middle of it.
	margin := 2 + p.maxDepth
	street := max(opts.BoxGap, 2*room[streetKey]+3)
	type half struct{ left, right int }
	halves := make([]half, len(p.bands))
	widths := make([][]int, len(p.bands))
	gapBefore := func(bi, k int) int {
		b := p.bands[bi]
		it, prev := p.items[b.items[k]], p.items[b.items[k-1]]
		gap := opts.BoxGap
		if it.group != prev.group {
			gap = opts.GroupGap
		}
		return max(gap, 2*room[corridorKey(bi, k)]+3)
	}
	for bi, b := range p.bands {
		total := 0
		for k, idx := range b.items {
			w, _ := p.size(p.items[idx], b)
			widths[bi] = append(widths[bi], w)
			total += w
			if k > 0 {
				total += gapBefore(bi, k)
			}
		}
		b.split = -1
		switch {
		case len(b.items) == 0:
			continue
		case len(b.items) == 1 && b.lane == model.LaneEdge:
			// the way in stands in the middle, above what it leads to
			halves[bi] = half{(total - street + 1) / 2, (total - street) / 2}
			continue
		case len(b.items) == 1:
			// anything else leaves the street free for what passes
			b.split = 1
			halves[bi] = half{total + 2*room[corridorKey(bi, 0)], 0}
			continue
		}
		// the split that leaves the halves most alike
		best, left := 1<<30, 0
		for k := 1; k < len(b.items); k++ {
			left += widths[bi][k-1]
			if k > 1 {
				left += gapBefore(bi, k-1)
			}
			right := total - left - gapBefore(bi, k)
			if d := abs(left - right); d < best {
				best, b.split = d, k
				halves[bi] = half{left + 2*room[corridorKey(bi, 0)], right + 2*room[corridorKey(bi, len(b.items))]}
			}
		}
	}
	maxLeft, maxRight := 0, 0
	for _, h := range halves {
		maxLeft, maxRight = max(maxLeft, h.left), max(maxRight, h.right)
	}
	p.streetX = margin + maxLeft
	for bi, b := range p.bands {
		x := p.streetX - halves[bi].left + 2*room[corridorKey(bi, 0)]
		if b.split < 0 {
			x = p.streetX - halves[bi].left
		}
		for k, idx := range b.items {
			it := p.items[idx]
			w, h := p.size(it, b)
			switch {
			case k == b.split:
				x = p.streetX + street
			case k > 0:
				x += gapBefore(bi, k)
			}
			boxes[it.id] = &Box{ID: it.id, X: x, W: w, H: h, Lane: it.lane, Group: it.group, GroupBox: it.gbox, Members: it.mem}
			x += w
		}
	}
	maxW := maxLeft + street + maxRight
	above, below := p.halos(g)
	p.chanY = make([]int, len(p.bands)+1)
	// Top to bottom. Between two bands lie the room below the upper one, the
	// lanes of the channel, and the room above the lower one.
	y := 1
	for bi, b := range p.bands {
		p.chanY[bi] = y
		y += room[chanKey(bi)] + above[bi]
		for _, idx := range b.items {
			boxes[p.items[idx].id].Y = y
		}
		b.y = y
		y += b.h + below[bi]
	}
	p.chanY[len(p.bands)] = y

	// The side column stands right of everything, the spines between.
	right := margin + maxW + margin
	p.spines = room[spineKey]
	p.spineX = right + 2
	sideX := right + opts.GroupGap
	if p.spines > 0 {
		sideX = p.spineX + 2*p.spines + stubW
	}
	sy := 1
	framed := "" // the framed group the row above belongs to
	for _, idx := range p.side {
		it := p.items[idx]
		w, h := p.size(it, nil)
		x := sideX
		in := ""
		if p.sideFramed[it.group] && !it.gbox {
			in = it.group
			x += sideFramePad
		}
		if in != framed {
			// the border of a frame that ends, and of one that begins
			if framed != "" {
				sy += sideFramePad
			}
			if in != "" {
				sy += sideFramePad
			}
		}
		framed = in
		boxes[it.id] = &Box{ID: it.id, X: x, Y: sy, W: w, H: h, Lane: model.LaneSide, Group: it.group, GroupBox: it.gbox, Members: it.mem}
		sy += h + 2
	}

	// Saved positions; new boxes avoid saved ones.
	for id, b := range boxes {
		if pl, ok := p.l.Components[id]; ok && !pl.Auto {
			b.X, b.Y = pl.X, pl.Y
			// a saved size never shrinks a node frame below its residents
			frame := len(p.hostsOf[id]) > 0 && !b.GroupBox
			if pl.W >= opts.MinBoxW && (!frame || pl.W >= b.W) {
				b.W = pl.W
			}
			if pl.H >= 3 && (!frame || pl.H >= b.H) {
				b.H = pl.H
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
				if overlaps(b, o) {
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
	draw := func(idxs []int) {
		var laneIDs []string
		for _, idx := range idxs {
			laneIDs = append(laneIDs, p.items[idx].id)
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
	for _, b := range p.bands {
		draw(b.items)
	}
	draw(p.side)

	// Instances inside their node frames, drawn after every other box so
	// they sit on top, each in the slot of its component.
	for _, b := range p.bands {
		for _, idx := range b.items {
			it := p.items[idx]
			nb := boxes[it.id]
			hosts := p.hostsOf[it.id]
			if nb.GroupBox || it.comp.Type != "node" || len(hosts) == 0 || b.slots == 0 {
				continue
			}
			nb.Frame, nb.Header = true, header
			placed := append([]model.Component(nil), hosts...)
			sort.SliceStable(placed, func(i, j int) bool {
				return p.slot[placed[i].ID+"@"+it.id] < p.slot[placed[j].ID+"@"+it.id]
			})
			for _, hc := range placed {
				id := hc.ID + "@" + it.id
				ib := &Box{ID: id, X: nb.X + 2, Y: nb.Y + b.slotY[p.slot[id]], W: nb.W - 4, H: p.instanceH(), Lane: nb.Lane, Group: hc.Group, Instance: hc.ID, Node: it.id}
				boxes[id] = ib
				g.Order = append(g.Order, id)
			}
		}
	}

	// Group frames: bounding boxes of descendants, padded by depth.
	g.Groups = p.frames(g)

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
	if y+room[chanKey(len(p.bands))]+2 > g.H {
		g.H = y + room[chanKey(len(p.bands))] + 2
	}
	return g
}
