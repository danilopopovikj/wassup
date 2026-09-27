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
	if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "38 queued") {
		t.Error("svg export should carry the labels")
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
