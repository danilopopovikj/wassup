package render

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/danilopopovikj/wassup/internal/app"
	"github.com/danilopopovikj/wassup/internal/history"
	"github.com/danilopopovikj/wassup/internal/layout"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/state"
)

// Panel modes.
const (
	panelDetail = iota
	panelFindings
	panelEvents
	panelAnnotations
	panelHelp
)

// Options configure the TUI.
type Options struct {
	NoColor bool
	// Split is the graph width in percent (default 65, or layout.json's).
	Split int
}

type updateMsg app.Update
type frameMsg time.Time
type toastMsg string

type dragState struct {
	id           string
	startX       int
	startY       int
	origX, origY int
	origW, origH int
	resize       bool
	moved        bool
	lastRoute    time.Time
}

// Model is the Bubble Tea model of the whole screen.
type Model struct {
	rt   *app.Runtime
	opts Options

	cfg   *model.Config
	snap  *model.Snapshot // live
	view  *model.Snapshot // displayed (live or a replay frame)
	graph *layout.Graph
	annot model.Annotations

	width, height int
	split         int
	panelOpen     bool
	panelMode     int
	panelScroll   int
	selected      string
	filter        string
	filtering     bool

	lensOn    bool
	issueIdx  int
	autoLens  bool
	lastCrit  bool
	scrubbing bool
	scrubAt   time.Time

	frame   int
	animate bool
	drag    *dragState
	viewX   int
	viewY   int

	toast      string
	toastUntil time.Time
	confirm    string
	confirmAt  time.Time

	layoutDirty  bool
	layoutSaveAt time.Time
	quitting     bool
}

// New builds the model for a runtime.
func New(rt *app.Runtime, opts Options) *Model {
	cfg := rt.Config()
	m := &Model{rt: rt, opts: opts, cfg: cfg, animate: true, autoLens: cfg.Topology.Settings.AutoLens}
	m.split = opts.Split
	if m.split == 0 {
		m.split = cfg.Layout.GraphSplit
	}
	if m.split < 30 || m.split > 90 {
		m.split = 65
	}
	m.annot = rt.Annotations()
	m.snap = rt.Snapshot()
	m.view = m.snap
	m.relayout()
	return m
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.waitUpdate(), tick(100*time.Millisecond), func() tea.Msg { return tea.RequestWindowSize() })
}

func (m *Model) waitUpdate() tea.Cmd {
	return func() tea.Msg {
		u, ok := <-m.rt.Updates()
		if !ok {
			return nil
		}
		return updateMsg(u)
	}
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return frameMsg(t) })
}

