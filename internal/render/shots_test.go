package render

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/danilopopovikj/wassup/internal/app"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/fixture"
	"github.com/danilopopovikj/wassup/internal/scenario"
)

// TestShots writes SVG frames of the TUI for several scenarios when
// WASSUP_SHOTS names a directory. Used to produce screenshots headlessly.
func TestShots(t *testing.T) {
	out := os.Getenv("WASSUP_SHOTS")
	if out == "" {
		t.Skip("set WASSUP_SHOTS=<dir> to write frames")
	}
	type shot struct {
		name, dir string
		setup     func(m *Model)
	}
	shots := []shot{
		{"01-crash-loop-lens", "01-deploy-crash-loop", func(m *Model) { m.lensOn = true; m.selected = "api"; m.follow() }},
		{"02-pool-exhausted-panel", "02-connection-pool-exhausted", func(m *Model) {
			m.lensOn = true
			m.selected = "api->db"
			m.panelOpen = true
			m.panelMode = panelDetail
			m.follow()
		}},
		{"04-node-memory-lens", "04-node-memory-pressure", func(m *Model) { m.lensOn = true; m.selected = "node-2"; m.follow() }},
		{"07-node-dropped-timeline", "07-node-dropped-from-lb", func(m *Model) { m.lensOn = false; m.selected = "node-3"; m.follow() }},
		{"08-firewall-nodata", "08-firewall-blocked-observability", func(m *Model) { m.lensOn = true; m.selected = "signoz"; m.follow() }},
		{"09-cache-full-detail", "09-cache-full-miss-storm", func(m *Model) {
			m.cfg.Layout.Detail = model.DetailFull
			m.relayout()
			m.lensOn = false
			m.selected = "cache"
			m.panelOpen = true
			m.panelMode = panelDetail
			m.follow()
		}},
		{"03-replica-behind", "03-primary-disk-filling", func(m *Model) { m.lensOn = true; m.selected = "db"; m.follow() }},
		{"05-certificate-expired", "05-tls-certificate-expired", func(m *Model) { m.lensOn = true; m.selected = "ingress"; m.follow() }},
		{"10-external-down", "10-external-dependency-down", func(m *Model) { m.lensOn = true; m.selected = "docs-sync"; m.follow() }},
		{"11-hatchet-backlog", "11-hatchet-backlog", func(m *Model) { m.lensOn = true; m.selected = "hatchet-workers"; m.follow() }},
		{"12-electric-slot", "12-electric-slot-inactive", func(m *Model) { m.lensOn = true; m.selected = "electric"; m.follow() }},
		{"06-celery-findings", "06-celery-backlog-stuck-worker", func(m *Model) {
			m.lensOn = true
			m.selected = "worker-3"
			m.panelOpen = true
			m.panelMode = panelHelp
			m.follow()
		}},
	}
	for _, sh := range shots {
		dir := filepath.Join("..", "..", "testdata", "scenarios", sh.dir)
		fx, err := fixture.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		rt, err := app.New(app.Options{Dir: dir, Replay: fx, Speed: 1})
		if err != nil {
			t.Fatal(err)
		}
		run, _ := scenario.Load(dir)
		snap := run.Play()
		// The frames of the run feed the timeline strip.
		rt.Ring().AddAll(run.Frames)
		rt.Binder().Seed(run.Binder.Events())
		m := New(rt, Options{})
		m.Update(tea.WindowSizeMsg{Width: 190, Height: 100})
		m.snap, m.view = snap, snap
		m.frame = 7
		sh.setup(m)
		c := m.Screen()
		if err := os.WriteFile(filepath.Join(out, sh.name+".svg"), []byte(c.SVG()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
