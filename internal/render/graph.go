package render

import (
	"fmt"
	"strings"

	"github.com/danilopopovikj/wassup/internal/layout"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/state"
)

// StateColor maps a state to its one color.
func StateColor(s model.State) Color {
	switch s {
	case model.Flowing:
		return ColGreen
	case model.Idle:
		return ColGray
	case model.Waiting:
		return ColAmber
	case model.Processing:
		return ColBlue
	case model.Blocked, model.Failing:
		return ColRed
	}
	return ColDefault
}

// SeverityColor maps a severity to a color for the timeline and status line.
func SeverityColor(s model.Severity) Color {
	switch s {
	case model.Crit:
		return ColRed
	case model.Warn:
		return ColAmber
	}
	return ColGreen
}

// spinner frames for processing.
var spinner = []rune{'◐', '◓', '◑', '◒'}

// DrawOptions control one frame.
type DrawOptions struct {
	Selected    string       // selected element id
	Frame       int          // animation frame counter
	Lens        *model.Issue // when set, everything off the path is dimmed
	Annotations []model.Annotation
	Topology    *model.Topology
	Snapshot    *model.Snapshot
	Collapsed   map[string]bool
	Animate     bool
}

// Draw paints the whole graph on a canvas sized to the layout.
func Draw(g *layout.Graph, opts DrawOptions) *Canvas {
	c := NewCanvas(g.W, g.H)
	DrawOn(c, g, opts)
	return c
}

// DrawOn paints the graph onto an existing canvas.
func DrawOn(c *Canvas, g *layout.Graph, opts DrawOptions) {
	snap := opts.Snapshot
	if snap == nil {
		snap = &model.Snapshot{Components: map[string]model.ElementState{}, Edges: map[string]model.ElementState{}}
	}
	lit := map[string]bool{}
	if opts.Lens != nil {
		for _, id := range opts.Lens.Path {
			lit[id] = true
			if b := g.BoxOf[id]; b != "" {
				lit[b] = true
			}
		}
	}
	annotated := map[string]bool{}
	for _, an := range opts.Annotations {
		for _, ref := range an.Path {
			r, err := model.ParseRef(ref)
			if err != nil {
				continue
			}
			annotated[r.ID] = true
			if b := g.BoxOf[r.ID]; b != "" {
				annotated[b] = true
			}
		}
	}

	// Groups first (outermost first), then routes, then boxes on top.
	for _, gf := range g.Groups {
		st := Style{Fg: ColGray}
		if opts.Lens != nil {
			st.Dim = true
		}
		c.Box(gf.X, gf.Y, gf.W, gf.H, BorderDouble, st)
		title := " " + gf.Label + " "
		if gf.Kind != "" && gf.Kind != "db" {
			title = " " + gf.Label + " · " + gf.Kind + " "
		}
		c.Text(gf.X+2, gf.Y, title, Style{Fg: ColGray, Bold: true}, gf.W-4)
	}
	var labels []func()
	for _, id := range sortedRouteIDs(g.Routes) {
		r := g.Routes[id]
		if lf := drawRoute(c, g, r, snap, opts, lit, annotated); lf != nil {
			labels = append(labels, lf)
		}
	}
	// Labels go on after every line so they never get cut by a later route.
	for _, lf := range labels {
		lf()
	}
	// Frame titles again, on top of any route that crossed them.
	for _, gf := range g.Groups {
		title := " " + gf.Label + " "
		if gf.Kind != "" && gf.Kind != "db" {
			title = " " + gf.Label + " · " + gf.Kind + " "
		}
		c.Text(gf.X+2, gf.Y, title, Style{Fg: ColGray, Bold: true}, gf.W-4)
	}
	for _, id := range g.Order {
		b := g.Boxes[id]
		drawBox(c, g, b, snap, opts, lit, annotated)
	}
	// Annotation callouts anchored at the last element of each path.
	for i, an := range opts.Annotations {
		if len(an.Path) == 0 {
			continue
		}
		last, err := model.ParseRef(an.Path[len(an.Path)-1])
		if err != nil {
			continue
		}
		bid := g.BoxOf[last.ID]
		if bid == "" {
			if e, ok := opts.Topology.Edge(last.ID); ok {
				bid = g.BoxOf[e.To]
			}
		}
		b, ok := g.Boxes[bid]
		if !ok {
			continue
		}
		note := an.Note
		if an.Confidence != "" {
			note += " (" + an.Confidence + ")"
		}
		w := len([]rune(note)) + 4
		if w > 48 {
			w = 48
		}
		x, y := b.Right()+1, b.Y-2+i
		if x+w > c.W {
			x = b.X
			y = b.Bottom()
		}
		if y < 0 {
			y = 0
		}
		c.Fill(x, y, w, 3, ' ', Style{})
		c.Box(x, y, w, 3, BorderRounded, Style{Fg: ColAccent})
		c.Text(x+2, y+1, note, Style{Fg: ColAccent}, w-4)
	}
}