func (m *Model) relayout() {
	opts := layout.DefaultOptions()
	if m.height > 0 && m.height < 30 {
		opts.Compact = true
	}
	opts.Detail = m.cfg.Layout.DetailLevel()
	m.graph = layout.Compute(&m.cfg.Topology, m.cfg.Layout, opts)
	if m.selected == "" {
		if len(m.graph.Order) > 0 {
			m.selected = m.graph.Order[0]
		}
	}
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		compactBefore := m.height > 0 && m.height < 30
		m.width, m.height = msg.Width, msg.Height
		if (m.height < 30) != compactBefore {
			m.relayout()
		}
		return m, nil
	case updateMsg:
		return m.onUpdate(app.Update(msg))
	case frameMsg:
		m.frame++
		var cmds []tea.Cmd
		if m.layoutDirty && time.Since(m.layoutSaveAt) > 300*time.Millisecond && m.drag == nil {
			m.layoutDirty = false
			if err := m.rt.SaveLayout(m.cfg.Layout); err != nil {
				m.setToast("layout not saved: " + err.Error())
			}
		}
		if m.toast != "" && time.Now().After(m.toastUntil) {
			m.toast = ""
		}
		if m.confirm != "" && time.Since(m.confirmAt) > 3*time.Second {
			m.confirm = ""
		}
		d := 100 * time.Millisecond
		if !m.hasFlow() {
			d = 500 * time.Millisecond
		}
		cmds = append(cmds, tick(d))
		return m, tea.Batch(cmds...)
	case toastMsg:
		m.setToast(string(msg))
		return m, nil
	case tea.KeyPressMsg:
		return m.onKey(msg)
	case tea.MouseClickMsg:
		return m.onClick(msg.Mouse())
	case tea.MouseReleaseMsg:
		return m.onRelease(msg.Mouse())
	case tea.MouseMotionMsg:
		return m.onMotion(msg.Mouse())
	case tea.MouseWheelMsg:
		mm := msg.Mouse()
		if m.inPanel(mm.X, mm.Y) {
			if mm.Button == tea.MouseWheelUp {
				m.panelScroll -= 3
			} else if mm.Button == tea.MouseWheelDown {
				m.panelScroll += 3
			}
			if m.panelScroll < 0 {
				m.panelScroll = 0
			}
		} else {
			if mm.Button == tea.MouseWheelUp {
				m.viewY -= 2
			} else if mm.Button == tea.MouseWheelDown {
				m.viewY += 2
			}
			m.clampView()
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) hasFlow() bool {
	if m.view == nil {
		return false
	}
	for _, e := range m.view.Edges {
		if e.State == model.Flowing || e.State == model.Processing {
			return true
		}
	}
	return false
}

func (m *Model) onUpdate(u app.Update) (tea.Model, tea.Cmd) {
	if u.Config != nil {
		m.cfg = u.Config
		m.autoLens = m.cfg.Topology.Settings.AutoLens
		m.relayout()
	}
	if u.Annotations != nil {
		m.annot = *u.Annotations
	}
	if u.Snapshot != nil {
		m.snap = u.Snapshot
		if !m.scrubbing {
			m.view = m.snap
		}
		// Auto lens: turn on when a crit appears.
		crit := false
		for _, is := range m.snap.Issues {
			if is.Severity == model.Crit {
				crit = true
				break
			}
		}
		if crit && !m.lastCrit && m.autoLens {
			m.lensOn = true
			m.issueIdx = 0
		}
		m.lastCrit = crit
		if m.issueIdx >= len(m.snap.Issues) {
			m.issueIdx = 0
		}
	}
	if u.Toast != "" {
		m.setToast(u.Toast)
	}
	return m, m.waitUpdate()
}

func (m *Model) setToast(s string) {
	m.toast = s
	m.toastUntil = time.Now().Add(4 * time.Second)
}

// ---- geometry -------------------------------------------------------------

func (m *Model) stripRows() int {
	if m.lensOn && m.currentIssue() != nil {
		n := len(m.currentIssue().Story) + 1
		if n < 3 {
			n = 3
		}
		if n > 7 {
			n = 7
		}
		return n
	}
	return 3
}

func (m *Model) graphRect() (x, y, w, h int) {
	h = m.height - 1 - m.stripRows()
	if h < 1 {
		h = 1
	}
	w = m.width
	if m.panelOpen {
		w = m.width * m.split / 100
	}
	return 0, 1, w, h
}

func (m *Model) panelRect() (x, y, w, h int) {
	gx, gy, gw, gh := m.graphRect()
	return gx + gw, gy, m.width - gw, gh
}

func (m *Model) inPanel(x, y int) bool {
	if !m.panelOpen {
		return false
	}
	px, py, pw, ph := m.panelRect()
	return x >= px && x < px+pw && y >= py && y < py+ph
}

func (m *Model) toGraph(x, y int) (int, int, bool) {
	gx, gy, gw, gh := m.graphRect()
	if x < gx || x >= gx+gw || y < gy || y >= gy+gh {
		return 0, 0, false
	}
	return x - gx + m.viewX, y - gy + m.viewY, true
}

func (m *Model) clampView() {
	_, _, gw, gh := m.graphRect()
	maxX := m.graph.W - gw
	maxY := m.graph.H - gh
	if maxX < 0 {
		maxX = 0
	}
	if maxY < 0 {
		maxY = 0
	}
	if m.viewX > maxX {
		m.viewX = maxX
	}
	if m.viewY > maxY {
		m.viewY = maxY
	}
	if m.viewX < 0 {
		m.viewX = 0
	}
	if m.viewY < 0 {
		m.viewY = 0
	}
}

// follow scrolls the viewport so the selected box is visible.
func (m *Model) follow() {
	b, ok := m.graph.Boxes[m.graph.BoxOf[m.selected]]
	if !ok {
		if e, isEdge := m.cfg.Topology.Edge(m.selected); isEdge {
			b, ok = m.graph.Boxes[m.graph.BoxOf[e.To]]
		}
	}
	if !ok || b == nil {
		return
	}
	_, _, gw, gh := m.graphRect()
	if b.X < m.viewX+2 {
		m.viewX = b.X - 2
	}
	if b.Right() > m.viewX+gw-2 {
		m.viewX = b.Right() - gw + 2
	}
	if b.Y < m.viewY+2 {
		m.viewY = b.Y - 2
	}
	if b.Bottom() > m.viewY+gh-1 {
		m.viewY = b.Bottom() - gh + 1
	}
	m.clampView()
}

func (m *Model) currentIssue() *model.Issue {
	if m.view == nil || len(m.view.Issues) == 0 {
		return nil
	}
	i := m.issueIdx
	if i >= len(m.view.Issues) {
		i = 0
	}
	return &m.view.Issues[i]
}

// ---- keys -----------------------------------------------------------------

func (m *Model) onKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if m.filtering {
		switch key {
		case "esc":
			m.filtering, m.filter = false, ""
		case "enter":
			m.filtering = false
		case "backspace":
			if len(m.filter) > 0 {
				m.filter = m.filter[:len(m.filter)-1]
			}
		default:
			if k.Text != "" && len(k.Text) == 1 {
				m.filter += k.Text
				m.selectFirstMatch()
			}
		}
		return m, nil
	}
	switch key {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "left", "h":
		m.moveSelection(-1, 0)
	case "right", "l":
		m.moveSelection(1, 0)
	case "up", "k":
		m.moveSelection(0, -1)
	case "down", "j":
		m.moveSelection(0, 1)
	case "shift+left", "H":
		m.viewX -= 8
		m.clampView()
	case "shift+right", "L":
		m.viewX += 8
		m.clampView()
	case "shift+up", "K":
		m.viewY -= 4
		m.clampView()
	case "shift+down", "J":
		m.viewY += 4
		m.clampView()
	case "enter":
		m.panelOpen = true
		m.panelMode = panelDetail
		m.panelScroll = 0
	case "tab":
		m.panelOpen = !m.panelOpen
	case "i":
		m.lensOn = !m.lensOn
		if m.lensOn && m.currentIssue() == nil {
			m.lensOn = false
			m.setToast("nothing to focus on: no warn or crit")
		}
	case "n":
		if m.view != nil && len(m.view.Issues) > 0 {
			m.issueIdx = (m.issueIdx + 1) % len(m.view.Issues)
			m.lensOn = true
			if is := m.currentIssue(); is != nil {
				m.selected = is.Cause
				m.follow()
			}
		}
	case "t":
		m.scrubbing = !m.scrubbing
		if m.scrubbing {
			if _, latest, ok := m.rt.Ring().Bounds(); ok {
				m.scrubAt = latest
			} else {
				m.scrubAt = m.rt.Now()
			}
			m.showFrame()
		} else {
			m.view = m.snap
		}
	case "[":
		if m.scrubbing {
			m.scrubAt = m.scrubAt.Add(-time.Minute)
			m.showFrame()
		}
	case "]":
		if m.scrubbing {
			m.scrubAt = m.scrubAt.Add(time.Minute)
			m.showFrame()
		}
	case "{":
		if m.scrubbing {
			m.scrubAt = m.scrubAt.Add(-10 * time.Minute)
			m.showFrame()
		}
	case "}":
		if m.scrubbing {
			m.scrubAt = m.scrubAt.Add(10 * time.Minute)
			m.showFrame()
		}
	case "esc":
		switch {
		case m.scrubbing:
			m.scrubbing = false
			m.view = m.snap
		case m.filter != "":
			m.filter = ""
		case m.panelOpen:
			m.panelOpen = false
		case m.lensOn:
			m.lensOn = false
		}
	case "c":
		ref := m.selectedRef()
		m.setToast("copied " + ref)
		return m, copyCmd(ref)
	case "C":
		s := m.selectedSummary()
		m.setToast("copied summary")
		return m, copyCmd(s)
	case "y":
		lines := m.panelLines(200)
		m.setToast("copied detail panel")
		return m, copyCmd(strings.Join(lines, "\n"))
	case "r":
		if m.confirm == "r" {
			m.confirm = ""
			delete(m.cfg.Layout.Components, m.selected)
			m.relayout()
			m.markLayoutDirty()
			m.setToast("layout reset for " + m.selected)
		} else {
			m.confirm, m.confirmAt = "r", time.Now()
			m.setToast("reset layout of " + m.selected + "? press r again")
		}
	case "R":
		if m.confirm == "R" {
			m.confirm = ""
			m.cfg.Layout.Components = map[string]model.Placement{}
			m.cfg.Layout.Collapsed = nil
			m.cfg.Layout.Waypoints = nil
			m.relayout()
			m.markLayoutDirty()
			m.setToast("layout reset to auto for everything")
		} else {
			m.confirm, m.confirmAt = "R", time.Now()
			m.setToast("reset the whole layout? press R again")
		}
	case "f":
		m.togglePanel(panelFindings)
	case "e":
		m.togglePanel(panelEvents)
	case "a":
		m.togglePanel(panelAnnotations)
	case "?":
		m.togglePanel(panelHelp)
	case "/":
		m.filtering = true
		m.filter = ""
	case "x":
		m.clearAnnotation(false)
	case "X":
		m.clearAnnotation(true)
	case "s":
		return m, m.export()
	case "+", "=":
		if m.split < 90 {
			m.split += 5
			m.cfg.Layout.GraphSplit = m.split
			m.markLayoutDirty()
		}
	case "-":
		if m.split > 30 {
			m.split -= 5
			m.cfg.Layout.GraphSplit = m.split
			m.markLayoutDirty()
		}
	case "g":
		m.toggleCollapse(m.selected)
	case "d":
		next := map[string]string{model.DetailMinimal: model.DetailNormal, model.DetailNormal: model.DetailFull, model.DetailFull: model.DetailMinimal}
		m.cfg.Layout.Detail = next[m.cfg.Layout.DetailLevel()]
		m.relayout()
		m.markLayoutDirty()
		m.setToast("detail: " + m.cfg.Layout.DetailLevel() + " (d cycles, enter opens the full panel)")
	case "pgdown":
		m.panelScroll += 10
	case "pgup":
		m.panelScroll -= 10
		if m.panelScroll < 0 {
			m.panelScroll = 0
		}
	}
	return m, nil
}

