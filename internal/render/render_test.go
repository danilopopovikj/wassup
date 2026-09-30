package render

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/danilopopovikj/wassup/internal/app"
	"github.com/danilopopovikj/wassup/internal/layout"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/fixture"
	"github.com/danilopopovikj/wassup/internal/scenario"
)

// TestDrawScenario renders a played scenario and checks the picture carries
// the labels, the lens dimming and the waiting pile.
func TestDrawScenario(t *testing.T) {
	run, err := scenario.Load("../../testdata/scenarios/02-connection-pool-exhausted")
	if err != nil {
		t.Fatal(err)
	}
	snap := run.Play()
	g := layout.Compute(&run.Config.Topology, run.Config.Layout, layout.DefaultOptions())
	opts := DrawOptions{Topology: &run.Config.Topology, Snapshot: snap, Selected: "api"}
	plain := Draw(g, opts).Plain()
	for _, want := range []string{"● API", "38 queued", "conn ████████ 100/100", "Database (postgres)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("drawing lacks %q", want)
		}
	}
	if strings.Count(plain, "•") < 3 {
		t.Errorf("waiting edge should pile dots at the arrowhead, found %d", strings.Count(plain, "•"))
	}
	if len(snap.Issues) == 0 {
		t.Fatal("scenario 02 must produce an issue")
	}
	opts.Lens = &snap.Issues[0]
	c := Draw(g, opts)
	lit, dim := 0, 0
	for _, cell := range c.Cells {
		if cell.Ch == ' ' {
			continue
		}
		if cell.St.Dim {
			dim++
		} else {
			lit++
		}
	}
	if dim == 0 || lit == 0 {
		t.Errorf("lens should dim part of the picture: lit %d dim %d", lit, dim)
	}
	svg := c.SVG()
	// Words are separate elements pinned to the grid, so check them apart.
	if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, ">queued<") || !strings.Contains(svg, ">38<") || !strings.Contains(svg, `textLength="48"`) {
		t.Error("svg export should carry the labels on the cell grid")
	}
}

// TestModelSmoke drives the Bubble Tea model through a resize, keys and a
// view without a terminal.
func TestModelSmoke(t *testing.T) {
	dir := "../../testdata/scenarios/04-node-memory-pressure"
	fx, err := fixture.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := app.New(app.Options{Dir: dir, Replay: fx, Speed: 1000})
	if err != nil {
		t.Fatal(err)
	}
	m := New(rt, Options{NoColor: true})
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	snap := scenarioSnapshot(t, dir)
	m.onUpdate(app.Update{Snapshot: snap})
	if !m.lensOn {
		t.Error("auto lens should turn on for a crit")
	}
	for _, k := range []string{"tab", "f", "e", "a", "?", "n", "right", "down", "enter", "t", "[", "esc", "i", "/"} {
		m.Update(tea.KeyPressMsg{Text: keyText(k), Code: keyCode(k)})
	}
	m.filtering = false
	v := m.View()
	if !strings.Contains(v.Content, "bookstore") || strings.Count(v.Content, "\n") != 44 {
		t.Errorf("view has %d newlines", strings.Count(v.Content, "\n"))
	}
	if !strings.Contains(m.selectedRef(), model.RefScheme) {
		t.Errorf("ref %q", m.selectedRef())
	}
	m.selected = "node-2"
	m.Update(tea.MouseClickMsg{X: 5, Y: 44})
	_ = time.Second
}

func scenarioSnapshot(t *testing.T, dir string) *model.Snapshot {
	run, err := scenario.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return run.Play()
}

func keyText(k string) string {
	if len(k) == 1 {
		return k
	}
	return ""
}

func keyCode(k string) rune {
	switch k {
	case "tab":
		return tea.KeyTab
	case "enter":
		return tea.KeyEnter
	case "esc":
		return tea.KeyEscape
	case "right":
		return tea.KeyRight
	case "down":
		return tea.KeyDown
	}
	if len(k) == 1 {
		return rune(k[0])
	}
	return 0
}

