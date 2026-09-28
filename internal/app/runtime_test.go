package app

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// holder is a probe that holds a connection: it reports once and needs a
// moment to release what it holds after it was told to stop.
type holder struct {
	h        probe.Health
	done     chan struct{}
	released *atomic.Int32
}

func (p *holder) Kind() string                       { return "test.holder" }
func (p *holder) Validate(spec map[string]any) error { return nil }
func (p *holder) Health() probe.ProbeHealth          { return p.h.Get() }
func (p *holder) Done() <-chan struct{}              { return p.done }

func (p *holder) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	p.done = make(chan struct{})
	target, _ := spec["_target"].(string)
	go func() {
		defer close(p.done)
		p.h.Set(probe.HealthOK, "")
		probe.Send(ctx, out, probe.Observation{Target: target, Probe: p.Kind(), At: time.Now(), Metrics: map[string]float64{"connections_used": 3}})
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		p.released.Add(1)
	}()
	return nil
}

func writeConfig(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".wassup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunOnceWaitsUntilProbesReleased(t *testing.T) {
	var released atomic.Int32
	probe.Register(probe.Access{Kind: "test.holder", Implemented: true}, func() probe.Probe {
		return &holder{released: &released}
	})
	dir := writeConfig(t, map[string]string{
		"topology.yaml": "version: 1\nname: bookstore\ncomponents:\n" +
			"  - id: db\n    type: database\n    label: Database\n" +
			"  - id: replica\n    type: database\n    label: Replica\n",
		"bindings.yaml": "version: 1\ncomponents:\n" +
			"  db:\n    - probe: test.holder\n" +
			"  replica:\n    - probe: test.holder\n",
	})
	rt, err := New(Options{Dir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := rt.RunOnce(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := released.Load(); got != 2 {
		t.Errorf("RunOnce returned before the probes released their connections: %d of 2 released", got)
	}
	if es := snap.Components["db"]; es.Metrics["connections_used"] != 3 {
		t.Errorf("db should carry the probe's data, got %+v", es)
	}
}