func (m *Model) togglePanel(mode int) {
	if m.panelOpen && m.panelMode == mode {
		m.panelOpen = false
		return
	}
	m.panelOpen = true
	m.panelMode = mode
	m.panelScroll = 0
}

func (m *Model) markLayoutDirty() {
	m.layoutDirty = true
	m.layoutSaveAt = time.Now()
}

func (m *Model) showFrame() {
	f, ok := m.rt.Ring().At(m.scrubAt)
	if !ok {
		m.setToast("no history at " + m.scrubAt.Format("15:04"))
		return
	}
	snap := history.ToSnapshot(f, m.cfg.Topology.Name)
	// Rebuild gauges from the frame's metrics so the picture is complete.
	for id, es := range snap.Components {
		if comp, ok := m.cfg.Topology.Component(id); ok {
			es.Gauges = state.Gauges(m.cfg.Thresholds, comp, es)
			snap.Components[id] = es
		}
	}
	// Issues are recomputed cheaply from the frame states.
	snap.Issues = m.snap.Issues
	m.view = snap
}

func (m *Model) toggleCollapse(id string) {
	if _, ok := m.cfg.Topology.Group(id); !ok {
		// selected box inside a group: collapse its group
		if c, ok := m.cfg.Topology.Component(id); ok && c.Group != "" {
			id = c.Group
		} else {
			return
		}
	}
	var kept []string
	found := false
	for _, g := range m.cfg.Layout.Collapsed {
		if g == id {
			found = true
			continue
		}
		kept = append(kept, g)
	}
	if !found {
		kept = append(kept, id)
	}
	m.cfg.Layout.Collapsed = kept
	m.relayout()
	m.markLayoutDirty()
	if found {
		m.setToast("expanded " + id)
	} else {
		m.setToast("collapsed " + id)
		m.selected = id
	}
}