// machines is a system with the api on two machines, a worker on one, and
// two services of others.
func machines() *model.Topology {
	return &model.Topology{Name: "t", Components: []model.Component{
		{ID: "n1", Type: "node"}, {ID: "n2", Type: "node"},
		{ID: "api", Type: "workload", Label: "API", RunsOn: []string{"n1", "n2"}},
		{ID: "worker", Type: "backgroundworker", Label: "Workers", RunsOn: []string{"n1", "n2"}},
		{ID: "db", Type: "database", Label: "Database"},
		{ID: "mail", Type: "external", Label: "Mail"},
		{ID: "pay", Type: "external", Label: "Payments"},
	}, Edges: []model.Edge{
		{From: "api", To: "db", Kind: "sql"}, {From: "worker", To: "db", Kind: "sql"},
		{From: "api", To: "mail", Kind: "external"}, {From: "api", To: "pay", Kind: "external"},
		{From: "worker", To: "mail", Kind: "external"},
	}}
}

// A machine that holds none of a component's pods shows its name, faint,
// and no box; no wire ends there.
func TestAnEmptyPlaceIsNoBoxAndHasNoWire(t *testing.T) {
	top := machines()
	g := layout.Compute(top, model.Layout{}, layout.DefaultOptions())
	snap := &model.Snapshot{Edges: map[string]model.ElementState{}, Components: map[string]model.ElementState{
		"api": {State: model.Flowing, Label: "flowing, 40 req/s"},
		"n1":  {State: model.Flowing, Label: "flowing", Hosted: []model.Hosted{{ID: "api", Known: true, Pods: 2, Ready: 2, State: model.Flowing}, {ID: "worker", Known: true, Pods: 1, Ready: 1}}},
		"n2":  {State: model.Flowing, Label: "flowing", Hosted: []model.Hosted{{ID: "api", Known: true, Pods: 0}, {ID: "worker", Known: true, Pods: 1, Ready: 1}}},
	}}
	c := Draw(g, DrawOptions{Topology: top, Snapshot: snap})
	plain := c.Plain()
	if !strings.Contains(plain, "API, none here") || strings.Count(plain, "API ×2") != 1 {
		t.Errorf("the api runs on one machine and is named on the other:\n%s", plain)
	}
	empty := g.Boxes["api@n2"]
	if got := c.Get(empty.X, empty.Y).Ch; got != ' ' {
		t.Errorf("the empty place has a border: %q", got)
	}
	for id, r := range g.Routes {
		if r.From != "api@n2" {
			continue
		}
		// the first cell of a route from the empty place is its own
		if p := r.Cells[0]; c.Get(p.X, p.Y).Ch != ' ' {
			t.Errorf("%s is drawn from a place that holds nothing: %q at %v", id, c.Get(p.X, p.Y).Ch, p)
		}
	}
	// without a word of the cluster every copy is drawn
	plain = Draw(g, DrawOptions{Topology: top, Snapshot: &model.Snapshot{Components: map[string]model.ElementState{}, Edges: map[string]model.ElementState{}}}).Plain()
	if strings.Contains(plain, "none here") || strings.Count(plain, "API") != 2 {
		t.Errorf("nothing is known of the machines:\n%s", plain)
	}
}

// Where the routes of one net meet they are joined; where two nets meet
// they cross, and a crossing is drawn as one line over the other, never as
// a junction.
func TestNetsCrossAndTheirRoutesJoin(t *testing.T) {
	top := machines()
	g := layout.Compute(top, model.Layout{}, layout.DefaultOptions())
	c := Draw(g, DrawOptions{Topology: top, Snapshot: &model.Snapshot{Components: map[string]model.ElementState{}, Edges: map[string]model.ElementState{}}})
	nets := map[layout.Point]map[string]bool{}
	ends := map[layout.Point]bool{}
	for _, r := range g.Routes {
		for _, p := range r.Cells {
			if nets[p] == nil {
				nets[p] = map[string]bool{}
			}
			nets[p][r.Net] = true
		}
		ends[r.Cells[len(r.Cells)-1]] = true
	}
	crossings, joints := 0, 0
	for p, of := range nets {
		ch := c.Get(p.X, p.Y).Ch
		switch {
		case ends[p]:
		case len(of) > 1:
			crossings++
			if !strings.ContainsRune("─│┄┆╌╎", ch) {
				t.Errorf("two nets meet at %v and are drawn as %q", p, ch)
			}
		case strings.ContainsRune("┬┴├┤┼", ch):
			joints++
		}
	}
	if crossings == 0 || joints == 0 {
		t.Errorf("crossings %d, joints %d: the picture has both", crossings, joints)
	}
}