func sortedRouteIDs(m map[string]*layout.Route) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// simple insertion sort keeps the package free of extra imports
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func drawBox(c *Canvas, g *layout.Graph, b *layout.Box, snap *model.Snapshot, opts DrawOptions, lit, annotated map[string]bool) {
	var es model.ElementState
	label := b.ID
	var comp model.Component
	if b.GroupBox {
		// worst state of the members
		worst := model.Idle
		var worstSev model.Severity
		for _, m := range b.Members {
			ms := snap.Components[m]
			if ms.Severity > worstSev || (ms.Severity == worstSev && stateRank(ms.State) > stateRank(worst)) {
				worst, worstSev = ms.State, ms.Severity
			}
		}
		es = model.ElementState{State: worst, Severity: worstSev, Label: fmt.Sprintf("%d inside, %s", len(b.Members), worst)}
		if gr, ok := opts.Topology.Group(b.ID); ok && gr.Label != "" {
			label = gr.Label
		}
		label = "▸ " + label
	} else {
		var known bool
		es, known = snap.Components[b.ID]
		if !known {
			es = model.ElementState{State: model.Idle, Marker: model.MarkerUnbound, Label: "no snapshot yet"}
		}
		if cc, ok := opts.Topology.Component(b.ID); ok {
			comp = cc
			label = cc.DisplayLabel()
		}
	}
	col := StateColor(es.State)
	kind := BorderRounded
	st := Style{Fg: col}
	switch es.Marker {
	case model.MarkerUnbound:
		kind = BorderDashed
		st = Style{Fg: ColGray, Dim: true}
	case model.MarkerStale:
		st.Dim = true
	}
	if es.State == model.Idle && es.Marker == "" {
		st.Dim = true
	}
	selected := opts.Selected == b.ID
	if selected {
		kind = BorderHeavy
		st.Bold = true
		st.Dim = false
	}
	if annotated[b.ID] {
		st.Fg = ColAccent
		st.Dim = false
	}
	// Clear interior, hatch when stale.
	fill := ' '
	if es.Marker == model.MarkerStale {
		fill = '░'
	}
	c.Fill(b.X, b.Y, b.W, b.H, fill, Style{Fg: ColGray, Dim: true})
	c.Box(b.X, b.Y, b.W, b.H, kind, st)

	inner := b.W - 4
	x, y := b.X+2, b.Y+1
	// Line 1: glyph + label.
	glyph := es.State.Glyph()
	if es.State == model.Processing && opts.Animate {
		glyph = string(spinner[opts.Frame/2%len(spinner)])
	}
	switch es.Marker {
	case model.MarkerUnbound:
		glyph = "┄"
	case model.MarkerStale:
		glyph = "◷"
	}
	tst := Style{Fg: col, Bold: true}
	if es.Marker == model.MarkerUnbound {
		tst = Style{Fg: ColGray}
	}
	c.Text(x, y, glyph+" ", tst, 2)
	c.Text(x+2, y, ellipsis(label, inner-2), Style{Bold: true}, inner-2)
	// Type hint on the right of the title when there is room.
	hint := comp.Type
	if comp.Type == "db" && comp.Notes != "" && comp.Parent != "" {
		hint = comp.Notes // primary / replica
	}
	if b.Lane == "side" && comp.Group != "" {
		hint = comp.Group
	}
	if hint != "" && inner-len([]rune(label))-3 >= len([]rune(hint)) {
		c.Text(b.Right()-2-len([]rune(hint)), y, hint, Style{Fg: ColGray, Dim: true}, len([]rune(hint)))
	}
	y++
	if y >= b.Bottom()-1 {
		return
	}
	// Line 2: state label.
	lab := es.Label
	if es.Marker == model.MarkerStale {
		lab = "stale, " + lab
	}
	c.Text(x, y, ellipsis(lab, inner), Style{Fg: col}, inner)
	y++
	// Gauges.
	for _, gg := range es.Gauges {
		if y >= b.Bottom()-1 {
			break
		}
		drawGauge(c, x, y, inner, gg)
		y++
	}
	// Hosted workloads for nodes.
	if comp.Type == "node" {
		if hosted, ok := es.Detail["hosted"].([]string); ok {
			for _, h := range hosted {
				if y >= b.Bottom()-1 {
					break
				}
				id, stt, _ := strings.Cut(h, ":")
				ws := model.State(stt)
				c.Text(x, y, ws.Glyph()+" "+id, Style{Fg: StateColor(ws)}, inner)
				y++
			}
		} else if hostedAny, ok := es.Detail["hosted"].([]any); ok {
			for _, hv := range hostedAny {
				if y >= b.Bottom()-1 {
					break
				}
				h, _ := hv.(string)
				id, stt, _ := strings.Cut(h, ":")
				ws := model.State(stt)
				c.Text(x, y, ws.Glyph()+" "+id, Style{Fg: StateColor(ws)}, inner)
				y++
			}
		}
	}
	// Notes: change stamps, target health, cert countdown.
	for _, n := range es.Notes {
		if y >= b.Bottom()-1 {
			break
		}
		c.Text(x, y, ellipsis(n, inner), Style{Fg: ColGray}, inner)
		y++
	}
	// Waiting pile in the corner.
	if es.State == model.Waiting {
		c.Text(b.Right()-4, b.Bottom()-1, "≡≡", Style{Fg: ColAmber, Bold: true}, 2)
	}
	if es.State == model.Blocked {
		c.Text(b.Right()-3, b.Bottom()-1, "⊘", Style{Fg: ColRed, Bold: true}, 1)
	}
	if opts.Lens != nil && !lit[b.ID] {
		c.Dim(b.X, b.Y, b.W, b.H)
	}
}