func (m *Model) clearAnnotation(all bool) {
	live := m.annot.Live(m.rt.Now())
	if len(live) == 0 {
		m.setToast("no annotations")
		return
	}
	var kept []model.Annotation
	if !all {
		removed := false
		for _, an := range live {
			hit := false
			for _, ref := range an.Path {
				if r, err := model.ParseRef(ref); err == nil && r.ID == m.selected {
					hit = true
				}
			}
			if hit && !removed {
				removed = true
				continue
			}
			kept = append(kept, an)
		}
		if !removed && len(live) > 0 {
			kept = live[1:]
		}
	}
	m.annot = model.Annotations{Version: model.Version, Annotations: kept}
	if err := m.rt.SetAnnotations(m.annot); err != nil {
		m.setToast("annotations not saved: " + err.Error())
		return
	}
	if all {
		m.setToast("cleared all annotations")
	} else {
		m.setToast("cleared annotation")
	}
}

func (m *Model) selectFirstMatch() {
	if m.filter == "" {
		return
	}
	f := strings.ToLower(m.filter)
	for _, id := range m.graph.Order {
		lbl := strings.ToLower(m.cfg.Topology.LabelOf(id))
		if strings.Contains(id, f) || strings.Contains(lbl, f) {
			m.selected = id
			m.follow()
			return
		}
	}
}

