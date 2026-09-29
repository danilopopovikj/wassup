package render

import (
	"fmt"
	"sort"
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
	// Detail is minimal, normal or full (model.DetailLevel).
	Detail string
}

// primaryGauges lists, per type, the gauges an engineer wants at a glance.
// Anything amber or red is promoted regardless; zero rate-only gauges hide.
var primaryGauges = map[string][]string{
	"node":             {"cpu", "ram", "disk"},
	"workload":         {"ready", "cpu", "ram"},
	"loadbalancer":     {"targets"},
	"backgroundworker": {"workers", "slots", "cpu"},
	"ingress":          {"errors", "cert"},
	"scheduledjob":     {"running", "succeeded", "failed"},
	"queue":            {"depth", "oldest"},
	"cache":            {"mem", "hits"},
	"database":         {"conns", "cpu", "disk"},
	"storage":          {"used", "size"},
	"observability":    {"ingest", "disk"},
	"external":         {"latency", "errors"},
	"firewall":         {},
}

// visibleGauges picks what a box shows at the given detail level.
func visibleGauges(comp model.Component, es model.ElementState, level string) []model.Gauge {
	if level == model.DetailMinimal {
		return nil
	}
	if level == model.DetailFull {
		return es.Gauges
	}
	primary := map[string]bool{}
	for _, n := range primaryGauges[comp.Type] {
		primary[n] = true
	}
	var out []model.Gauge
	// promoted first: amber and red gauges are the signal
	for _, g := range es.Gauges {
		if g.Level == "amber" || g.Level == "red" {
			out = append(out, g)
		}
	}
	for _, g := range es.Gauges {
		if g.Level == "amber" || g.Level == "red" || !primary[g.Name] {
			continue
		}
		// a zero rate-only gauge ("restarts 0") says nothing
		if g.Pct < 0 && (g.Value == "0" || g.Value == "0%" || strings.HasPrefix(g.Value, "0 ")) {
			continue
		}
		out = append(out, g)
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
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
			for _, ib := range g.Instances[id] {
				lit[ib] = true
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
		c.Text(gf.X+2, gf.Y, groupTitle(gf, opts), Style{Fg: ColGray, Bold: true}, gf.W-4)
	}
	// Node frames go under the routes, so the arrows between the instances
	// inside a machine stay visible; every other box goes on top.
	for _, id := range g.Order {
		if b := g.Boxes[id]; b.Frame {
			drawBox(c, g, b, snap, opts, lit, annotated)
		}
	}
	// Labels go on after every line so they never get cut by a later route.
	for _, lf := range drawWires(c, g, snap, opts, lit, annotated) {
		lf()
	}
	// Frame titles again, on top of any route that crossed them.
	for _, gf := range g.Groups {
		c.Text(gf.X+2, gf.Y, groupTitle(gf, opts), Style{Fg: ColGray, Bold: true}, gf.W-4)
	}
	for _, id := range g.Order {
		b := g.Boxes[id]
		if b.Frame {
			continue
		}
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

func groupTitle(gf layout.GroupFrame, opts DrawOptions) string {
	if opts.Detail == model.DetailFull && gf.Kind != "" && gf.Kind != "database" {
		return " " + gf.Label + " · " + gf.Kind + " "
	}
	return " " + gf.Label + " "
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
		es, known = snap.Components[b.Element()]
		if !known {
			es = model.ElementState{State: model.Idle, Marker: model.MarkerUnbound, Label: "no snapshot yet"}
		}
		if cc, ok := opts.Topology.Component(b.Element()); ok {
			comp = cc
			label = cc.DisplayLabel()
		}
	}
	if b.Instance != "" {
		drawInstance(c, g, b, comp, es, snap, opts, lit, annotated)
		return
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
	if b.Frame {
		// A machine: its own label, state and gauges in the header; the
		// instances inside are drawn afterwards, on top.
		drawFrameHeader(c, b, comp, es, opts)
		if opts.Lens != nil && !lit[b.ID] {
			c.Dim(b.X, b.Y, b.W, b.H)
		}
		return
	}

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
	case model.MarkerUnmetered:
		glyph = unmeteredGlyph
	}
	tst := Style{Fg: col, Bold: true}
	if es.Marker == model.MarkerUnbound {
		tst = Style{Fg: ColGray}
	}
	c.Text(x, y, glyph+" ", tst, 2)
	c.Text(x+2, y, ellipsis(label, inner-2), Style{Bold: true}, inner-2)
	// Type hint on the right of the title when there is room.
	hint := ""
	if opts.Detail == model.DetailFull {
		hint = comp.Type
		if b.Lane == "side" && comp.Group != "" {
			hint = comp.Group
		}
	}
	if comp.Type == "database" && comp.Notes != "" && comp.Parent != "" {
		hint = comp.Notes // primary / replica: always worth knowing
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
	for _, gg := range visibleGauges(comp, es, opts.Detail) {
		if y >= b.Bottom()-1 {
			break
		}
		drawGauge(c, x, y, inner, gg)
		y++
	}
	// Notes: change stamps always (that is "what changed"); the rest only when
	// the component is in trouble or the level is full.
	for _, n := range es.Notes {
		if y >= b.Bottom()-1 {
			break
		}
		if opts.Detail != model.DetailFull && es.Severity < model.Warn && !isStamp(n) {
			continue
		}
		st := Style{Fg: ColGray}
		if isStamp(n) {
			st = Style{Fg: ColCyan}
		}
		c.Text(x, y, ellipsis(n, inner), st, inner)
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

// unmeteredGlyph marks a box that is up and in order while nothing counts
// what goes through it. It is drawn bright, as what runs is, and not with
// the ring of idle, which would say that nothing goes through it.
const unmeteredGlyph = "◌"

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

// hostedOf finds what a node knows about one of its residents.
func hostedOf(snap *model.Snapshot, nodeID, compID string) (model.Hosted, bool) {
	for _, h := range snap.Components[nodeID].Hosted {
		if h.ID == compID {
			return h, true
		}
	}
	return model.Hosted{}, false
}

// drawFrameHeader paints a node's title, state and gauges in the header
// rows of its frame.
func drawFrameHeader(c *Canvas, b *layout.Box, comp model.Component, es model.ElementState, opts DrawOptions) {
	col := StateColor(es.State)
	inner := b.W - 4
	x, y := b.X+2, b.Y+1
	glyph := es.State.Glyph()
	tst := Style{Fg: col, Bold: true}
	switch es.Marker {
	case model.MarkerUnbound:
		glyph, tst = "┄", Style{Fg: ColGray}
	case model.MarkerStale:
		glyph = "◷"
	case model.MarkerUnmetered:
		glyph = unmeteredGlyph
	}
	c.Text(x, y, glyph+" ", tst, 2)
	c.Text(x+2, y, ellipsis(comp.DisplayLabel(), inner-2), Style{Bold: true}, inner-2)
	if opts.Detail == model.DetailFull {
		hint := "machine"
		c.Text(b.Right()-2-len(hint), y, hint, Style{Fg: ColGray, Dim: true}, len(hint))
	}
	y++
	lab := es.Label
	if es.Marker == model.MarkerStale {
		lab = "stale, " + lab
	}
	c.Text(x, y, ellipsis(lab, inner), Style{Fg: col}, inner)
	y++
	for _, gg := range visibleGauges(comp, es, opts.Detail) {
		if y > b.Y+b.Header {
			break
		}
		drawGauge(c, x, y, inner, gg)
		y++
	}
}

// drawInstance paints one copy of a component inside a node: "● API ×1",
// then what this node knows (0 of 1 ready, 6 restarts) or the component's
// own state line, then a compact gauge line when there is room.
func drawInstance(c *Canvas, g *layout.Graph, b *layout.Box, comp model.Component, es model.ElementState, snap *model.Snapshot, opts DrawOptions, lit, annotated map[string]bool) {
	state, marker := es.State, es.Marker
	label := comp.DisplayLabel()
	hint := ""
	if p, ok := opts.Topology.Component(comp.Parent); ok && comp.Parent != "" {
		// an instance of a database: the database's name, and primary or
		// replica beside it, since no group title names it in here
		label, hint = p.DisplayLabel(), comp.Notes
	}
	line := es.Label
	if h, ok := hostedOf(snap, b.Node, b.Instance); ok && h.Known && h.Pods == 0 {
		drawEmptyPlace(c, b, label, opts)
		return
	}
	gauges := visibleGauges(comp, es, opts.Detail)
	if h, ok := hostedOf(snap, b.Node, b.Instance); ok && h.Known {
		state = h.State
		label += fmt.Sprintf(" ×%d", h.Pods)
		elsewhere := podsOf(snap, b.Instance) > h.Pods
		switch {
		case h.Ready < h.Pods:
			line = fmt.Sprintf("%d of %d ready", h.Ready, h.Pods)
		case h.Restarts > 0:
			line = fmt.Sprintf("%d restarts here", h.Restarts)
		case h.State == model.Flowing && es.State != model.Flowing:
			line = "fine here"
		case h.Rate != nil && es.State == model.Flowing:
			line = "idle here"
			if *h.Rate > 0 {
				line = "flowing, " + rateText(*h.Rate, es.Unit)
			}
		case elsewhere && es.State == model.Flowing && es.Rate > 0:
			// the rate is the component's, not this copy's
			line = "flowing, " + rateText(es.Rate, es.Unit) + " in all"
		}
		if elsewhere {
			gauges = placeGauges(gauges, h)
		}
	}
	col := StateColor(state)
	kind := BorderRounded
	st := Style{Fg: col}
	switch marker {
	case model.MarkerUnbound:
		kind = BorderDashed
		st = Style{Fg: ColGray, Dim: true}
	case model.MarkerStale:
		st.Dim = true
	}
	if state == model.Idle && marker == "" {
		st.Dim = true
	}
	if opts.Selected == b.Instance || opts.Selected == b.ID {
		kind = BorderHeavy
		st.Bold = true
		st.Dim = false
	}
	if annotated[b.Instance] {
		st.Fg = ColAccent
		st.Dim = false
	}
	fill := ' '
	if marker == model.MarkerStale {
		fill = '░'
	}
	c.Fill(b.X, b.Y, b.W, b.H, fill, Style{Fg: ColGray, Dim: true})
	c.Box(b.X, b.Y, b.W, b.H, kind, st)
	inner := b.W - 4
	x, y := b.X+2, b.Y+1
	glyph := state.Glyph()
	if state == model.Processing && opts.Animate {
		glyph = string(spinner[opts.Frame/2%len(spinner)])
	}
	tst := Style{Fg: col, Bold: true}
	switch marker {
	case model.MarkerUnbound:
		glyph, tst = "┄", Style{Fg: ColGray}
	case model.MarkerStale:
		glyph = "◷"
	case model.MarkerUnmetered:
		glyph = unmeteredGlyph
	}
	c.Text(x, y, glyph+" ", tst, 2)
	c.Text(x+2, y, ellipsis(label, inner-2), Style{Bold: true}, inner-2)
	if hint != "" && inner-len([]rune(label))-3 >= len([]rune(hint)) {
		c.Text(b.Right()-2-len([]rune(hint)), y, hint, Style{Fg: ColGray, Dim: true}, len([]rune(hint)))
	}
	y++
	if y < b.Bottom()-1 {
		if marker == model.MarkerStale {
			line = "stale, " + line
		}
		c.Text(x, y, ellipsis(line, inner), Style{Fg: col}, inner)
		y++
	}
	if y < b.Bottom()-1 {
		// cpu 40% · ram 55%: the component's essentials in one line
		var parts []string
		for _, gg := range gauges {
			name := gg.Short
			if name == "" {
				name = gg.Name
			}
			parts = append(parts, name+" "+gg.Value)
			if len(parts) == 3 {
				break
			}
		}
		if len(parts) > 0 {
			c.Text(x, y, ellipsis(strings.Join(parts, " · "), inner), Style{Fg: ColGray}, inner)
			y++
		}
	}
	for _, n := range es.Notes {
		if y >= b.Bottom()-1 {
			break
		}
		if opts.Detail != model.DetailFull && es.Severity < model.Warn && !isStamp(n) {
			continue
		}
		nst := Style{Fg: ColGray}
		if isStamp(n) {
			nst = Style{Fg: ColCyan}
		}
		c.Text(x, y, ellipsis(n, inner), nst, inner)
		y++
	}
	if state == model.Waiting {
		c.Text(b.Right()-4, b.Bottom()-1, "≡≡", Style{Fg: ColAmber, Bold: true}, 2)
	}
	if state == model.Blocked {
		c.Text(b.Right()-3, b.Bottom()-1, "⊘", Style{Fg: ColRed, Bold: true}, 1)
	}
	if opts.Lens != nil && !lit[b.Instance] && !lit[b.ID] {
		c.Dim(b.X, b.Y, b.W, b.H)
	}
}

// rateText says a rate in its unit; drawInstance names its state "state".
func rateText(v float64, unit string) string { return state.Rate(v, unit) }

// podsOf counts the pods of a component on every machine that says.
func podsOf(snap *model.Snapshot, compID string) int {
	n := 0
	for _, es := range snap.Components {
		for _, h := range es.Hosted {
			if h.ID == compID && h.Known {
				n += h.Pods
			}
		}
	}
	return n
}

// placeGauges makes the gauges of a component that runs on several machines
// say what its copy on one of them does: its own pods ready, and its own cpu
// and ram where the provider read them pod by pod. A gauge of the whole
// that the copy cannot say is left out rather than shown in its place.
func placeGauges(all []model.Gauge, h model.Hosted) []model.Gauge {
	var out []model.Gauge
	for _, g := range all {
		switch g.Name {
		case "ready":
			g.Value = fmt.Sprintf("%d/%d", h.Ready, h.Pods)
			g.Pct = 100 * float64(h.Ready) / float64(max(h.Pods, 1))
		case "cpu":
			if h.CPUPct == nil {
				continue
			}
			g.Value, g.Pct = fmt.Sprintf("%.0f%%", *h.CPUPct), *h.CPUPct
		case "ram":
			if h.MemPct == nil {
				continue
			}
			g.Value, g.Pct = fmt.Sprintf("%.0f%%", *h.MemPct), *h.MemPct
		case "restarts":
			g.Value = fmt.Sprint(h.Restarts)
		}
		out = append(out, g)
	}
	return out
}

// drawEmptyPlace paints the place of a component on a machine that holds
// none of its pods: its name, faint, and no box. The component may run
// there, which is why the place is kept; it does not now, which is why
// nothing is drawn that looks like a copy of it.
func drawEmptyPlace(c *Canvas, b *layout.Box, label string, opts DrawOptions) {
	c.Fill(b.X, b.Y, b.W, b.H, ' ', Style{})
	st := Style{Fg: ColGray, Dim: true}
	if opts.Selected == b.Instance || opts.Selected == b.ID {
		c.Box(b.X, b.Y, b.W, b.H, BorderDashed, st)
	}
	c.Text(b.X+2, b.Y+b.H/2, ellipsis("· "+label+", none here", b.W-4), st, b.W-4)
}

// isStamp reports whether a note is a change marker ("deploy 09:48 b7e9f21").
func isStamp(n string) bool {
	for _, k := range []string{"deploy ", "terraform ", "node ", "cert ", "scale ", "switchover ", "eviction ", "job "} {
		if strings.HasPrefix(n, k) && len(n) > len(k)+4 && n[len(k)+2] == ':' {
			return true
		}
	}
	return false
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
	name := g.Short
	if name == "" {
		name = g.Name
	}
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

// look is how one logical edge is drawn: its state, its style, and how much
// it matters when two wires want the same cell.
type look struct {
	es     model.ElementState
	st     Style
	dashed bool
	lit    bool
	rank   int
}

// lookOf decides how the edges one route draws look. The drawn state is the
// worst among the merged edges.
func lookOf(r *layout.Route, snap *model.Snapshot, opts DrawOptions, lit, annotated map[string]bool) look {
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
	lk := look{es: es, st: Style{Fg: StateColor(es.State)}}
	lk.dashed = es.State == model.Blocked || es.State == model.Failing
	lk.rank = 2 * stateRank(es.State)
	switch es.Marker {
	case model.MarkerUnbound, model.MarkerNoData:
		lk.st = Style{Fg: ColGray, Dim: true}
		lk.rank = 0
	}
	if es.State == model.Idle && (es.Marker == "" || es.Marker == model.MarkerUnmetered) {
		lk.st.Dim = true
		lk.rank = 1
	}
	for _, id := range r.Merged {
		if lit[id] {
			lk.lit = true
		}
		if annotated[id] {
			lk.st.Fg = ColAccent
			lk.st.Dim = false
			lk.rank += 20
		}
		if opts.Selected == id {
			lk.st.Bold = true
			lk.st.Dim = false
			lk.rank += 40
		}
	}
	if opts.Lens != nil {
		if lk.lit {
			lk.rank += 20
		} else {
			lk.st.Dim = true
			lk.st.Bold = false
		}
	}
	return lk
}

// noneHere reports whether a route ends at a copy of a component that is
// known not to be there: the machine holds none of its pods. The wire to an
// empty place is not drawn, so what is drawn joins what runs.
func noneHere(g *layout.Graph, r *layout.Route, snap *model.Snapshot) bool {
	for _, id := range []string{r.From, r.To} {
		b := g.Boxes[id]
		if b == nil || b.Instance == "" {
			continue
		}
		if h, ok := hostedOf(snap, b.Node, b.Instance); ok && h.Known && h.Pods == 0 {
			return true
		}
	}
	return false
}

// Directions a line cell connects to, as bits.
const (
	dirUp = 1 << iota
	dirDown
	dirLeft
	dirRight
)

// junctionChar picks the box-drawing rune for a cell from the directions it
// connects to: straights, corners, tees where a sibling route joins its
// trunk, a cross where four meet.
func junctionChar(d int) rune {
	switch d {
	case dirLeft | dirRight, dirLeft, dirRight:
		return '─'
	case dirUp | dirDown, dirUp, dirDown:
		return '│'
	case dirRight | dirDown:
		return '╭'
	case dirLeft | dirDown:
		return '╮'
	case dirRight | dirUp:
		return '╰'
	case dirLeft | dirUp:
		return '╯'
	case dirLeft | dirRight | dirDown:
		return '┬'
	case dirLeft | dirRight | dirUp:
		return '┴'
	case dirUp | dirDown | dirRight:
		return '├'
	case dirUp | dirDown | dirLeft:
		return '┤'
	}
	return '┼'
}

// bit is the direction from a to its orthogonal neighbour b.
func bit(a, b Point) int {
	switch {
	case b.X > a.X:
		return dirRight
	case b.X < a.X:
		return dirLeft
	case b.Y > a.Y:
		return dirDown
	case b.Y < a.Y:
		return dirUp
	}
	return 0
}

// drawWires paints every route. The routes of one net are one drawing: where
// they meet they are joined by a tee, and each cell takes the color of the
// edge that matters most among those that pass it. Two nets that meet
// cross: the one that matters more is drawn over the other, in one piece,
// so that a crossing never reads as a junction. Arrowheads go on last. It
// returns the label painters, run after every line is down.
func drawWires(c *Canvas, g *layout.Graph, snap *model.Snapshot, opts DrawOptions, lit, annotated map[string]bool) []func() {
	type wire struct {
		key    string
		routes []*layout.Route
		looks  []look
		rank   int
	}
	byKey := map[string]*wire{}
	var wires []*wire
	labelled, moving := map[string]bool{}, map[string]bool{}
	var labels []func()
	for _, id := range sortedRouteIDs(g.Routes) {
		r := g.Routes[id]
		if len(r.Cells) < 2 || noneHere(g, r, snap) {
			continue
		}
		key := "net " + r.Net
		if r.Net == "" {
			key = "edge " + edgeOf(r)
		}
		w := byKey[key]
		if w == nil {
			w = &wire{key: key}
			byKey[key] = w
			wires = append(wires, w)
		}
		lk := lookOf(r, snap, opts, lit, annotated)
		w.routes = append(w.routes, r)
		w.looks = append(w.looks, lk)
		if lk.rank > w.rank {
			w.rank = lk.rank
		}
	}
	sort.SliceStable(wires, func(i, j int) bool {
		if wires[i].rank != wires[j].rank {
			return wires[i].rank < wires[j].rank
		}
		return wires[i].key < wires[j].key
	})

	type head struct {
		ch rune
		st Style
	}
	heads := map[Point]head{}
	for _, w := range wires {
		links := map[Point]int{}
		best := map[Point]int{} // cell -> index of the look that wins it
		for i, r := range w.routes {
			cells := r.Cells
			for k, p := range cells {
				if k > 0 {
					links[cells[k-1]] |= bit(cells[k-1], p)
					links[p] |= bit(p, cells[k-1])
				}
				if j, ok := best[p]; !ok || w.looks[i].rank > w.looks[j].rank {
					best[p] = i
				}
			}
		}
		for p, d := range links {
			lk := w.looks[best[p]]
			ch := junctionChar(d)
			if lk.es.Marker == model.MarkerNoData || lk.es.Marker == model.MarkerUnbound {
				if ch == '─' {
					ch = '┄'
				} else if ch == '│' {
					ch = '┆'
				}
			} else if lk.dashed {
				if ch == '─' {
					ch = '╌'
				} else if ch == '│' {
					ch = '╎'
				}
			}
			c.Set(p.X, p.Y, ch, lk.st)
		}
		for i, r := range w.routes {
			n := len(r.Cells)
			heads[r.Cells[n-1]] = head{arrow(r.Cells[n-2], r.Cells[n-1]), w.looks[i].st}
			if r.Both {
				heads[r.Cells[0]] = head{arrow(r.Cells[1], r.Cells[0]), w.looks[i].st}
			}
		}
	}
	for p, h := range heads {
		c.Set(p.X, p.Y, h.ch, h.st)
	}

	for _, w := range wires {
		for i, r := range w.routes {
			lk := w.looks[i]
			es, st := lk.es, lk.st
			cells := r.Cells
			n := len(cells)
			// Animation: a bright dot moving along a flowing edge; speed
			// scales with rate. One dot an edge, however many routes draw it.
			if es.State == model.Flowing && opts.Animate && es.Marker == "" && n > 3 && !moving[edgeOf(r)] {
				moving[edgeOf(r)] = true
				step := speedStep(es.Rate)
				if step > 0 {
					pos := (opts.Frame / step) % (n - 1)
					if pos > 0 {
						p := cells[pos]
						c.Set(p.X, p.Y, '●', Style{Fg: st.Fg, Bold: true, Dim: st.Dim})
					}
				}
			}
			if es.State == model.Processing && opts.Animate && n > 2 {
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
			// One label per edge, drawn later. Off the lit path, and on
			// healthy plumbing edges (tcp health checks, replication), a
			// label is noise.
			if labelled[edgeOf(r)] || (opts.Lens != nil && !lk.lit) || !edgeLabelWorthIt(g, r, es, opts) {
				continue
			}
			labelled[edgeOf(r)] = true
			labels = append(labels, func() { labelRoute(c, g, r, es, st, opts) })
		}
	}
	return labels
}

// edgeLabelWorthIt decides whether a flowing edge deserves a rate label.
func edgeLabelWorthIt(g *layout.Graph, r *layout.Route, es model.ElementState, opts DrawOptions) bool {
	if opts.Detail == model.DetailFull {
		return true
	}
	if es.Marker == model.MarkerUnbound {
		return false // the dashed line says it
	}
	if es.State != model.Flowing || es.Marker != "" {
		return true // anything not plainly flowing is signal
	}
	if opts.Detail == model.DetailMinimal {
		return false
	}
	kind := ""
	if opts.Topology != nil {
		if e, ok := opts.Topology.Edge(edgeOf(r)); ok {
			kind = e.Kind
		}
	}
	switch kind {
	case "tcp", "replication":
		return false
	}
	return true
}

// edgeOf is the logical edge a route draws.
func edgeOf(r *layout.Route) string {
	if r.Edge != "" {
		return r.Edge
	}
	return r.ID
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

func dir(a, b Point) byte {
	if a.Y == b.Y {
		return 'h'
	}
	return 'v'
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
	if r.Net != "" {
		// The routes of a net share their trunk: the stretch that belongs to
		// this edge alone is the one at the box it reaches, which is the
		// last of a net that leaves a component and the first of one that
		// arrives at it.
		if !strings.HasPrefix(r.Net, "in:") {
			for a, b := 0, len(segs)-1; a < b; a, b = a+1, b-1 {
				segs[a], segs[b] = segs[b], segs[a]
			}
		}
	} else {
		for a := 1; a < len(segs); a++ {
			for b := a; b > 0 && (segs[b].end-segs[b].start) > (segs[b-1].end-segs[b-1].start); b-- {
				segs[b], segs[b-1] = segs[b-1], segs[b]
			}
		}
	}
	lst := Style{Fg: st.Fg, Dim: st.Dim}
	// free reports whether the text and a cell of air on either side of it
	// fit at (x, y).
	free := func(x, y int) bool {
		return x >= 1 && x+w < c.W && !rowBusy(c, x, y, w) && !overBox(g, x, y, w)
	}
	for _, sg := range segs {
		horizontal := dir(r.Cells[sg.start], r.Cells[sg.start+1]) == 'h'
		mid := (sg.start + sg.end) / 2
		for d := 0; d <= sg.end-sg.start; d++ {
			k := mid + d/2
			if d%2 == 1 {
				k = mid - (d+1)/2
			}
			if k < sg.start || k > sg.end {
				continue
			}
			p := r.Cells[k]
			if horizontal {
				x := p.X - w/2
				for _, dy := range []int{-1, 1} {
					if free(x, p.Y+dy) {
						c.Text(x, p.Y+dy, text, lst, w)
						return
					}
				}
				continue
			}
			// beside a line that runs down: a cell of air, then the text
			for _, x := range []int{p.X + 2, p.X - 2 - w} {
				if free(x, p.Y) {
					c.Text(x, p.Y, text, lst, w)
					return
				}
			}
		}
	}
}

// overBox reports whether a label of width w at (x, y) would touch a box.
// Boxes are painted after labels, so a label allowed there would be cut.
func overBox(g *layout.Graph, x, y, w int) bool {
	for _, b := range g.Boxes {
		if y >= b.Y && y < b.Bottom() && x+w > b.X-1 && x-1 < b.Right() {
			return true
		}
	}
	return false
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
			s := state.Rate(es.Rate, es.Unit)
			if strings.Contains(es.Label, "miss") {
				parts := strings.Split(es.Label, ", ")
				s += " · " + strings.Replace(parts[len(parts)-1], " percent", "%", 1)
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