// ellipsis clips s to w runes, ending with … when clipped.
func ellipsis(s string, w int) string {
	r := []rune(s)
	if len(r) <= w || w <= 0 {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

func stateRank(s model.State) int {
	switch s {
	case model.Failing:
		return 5
	case model.Blocked:
		return 4
	case model.Waiting:
		return 3
	case model.Processing:
		return 2
	case model.Flowing:
		return 1
	}
	return 0
}

// drawGauge paints "name ▓▓▓▓░░░░ 84%" in inner cells.
func drawGauge(c *Canvas, x, y, inner int, g model.Gauge) {
	name := g.Name
	if len(name) > 4 {
		name = name[:4]
	}
	c.Text(x, y, fmt.Sprintf("%-4s", name), Style{Fg: ColGray}, 4)
	barX := x + 5
	barW := 8
	col := ColGreen
	switch g.Level {
	case "amber":
		col = ColAmber
	case "red":
		col = ColRed
	case "none":
		col = ColGray
	}
	if g.Pct < 0 {
		// no percentage: dotted bar when no data, plain value when rate-only
		if g.Value == "" {
			c.Text(barX, y, strings.Repeat("┈", barW), Style{Fg: ColGray, Dim: true}, barW)
		} else {
			c.Text(barX, y, g.Value, Style{Fg: col}, inner-5)
		}
	} else {
		filled := int(g.Pct/100*float64(barW) + 0.5)
		if filled > barW {
			filled = barW
		}
		if filled < 0 {
			filled = 0
		}
		c.Text(barX, y, strings.Repeat("█", filled), Style{Fg: col}, barW)
		c.Text(barX+filled, y, strings.Repeat("░", barW-filled), Style{Fg: ColGray, Dim: true}, barW-filled)
		val := g.Value
		switch g.Trend {
		case "up":
			val += " ↑"
		case "down":
			val += " ↓"
		}
		c.Text(barX+barW+1, y, val, Style{Fg: col}, inner-5-barW-1)
	}
}

// drawRoute paints an edge along its cells.
func drawRoute(c *Canvas, g *layout.Graph, r *layout.Route, snap *model.Snapshot, opts DrawOptions, lit, annotated map[string]bool) func() {
	if len(r.Cells) < 2 {
		return nil
	}
	// The drawn state is the worst among merged edges.
	var es model.ElementState
	first := true
	for _, id := range r.Merged {
		e := snap.Edges[id]
		if first || stateRank(e.State) > stateRank(es.State) || (e.State == es.State && e.Rate > es.Rate) {
			if first || stateRank(e.State) >= stateRank(es.State) {
				es = e
			}
		}
		first = false
	}
	col := StateColor(es.State)
	st := Style{Fg: col}
	dashed := es.State == model.Blocked || es.State == model.Failing
	switch es.Marker {
	case model.MarkerUnbound:
		st = Style{Fg: ColGray, Dim: true}
	case model.MarkerNoData:
		st = Style{Fg: ColGray, Dim: true}
	}
	if es.State == model.Idle && es.Marker == "" {
		st.Dim = true
	}
	isLit := false
	for _, id := range r.Merged {
		if lit[id] {
			isLit = true
		}
		if annotated[id] {
			st.Fg = ColAccent
			st.Dim = false
		}
		if opts.Selected == id {
			st.Bold = true
			st.Dim = false
		}
	}
	if opts.Lens != nil && !isLit {
		st.Dim = true
		st.Bold = false
	}
	cells := r.Cells
	n := len(cells)
	for i, p := range cells {
		var ch rune
		prev, next := Point(p), Point(p)
		if i > 0 {
			prev = Point(cells[i-1])
		}
		if i < n-1 {
			next = Point(cells[i+1])
		}
		if i == n-1 {
			ch = arrow(prev, p)
		} else if i == 0 && r.Both {
			ch = arrow(next, p)
		} else {
			ch = lineChar(prev, p, next, i == 0, i == n-1)
			if es.Marker == model.MarkerNoData || es.Marker == model.MarkerUnbound {
				if ch == '─' {
					ch = '┄'
				} else if ch == '│' {
					ch = '┆'
				}
			} else if dashed {
				if ch == '─' {
					ch = '╌'
				} else if ch == '│' {
					ch = '╎'
				}
			}
		}
		// Do not overwrite box borders or other arrowheads at endpoints.
		existing := c.Get(p.X, p.Y)
		if i != n-1 && existing.Ch != ' ' && isLineRune(existing.Ch) && existing.Ch != ch {
			ch = crossChar(existing.Ch, ch)
		}
		c.Set(p.X, p.Y, ch, st)
	}
	// Animation: a bright dot moving along a flowing edge; speed scales with rate.
	if es.State == model.Flowing && opts.Animate && es.Marker == "" && n > 3 {
		step := speedStep(es.Rate)
		if step > 0 {
			pos := (opts.Frame / step) % (n - 1)
			if pos > 0 {
				p := cells[pos]
				c.Set(p.X, p.Y, '●', Style{Fg: col, Bold: true, Dim: st.Dim})
			}
		}
	}
	if es.State == model.Processing && opts.Animate {
		if (opts.Frame/5)%2 == 0 {
			for _, p := range cells[1 : n-1] {
				cell := c.Get(p.X, p.Y)
				cell.St.Bold = true
				c.Set(p.X, p.Y, cell.Ch, cell.St)
			}
		}
	}
	// Waiting: dots piled at the arrowhead, up to 5, plus the count.
	if es.State == model.Waiting && n > 2 {
		pile := int(es.Queued)
		if pile > 5 {
			pile = 5
		}
		if pile < 1 {
			pile = 1
		}
		for k := 1; k <= pile && n-1-k > 0; k++ {
			p := cells[n-1-k]
			c.Set(p.X, p.Y, '•', Style{Fg: ColAmber, Bold: true})
		}
	}
	// Label at the midpoint, drawn later.
	return func() { labelRoute(c, g, r, es, st, opts) }
}

// Point alias for readability.
type Point = layout.Point

func speedStep(rate float64) int {
	switch {
	case rate <= 0:
		return 0
	case rate >= 1000:
		return 1
	case rate >= 100:
		return 2
	case rate >= 10:
		return 3
	case rate >= 1:
		return 4
	}
	return 6
}

func arrow(from, to Point) rune {
	switch {
	case to.X > from.X:
		return '▶'
	case to.X < from.X:
		return '◀'
	case to.Y > from.Y:
		return '▼'
	default:
		return '▲'
	}
}

func lineChar(prev, cur, next Point, first, last bool) rune {
	dIn := dir(prev, cur)
	dOut := dir(cur, next)
	if first {
		dIn = dOut
	}
	if last {
		dOut = dIn
	}
	if dIn == dOut {
		if dIn == 'h' {
			return '─'
		}
		return '│'
	}
	// corners: figure out which quadrant
	fromLeft := prev.X < cur.X
	fromRight := prev.X > cur.X
	fromUp := prev.Y < cur.Y
	fromDown := prev.Y > cur.Y
	toLeft := next.X < cur.X
	toRight := next.X > cur.X
	toUp := next.Y < cur.Y
	toDown := next.Y > cur.Y
	switch {
	case (fromLeft && toDown) || (fromDown && toLeft):
		return '╮'
	case (fromLeft && toUp) || (fromUp && toLeft):
		return '╯'
	case (fromRight && toDown) || (fromDown && toRight):
		return '╭'
	case (fromRight && toUp) || (fromUp && toRight):
		return '╰'
	}
	return '┼'
}

func dir(a, b Point) byte {
	if a.Y == b.Y {
		return 'h'
	}
	return 'v'
}

func isLineRune(r rune) bool {
	return strings.ContainsRune("─│╌╎┄┆╭╮╰╯┼", r)
}

func crossChar(existing, ch rune) rune {
	h := func(r rune) bool { return r == '─' || r == '╌' || r == '┄' }
	v := func(r rune) bool { return r == '│' || r == '╎' || r == '┆' }
	if (h(existing) && v(ch)) || (v(existing) && h(ch)) {
		return '┼'
	}
	return ch
}

// labelRoute writes the short edge label next to the route, trying the
// longest straight segments first and several offsets on both sides, so a
// label always finds a free spot unless the picture is packed solid.
func labelRoute(c *Canvas, g *layout.Graph, r *layout.Route, es model.ElementState, st Style, opts DrawOptions) {
	n := len(r.Cells)
	if n < 4 {
		return
	}
	text := shortEdgeLabel(es)
	if text == "" {
		return
	}
	w := len([]rune(text))
	type seg struct{ start, end int }
	var segs []seg
	i := 1
	for i < n-1 {
		j := i
		for j+1 < n-1 && dir(r.Cells[j], r.Cells[j+1]) == dir(r.Cells[i], r.Cells[i+1]) {
			j++
		}
		if j-i >= 1 {
			segs = append(segs, seg{i, j})
		}
		i = j + 1
	}
	for a := 1; a < len(segs); a++ {
		for b := a; b > 0 && (segs[b].end-segs[b].start) > (segs[b-1].end-segs[b-1].start); b-- {
			segs[b], segs[b-1] = segs[b-1], segs[b]
		}
	}
	lst := Style{Fg: st.Fg, Dim: st.Dim}
	for _, sg := range segs {
		horizontal := dir(r.Cells[sg.start], r.Cells[sg.start+1]) == 'h'
		mid := (sg.start + sg.end) / 2
		for _, off := range []int{0, -3, 3, -6, 6} {
			k := mid + off
			if k <= sg.start || k >= sg.end {
				continue
			}
			p := r.Cells[k]
			if horizontal {
				x := p.X - w/2
				for _, dy := range []int{-1, 1} {
					if !rowBusy(c, x, p.Y+dy, w) {
						c.Text(x, p.Y+dy, text, lst, w)
						return
					}
				}
			} else {
				for _, x := range []int{p.X + 1, p.X - 1 - w} {
					if x >= 0 && x+w <= c.W && !rowBusy(c, x, p.Y, w) {
						c.Text(x, p.Y, text, lst, w)
						return
					}
				}
			}
		}
	}
}

func rowBusy(c *Canvas, x, y, w int) bool {
	for i := -1; i <= w; i++ {
		if c.Get(x+i, y).Ch != ' ' {
			return true
		}
	}
	return false
}

// shortEdgeLabel is the few characters drawn on the line itself.
func shortEdgeLabel(es model.ElementState) string {
	switch es.Marker {
	case model.MarkerUnbound:
		return "unbound"
	case model.MarkerNoData:
		return "no data"
	}
	switch es.State {
	case model.Flowing:
		if es.Rate > 0 {
			s := state.Num(es.Rate) + " " + es.Unit
			if strings.Contains(es.Label, "miss") {
				parts := strings.Split(es.Label, ", ")
				s += ", " + parts[len(parts)-1]
			}
			if strings.Contains(es.Label, "double normal") {
				s += " ×2"
			}
			return s
		}
		return ""
	case model.Waiting:
		return state.Num(es.Queued) + " queued"
	case model.Failing:
		if es.ErrorRate > 0 {
			return fmt.Sprintf("%.0f%% errors", es.ErrorRate)
		}
		return strings.TrimPrefix(es.Label, "failing, ")
	case model.Blocked:
		return "⊘ " + strings.TrimPrefix(strings.TrimPrefix(es.Label, "blocked at "), "blocked, ")
	case model.Processing:
		return strings.TrimPrefix(es.Label, "processing ")
	}
	return ""
}