// moveSelection picks the nearest box in a direction.
func (m *Model) moveSelection(dx, dy int) {
	cur, ok := m.graph.Boxes[m.graph.BoxOf[m.selected]]
	if !ok {
		if e, isEdge := m.cfg.Topology.Edge(m.selected); isEdge {
			cur = m.graph.Boxes[m.graph.BoxOf[e.To]]
		}
	}
	if cur == nil {
		if len(m.graph.Order) > 0 {
			m.selected = m.graph.Order[0]
		}
		return
	}
	cx, cy := cur.Center()
	best := ""
	bestScore := 1 << 30
	for _, id := range m.graph.Order {
		if id == cur.ID {
			continue
		}
		b := m.graph.Boxes[id]
		bx, by := b.Center()
		vx, vy := bx-cx, by-cy
		// must be in the requested half-plane
		if dx != 0 && vx*dx <= 0 {
			continue
		}
		if dy != 0 && vy*dy <= 0 {
			continue
		}
		var score int
		if dx != 0 {
			score = abs(vx) + 3*abs(vy)
		} else {
			score = abs(vy) + abs(vx)/2
		}
		if score < bestScore {
			best, bestScore = id, score
		}
	}
	if best != "" {
		m.selected = best
		m.follow()
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---- mouse ----------------------------------------------------------------

func (m *Model) onClick(mm tea.Mouse) (tea.Model, tea.Cmd) {
	if mm.Button != tea.MouseLeft {
		return m, nil
	}
	// timeline click → scrub
	sy := m.height - m.stripRows()
	if mm.Y >= sy && !m.lensOn {
		barX := 7
		barW := m.width - barX
		if mm.X >= barX && barW > 0 {
			frac := float64(mm.X-barX) / float64(barW)
			now := m.rt.Now()
			m.scrubbing = true
			m.scrubAt = now.Add(-TimelineSpan).Add(time.Duration(frac * float64(TimelineSpan)))
			m.showFrame()
		}
		return m, nil
	}
	if m.inPanel(mm.X, mm.Y) {
		return m, nil
	}
	gx, gy, ok := m.toGraph(mm.X, mm.Y)
	if !ok {
		return m, nil
	}
	if gf := m.graph.GroupTitleAt(gx, gy); gf != nil {
		m.toggleCollapse(gf.ID)
		return m, nil
	}
	if b := m.graph.BoxAt(gx, gy); b != nil {
		m.selected = b.ID
		resize := gx >= b.Right()-2 && gy >= b.Bottom()-1
		m.drag = &dragState{id: b.ID, startX: gx, startY: gy, origX: b.X, origY: b.Y, origW: b.W, origH: b.H, resize: resize}
		return m, nil
	}
	if r := m.graph.RouteAt(gx, gy); r != nil {
		m.selected = r.ID
		m.panelMode = panelDetail
	}
	return m, nil
}

func (m *Model) onMotion(mm tea.Mouse) (tea.Model, tea.Cmd) {
	if m.drag == nil || mm.Button != tea.MouseLeft {
		return m, nil
	}
	gx, gy, ok := m.toGraph(mm.X, mm.Y)
	if !ok {
		return m, nil
	}
	dx, dy := gx-m.drag.startX, gy-m.drag.startY
	if dx == 0 && dy == 0 {
		return m, nil
	}
	m.drag.moved = true
	b := m.graph.Boxes[m.drag.id]
	if b == nil {
		return m, nil
	}
	p := m.cfg.Layout.Components[m.drag.id]
	if m.drag.resize {
		p.X, p.Y = m.drag.origX, m.drag.origY
		p.W = max(18, m.drag.origW+dx)
		p.H = max(4, m.drag.origH+dy)
	} else {
		p.X = max(0, m.drag.origX+dx)
		p.Y = max(0, m.drag.origY+dy)
		if p.W == 0 {
			p.W, p.H = b.W, b.H
		}
	}
	p.Auto = false
	if m.cfg.Layout.Components == nil {
		m.cfg.Layout.Components = map[string]model.Placement{}
	}
	m.cfg.Layout.Components[m.drag.id] = p
	// Move the box immediately; re-route at most every 100 ms.
	b.X, b.Y, b.W, b.H = p.X, p.Y, max(p.W, 18), max(p.H, 4)
	if time.Since(m.drag.lastRoute) > 100*time.Millisecond {
		m.relayout()
		m.drag.lastRoute = time.Now()
	}
	return m, nil
}

func (m *Model) onRelease(mm tea.Mouse) (tea.Model, tea.Cmd) {
	if m.drag == nil {
		return m, nil
	}
	moved := m.drag.moved
	m.drag = nil
	if moved {
		m.relayout()
		m.markLayoutDirty()
	}
	return m, nil
}

// ---- refs and copies ------------------------------------------------------

func (m *Model) selectedRef() string {
	var r model.Ref
	if e, ok := m.cfg.Topology.Edge(m.selected); ok {
		r = model.EdgeRef(e.From, e.To)
	} else if c, ok := m.cfg.Topology.Component(m.selected); ok {
		r = model.ComponentRef(c)
	} else if _, ok := m.cfg.Topology.Group(m.selected); ok {
		r = model.Ref{Kind: "group", ID: m.selected}
	} else {
		r = model.Ref{Kind: "component", ID: m.selected}
	}
	if m.scrubbing {
		r.At = m.scrubAt
	}
	return r.String()
}

func (m *Model) selectedState() (model.ElementState, bool) {
	if m.view == nil {
		return model.ElementState{}, false
	}
	if es, ok := m.view.Components[m.selected]; ok {
		return es, true
	}
	es, ok := m.view.Edges[m.selected]
	return es, ok
}

func (m *Model) selectedSummary() string {
	es, _ := m.selectedState()
	parts := []string{m.selectedRef(), m.cfg.Topology.LabelOf(m.selected) + ": " + es.Label}
	var keys []string
	for k := range es.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ms []string
	for _, k := range keys {
		ms = append(ms, fmt.Sprintf("%s=%s", k, state.Num(es.Metrics[k])))
	}
	if len(ms) > 0 {
		parts = append(parts, strings.Join(ms, " "))
	}
	return strings.Join(parts, " | ")
}

func (m *Model) export() tea.Cmd {
	dir, err := m.rt.Store().ExportDir()
	if err != nil {
		m.setToast("export: " + err.Error())
		return nil
	}
	c := m.graphCanvas()
	var story []string
	if is := m.currentIssue(); is != nil && m.lensOn {
		story = is.Story
	}
	res := Export(dir, c, story, m.rt.Now())
	if res.Err != "" && res.PNG == "" {
		m.setToast("exported " + res.SVG + " (" + res.Err + ")")
		return copyCmd(res.SVG)
	}
	m.setToast("exported " + res.PNG)
	return copyCmd(res.PNG)
}

// ---- view -----------------------------------------------------------------

func (m *Model) drawOpts() DrawOptions {
	opts := DrawOptions{Selected: m.selected, Frame: m.frame, Topology: &m.cfg.Topology, Snapshot: m.view, Animate: m.animate && !m.scrubbing, Detail: m.cfg.Layout.DetailLevel()}
	if m.lensOn {
		opts.Lens = m.currentIssue()
	}
	opts.Annotations = m.annot.Live(m.rt.Now())
	return opts
}

// graphCanvas draws the full diagram (not the viewport).
func (m *Model) graphCanvas() *Canvas {
	c := Draw(m.graph, m.drawOpts())
	c.NoColor = m.opts.NoColor
	if m.filter != "" {
		f := strings.ToLower(m.filter)
		for _, id := range m.graph.Order {
			lbl := strings.ToLower(m.cfg.Topology.LabelOf(id))
			if !strings.Contains(id, f) && !strings.Contains(lbl, f) {
				b := m.graph.Boxes[id]
				c.Dim(b.X, b.Y, b.W, b.H)
			}
		}
	}
	return c
}

// View implements tea.Model.
func (m *Model) View() tea.View {
	if m.width == 0 || m.height == 0 {
		v := tea.NewView("loading…")
		v.AltScreen = true
		return v
	}
	screen := m.Screen()
	v := tea.NewView(screen.String())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "wassup · " + m.cfg.Topology.Name
	return v
}

// Screen paints the whole terminal frame: status line, graph viewport,
// panel and the timeline or story strip. Export and tests use it too.
func (m *Model) Screen() *Canvas {
	screen := NewCanvas(m.width, m.height)
	screen.NoColor = m.opts.NoColor
	m.drawStatus(screen)
	gx, gy, gw, gh := m.graphRect()
	full := m.graphCanvas()
	m.clampView()
	sub := full.Sub(m.viewX, m.viewY, gw, gh)
	for y := 0; y < gh; y++ {
		for x := 0; x < gw; x++ {
			screen.Cells[(gy+y)*screen.W+gx+x] = sub.Cells[y*gw+x]
		}
	}
	// scroll hints
	if m.viewX > 0 {
		screen.Text(gx, gy+gh/2, "◀", Style{Fg: ColCyan, Bold: true}, 1)
	}
	if m.viewX+gw < full.W {
		screen.Text(gx+gw-1, gy+gh/2, "▶", Style{Fg: ColCyan, Bold: true}, 1)
	}
	if m.viewY > 0 {
		screen.Text(gx+gw/2, gy, "▲", Style{Fg: ColCyan, Bold: true}, 1)
	}
	if m.viewY+gh < full.H {
		screen.Text(gx+gw/2, gy+gh-1, "▼", Style{Fg: ColCyan, Bold: true}, 1)
	}
	if m.panelOpen {
		m.drawPanel(screen)
	}
	m.drawStrip(screen)
	if m.toast != "" {
		t := " " + m.toast + " "
		x := (m.width - len([]rune(t))) / 2
		if x < 0 {
			x = 0
		}
		screen.Text(x, m.height-m.stripRows()-1, t, Style{Fg: ColWhite, Inverse: true, Bold: true}, m.width)
	}
	if m.filtering || m.filter != "" {
		t := " /" + m.filter + "_ "
		screen.Text(0, m.height-m.stripRows()-1, t, Style{Fg: ColCyan, Inverse: true}, m.width)
	}
	return screen
}

func (m *Model) drawStatus(c *Canvas) {
	c.Fill(0, 0, m.width, 1, ' ', Style{Inverse: true})
	name := " " + m.cfg.Topology.Name + " "
	x := c.Text(0, 0, name, Style{Inverse: true, Bold: true}, m.width)
	if m.view != nil {
		worst, warn, crit := state.WorstSeverity(m.view)
		sev := "all clear"
		if warn > 0 || crit > 0 {
			var parts []string
			if warn > 0 {
				parts = append(parts, fmt.Sprintf("%d warn", warn))
			}
			if crit > 0 {
				parts = append(parts, fmt.Sprintf("%d crit", crit))
			}
			sev = strings.Join(parts, ", ")
		}
		x += c.Text(x, 0, " "+sev+" ", Style{Inverse: true, Fg: SeverityColor(worst), Bold: true}, m.width-x)
		// probe health dots
		var kinds []string
		for k := range m.view.ProbeHealth {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		if len(kinds) > 0 {
			x += c.Text(x, 0, " probes ", Style{Inverse: true}, m.width-x)
			for _, k := range kinds {
				h := m.view.ProbeHealth[k]
				col := ColGreen
				if strings.HasPrefix(h, "degraded") {
					col = ColAmber
				} else if strings.HasPrefix(h, "failed") {
					col = ColRed
				}
				x += c.Text(x, 0, "●", Style{Inverse: true, Fg: col}, 1)
			}
			x += c.Text(x, 0, " ", Style{Inverse: true}, 1)
		}
		age := m.rt.Now().Sub(m.view.GeneratedAt)
		if age < 0 {
			age = 0
		}
		x += c.Text(x, 0, fmt.Sprintf(" tick %ds ago ", int(age.Seconds())), Style{Inverse: true, Dim: age > 15*time.Second}, m.width-x)
	}
	if m.scrubbing {
		lbl := " REPLAY " + m.scrubAt.Format("15:04") + " "
		c.Text(x+1, 0, lbl, Style{Fg: ColAmber, Bold: true}, m.width-x)
		x += len(lbl) + 1
	} else if m.rt.Replaying() {
		lbl := " fixture "
		c.Text(x+1, 0, lbl, Style{Fg: ColBlue, Inverse: true}, m.width-x)
		x += len(lbl) + 1
	}
	if m.lensOn {
		if is := m.currentIssue(); is != nil {
			lbl := fmt.Sprintf(" lens %d/%d · cause: %s ", m.issueIdx+1, len(m.view.Issues), m.cfg.Topology.LabelOf(is.Cause))
			x += c.Text(x+1, 0, lbl, Style{Fg: ColAmber, Inverse: true, Bold: true}, m.width-x-1) + 1
		}
	}
	if lvl := m.cfg.Layout.DetailLevel(); lvl != model.DetailNormal {
		c.Text(x+1, 0, " detail: "+lvl+" ", Style{Inverse: true, Dim: true}, m.width-x-1)
	}
	clock := " " + m.rt.Now().Format("15:04:05") + " "
	c.Text(m.width-len(clock), 0, clock, Style{Inverse: true}, len(clock))
	help := " ? keys "
	if m.width-len(clock)-len(help) > x {
		c.Text(m.width-len(clock)-len(help), 0, help, Style{Inverse: true, Dim: true}, len(help))
	}
}

func (m *Model) drawStrip(c *Canvas) {
	rows := m.stripRows()
	y := m.height - rows
	c.Fill(0, y, m.width, rows, ' ', Style{})
	if m.lensOn {
		if is := m.currentIssue(); is != nil {
			hdr := fmt.Sprintf(" story · %s · press n for the next issue, i to leave the lens ", m.cfg.Topology.LabelOf(is.ID))
			c.Text(0, y, hdr, Style{Fg: ColAmber, Bold: true}, m.width)
			for i, line := range is.Story {
				if y+1+i >= m.height {
					break
				}
				st := Style{}
				prefix := "  "
				isCause := i == len(is.Story)-1 || (i == len(is.Story)-2 && strings.HasPrefix(is.Story[len(is.Story)-1], "last change"))
				if strings.HasPrefix(line, "last change") {
					st = Style{Fg: ColCyan}
					prefix = "  "
					isCause = false
				}
				if isCause {
					st = Style{Fg: SeverityColor(is.Severity), Bold: true}
					prefix = "▸ "
				}
				c.Text(0, y+1+i, prefix+ellipsis(line, m.width-3), st, m.width)
			}
			return
		}
	}
	drawTimeline(c, 0, y, m.width, m.rt.Ring(), m.rt.Events(), m.rt.Now(), m.scrubAt, m.scrubbing)
}
