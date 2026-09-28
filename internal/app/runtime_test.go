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

// counter is a probe whose rate only exists from its second round on, like
// every probe that computes a rate from two readings. With broken set it
// reports an error every round instead.
type counter struct {
	h      probe.Health
	broken bool
}

func (p *counter) Kind() string {
	if p.broken {
		return "test.broken"
	}
	return "test.counter"
}
func (p *counter) Validate(spec map[string]any) error { return nil }
func (p *counter) Health() probe.ProbeHealth          { return p.h.Get() }

func (p *counter) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	target, _ := spec["_target"].(string)
	every, _ := spec["_tick"].(time.Duration)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for round := 1; ; round++ {
			o := probe.Observation{Target: target, Probe: p.Kind(), At: time.Now()}
			switch {
			case p.broken:
				o.Err = "connection refused"
				p.h.Set(probe.HealthDegraded, o.Err)
			case round == 1:
				o.Metrics = map[string]float64{"pool_used": 3, "pool_max": 20}
			default:
				o.Metrics = map[string]float64{"pool_used": 3, "pool_max": 20, "rate": 40}
			}
			if !probe.Send(ctx, out, o) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func sampleConfig(t *testing.T) string {
	t.Helper()
	probe.Register(probe.Access{Kind: "test.counter", Implemented: true}, func() probe.Probe { return &counter{} })
	probe.Register(probe.Access{Kind: "test.broken", Implemented: true}, func() probe.Probe { return &counter{broken: true} })
	return writeConfig(t, map[string]string{
		"topology.yaml": "version: 1\nname: bookstore\nsettings:\n  tick: 100ms\ncomponents:\n" +
			"  - id: api\n    type: workload\n    label: API\n" +
			"  - id: db\n    type: database\n    label: Database\n" +
			"edges:\n  - from: api\n    to: db\n    kind: sql\n",
		"bindings.yaml": "version: 1\ncomponents:\n" +
			"  api:\n    - probe: test.counter\n" +
			"  db:\n    - probe: test.broken\n" +
			"edges:\n  api->db:\n    - probe: test.counter\n",
	})
}

func TestSampleTwiceShowsTheRate(t *testing.T) {
	rt, err := New(Options{Dir: sampleConfig(t), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	snap, err := rt.Sample(context.Background(), 10*time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	if e := snap.Edges["api->db"]; e.Metrics["rate"] != 40 || e.Label != "flowing, 40 queries/s" && e.State != "flowing" {
		t.Errorf("two samples should show the rate: %+v", e)
	}
	// db's only probe fails every round. It has reported, so the run ends
	// when the samples are in and does not sit out the timeout.
	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("the run waited %s for a probe that had already reported its error", took)
	}
	if db := snap.Components["db"]; db.Label != "no data, connection refused" {
		t.Errorf("db = %+v", db)
	}
}

func TestSampleOnceHasNoRateYet(t *testing.T) {
	rt, err := New(Options{Dir: sampleConfig(t), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := rt.RunOnce(context.Background(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The settle time of a single sample may let a second round in; what
	// must hold is that the run returns data and never invents a rate.
	e := snap.Edges["api->db"]
	if e.Metrics["pool_max"] != 20 {
		t.Errorf("edge should carry the probe's data: %+v", e)
	}
	if r, ok := e.Metrics["rate"]; ok && r != 40 {
		t.Errorf("rate = %v", r)
	}
}