// TestMouseMovesAroundAndOpensDetail says what the mouse promises beyond
// selecting: the wheel pans the diagram both ways, and a double click on a
// box opens its detail panel as enter does.
func TestMouseMovesAroundAndOpensDetail(t *testing.T) {
	dir := "../../testdata/scenarios/04-node-memory-pressure"
	fx, err := fixture.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := app.New(app.Options{Dir: dir, Replay: fx, Speed: 1000})
	if err != nil {
		t.Fatal(err)
	}
	m := New(rt, Options{NoColor: true})
	// A window smaller than the picture, so there is somewhere to pan to.
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m.onUpdate(app.Update{Snapshot: scenarioSnapshot(t, dir)})
	m.panelOpen = false
	if m.graph.W <= 60 || m.graph.H <= 20 {
		t.Fatalf("the picture (%dx%d) fits the window, nothing to pan", m.graph.W, m.graph.H)
	}

	wheel := func(b tea.MouseButton, mod tea.KeyMod) {
		m.Update(tea.MouseWheelMsg{X: 10, Y: 5, Button: b, Mod: mod})
	}
	wheel(tea.MouseWheelRight, 0)
	if m.viewX <= 0 {
		t.Error("a wheel to the right should pan right")
	}
	wheel(tea.MouseWheelLeft, 0)
	if m.viewX != 0 {
		t.Errorf("a wheel to the left should pan back, viewX = %d", m.viewX)
	}
	wheel(tea.MouseWheelLeft, 0)
	if m.viewX != 0 {
		t.Errorf("the view should stop at the left edge, viewX = %d", m.viewX)
	}
	wheel(tea.MouseWheelDown, tea.ModShift)
	if m.viewX <= 0 || m.viewY != 0 {
		t.Errorf("shift and the wheel should pan sideways only, view = %d,%d", m.viewX, m.viewY)
	}
	wheel(tea.MouseWheelUp, tea.ModShift)
	wheel(tea.MouseWheelDown, 0)
	if m.viewX != 0 || m.viewY <= 0 {
		t.Errorf("the wheel should pan down, view = %d,%d", m.viewX, m.viewY)
	}
	wheel(tea.MouseWheelUp, 0)

	// Any box that is on screen will do.
	gx, gy, gw, gh := m.graphRect()
	x, y := -1, -1
	for _, b := range m.graph.Boxes {
		if b.X+1 < gw && b.Y+1 < gh && m.graph.BoxAt(b.X+1, b.Y+1) != nil && m.graph.GroupTitleAt(b.X+1, b.Y+1) == nil {
			x, y = gx+b.X+1, gy+b.Y+1
			break
		}
	}
	if x < 0 {
		t.Fatal("no box in the window")
	}
	click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
	m.selected = ""
	m.Update(click)
	m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if m.selected == "" {
		t.Fatal("a click should select the box")
	}
	if m.panelOpen {
		t.Error("one click should not open the panel")
	}
	m.Update(click)
	if !m.panelOpen || m.panelMode != panelDetail {
		t.Error("a double click should open the detail panel")
	}

	// Two slow clicks are two clicks.
	m.panelOpen = false
	m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(click)
	m.lastClickAt = m.lastClickAt.Add(-time.Second)
	m.Update(click)
	if m.panelOpen {
		t.Error("two clicks a second apart should not open the panel")
	}
}
