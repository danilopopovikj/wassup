// Package app is the runtime shared by the TUI and the CLI: it loads the
// .wassup directory, starts probes, joins observations, evaluates the state
// engine every tick, writes the snapshot and history, and hot-reloads
// configuration through fsnotify.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/danilopopovikj/wassup/internal/bind"
	"github.com/danilopopovikj/wassup/internal/history"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/fixture"
	"github.com/danilopopovikj/wassup/internal/state"
)

// Options configure a runtime.
type Options struct {
	Dir string // the .wassup directory
	// Replay plays a recorded fixture instead of live probes.
	Replay *fixture.Fixture
	// Speed multiplies the replay clock (1 = real time).
	Speed float64
	// ReadOnly disables every write under state/ (replay defaults to it).
	ReadOnly bool
	// Kubeconfig and Context are defaults for k8s.* and cnpg.* probes.
	Kubeconfig, Context string
	// Logf receives diagnostics; nil discards them.
	Logf func(format string, args ...any)
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Update is what the runtime pushes to the TUI after every tick or reload.
type Update struct {
	Snapshot    *model.Snapshot
	Config      *model.Config // non-nil when configuration was reloaded
	Annotations *model.Annotations
	Toast       string
	LayoutOnly  bool // the reload only touched layout.json
}

type instance struct {
	kind   string
	target string
	p      probe.Probe
	err    error
}

// Runtime is one running wassup.
type Runtime struct {
	opts Options

	mu       sync.RWMutex
	cfg      *model.Config
	snap     *model.Snapshot
	tick     int64
	binder   *bind.Binder
	ring     *history.Ring
	store    *history.Store
	inst     []instance
	annots   model.Annotations
	clock    func() time.Time
	replayCk *fixture.Clock

	obs     chan probe.Observation
	updates chan Update
	reload  chan string

	cancelProbes context.CancelFunc
	probeCtx     context.Context
}

// New loads the directory and prepares a runtime. A validation error is
// returned as *model.ValidationError with the partially loaded config kept.
func New(opts Options) (*Runtime, error) {
	if opts.Speed <= 0 {
		opts.Speed = 1
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	cfg, err := model.Load(opts.Dir)
	if err != nil {
		return nil, err
	}
	r := &Runtime{
		opts:    opts,
		cfg:     cfg,
		binder:  bind.New(),
		ring:    history.NewRing(24 * time.Hour),
		store:   history.NewStore(opts.Dir),
		obs:     make(chan probe.Observation, 1024),
		updates: make(chan Update, 16),
		reload:  make(chan string, 16),
	}
	r.clock = time.Now
	if opts.Now != nil {
		r.clock = opts.Now
	}
	if opts.Replay != nil {
		r.replayCk = &fixture.Clock{Start: opts.Replay.Scenario.Start, WallStart: time.Now(), Speed: opts.Speed}
		r.clock = r.replayCk.Now
		r.opts.ReadOnly = true
		r.opts.ReadOnly = opts.ReadOnly || true
	}
	if !r.opts.ReadOnly {
		if frames, err := r.store.LoadFrames(r.clock().Add(-24 * time.Hour)); err == nil {
			r.ring.AddAll(frames)
		}
		if evs, err := r.store.LoadEvents(); err == nil {
			r.binder.Seed(evs)
		}
		if _, err := r.store.Rollup(r.clock()); err != nil {
			opts.Logf("history rollup: %v", err)
		}
	}
	if a, err := model.LoadAnnotations(opts.Dir); err == nil {
		r.annots = a
	}
	return r, nil
}

// Config returns the current configuration.
func (r *Runtime) Config() *model.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// Snapshot returns the latest snapshot (nil before the first tick).
func (r *Runtime) Snapshot() *model.Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snap
}

// Ring returns the in-memory history.
func (r *Runtime) Ring() *history.Ring { return r.ring }

// Store returns the on-disk history store.
func (r *Runtime) Store() *history.Store { return r.store }

// Binder returns the binder (for explain).
func (r *Runtime) Binder() *bind.Binder { return r.binder }

// Events returns every change marker known.
func (r *Runtime) Events() []model.Event { return r.binder.Events() }

// Annotations returns the live annotations.
func (r *Runtime) Annotations() model.Annotations {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.annots
}

// Now returns the runtime clock (scenario time while replaying).
func (r *Runtime) Now() time.Time { return r.clock() }

// Replaying reports whether the runtime plays a fixture.
func (r *Runtime) Replaying() bool { return r.opts.Replay != nil }

// Updates delivers snapshots and reloads to the TUI.
func (r *Runtime) Updates() <-chan Update { return r.updates }

// ProbeHealth aggregates the health of every probe by kind (worst wins).
func (r *Runtime) ProbeHealth() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.probeHealthLocked()
}

func (r *Runtime) probeHealthLocked() map[string]string {
	rank := map[probe.HealthState]int{probe.HealthOK: 0, probe.HealthDegraded: 1, probe.HealthFailed: 2}
	out := map[string]string{}
	worst := map[string]int{}
	for _, in := range r.inst {
		h := probe.ProbeHealth{State: probe.HealthFailed, Message: "not started"}
		if in.err != nil {
			h.Message = in.err.Error()
		} else if in.p != nil {
			h = in.p.Health()
		}
		if cur, ok := worst[in.kind]; !ok || rank[h.State] > cur {
			worst[in.kind] = rank[h.State]
			out[in.kind] = string(h.State)
			if h.Message != "" && h.State != probe.HealthOK {
				out[in.kind] = string(h.State) + ": " + h.Message
			}
		}
	}
	return out
}

// Instances describes every probe binding and its health (for `probe --once`).
type Instance struct {
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Health  string `json:"health"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Instances lists probe instances.
func (r *Runtime) Instances() []Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Instance
	for _, in := range r.inst {
		i := Instance{Kind: in.kind, Target: in.target}
		if in.err != nil {
			i.Health = string(probe.HealthFailed)
			i.Error = in.err.Error()
		} else if in.p != nil {
			h := in.p.Health()
			i.Health, i.Message = string(h.State), h.Message
		}
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Target != out[b].Target {
			return out[a].Target < out[b].Target
		}
		return out[a].Kind < out[b].Kind
	})
	return out
}

// Start launches probes, the tick loop and the file watcher. It returns once
// everything is running; the loop stops when ctx is done.
func (r *Runtime) Start(ctx context.Context) error {
	r.startProbes(ctx)
	go r.watch(ctx)
	go r.loop(ctx)
	return nil
}

func (r *Runtime) startProbes(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelProbes != nil {
		r.cancelProbes()
	}
	pctx, cancel := context.WithCancel(ctx)
	r.probeCtx, r.cancelProbes = pctx, cancel
	r.inst = nil
	tick := r.cfg.Topology.Settings.TickDuration()
	if r.opts.Replay != nil {
		p, _ := probe.New("fixture")
		spec := map[string]any{"dir": r.opts.Replay.Dir, "speed": r.opts.Speed, "_target": "*", "_tick": tick}
		err := p.Start(pctx, spec, r.obs)
		r.inst = append(r.inst, instance{kind: "fixture", target: "*", p: p, err: err})
		return
	}
	start := func(target string, spec model.ProbeSpec) {
		kind := spec.Kind()
		in := instance{kind: kind, target: target}
		p, err := probe.New(kind)
		if err != nil {
			in.err = err
			r.inst = append(r.inst, in)
			return
		}
		s := spec.Plain()
		s["_target"] = target
		s["_tick"] = tick
		if strings.HasPrefix(kind, "k8s.") || strings.HasPrefix(kind, "cnpg.") {
			if _, ok := s["kubeconfig"]; !ok && r.opts.Kubeconfig != "" {
				s["kubeconfig"] = r.opts.Kubeconfig
			}
			if _, ok := s["context"]; !ok && r.opts.Context != "" {
				s["context"] = r.opts.Context
			}
		}
		if err := p.Validate(s); err != nil {
			in.err = fmt.Errorf("%s on %s: %w", kind, target, err)
			in.p = p
			r.inst = append(r.inst, in)
			return
		}
		in.p = p
		in.err = p.Start(pctx, s, r.obs)
		r.inst = append(r.inst, in)
	}
	ids := make([]string, 0, len(r.cfg.Bindings.Components))
	for id := range r.cfg.Bindings.Components {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, spec := range r.cfg.Bindings.Components[id] {
			start(id, spec)
		}
	}
	ids = ids[:0]
	for id := range r.cfg.Bindings.Edges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, spec := range r.cfg.Bindings.Edges[id] {
			start(id, spec)
		}
	}
}

// loop consumes observations and evaluates every tick.
func (r *Runtime) loop(ctx context.Context) {
	tick := r.cfg.Topology.Settings.TickDuration()
	if r.opts.Replay != nil {
		tick = time.Duration(r.opts.Replay.Scenario.TickS) * time.Second
	}
	wall := time.Duration(float64(tick) / r.opts.Speed)
	if wall < 50*time.Millisecond {
		wall = 50 * time.Millisecond
	}
	ticker := time.NewTicker(wall)
	defer ticker.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	pendingReload := map[string]bool{}
	// First evaluation right away so the screen is not empty.
	r.evaluate(false)
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-r.obs:
			r.binder.Apply(o)
			// drain what else is queued
			for n := 0; n < 1000; n++ {
				select {
				case o2 := <-r.obs:
					r.binder.Apply(o2)
					continue
				default:
				}
				break
			}
		case <-ticker.C:
			r.evaluate(true)
		case f := <-r.reload:
			pendingReload[f] = true
			debounce.Reset(300 * time.Millisecond)
		case <-debounce.C:
			files := pendingReload
			pendingReload = map[string]bool{}
			r.doReload(ctx, files)
		}
	}
}

// evaluate runs the engine once and publishes the result.
func (r *Runtime) evaluate(persist bool) {
	r.mu.Lock()
	cfg := r.cfg
	now := r.clock()
	r.tick++
	tickN := r.tick
	prev := r.snap
	tickEvery := cfg.Topology.Settings.TickDuration()
	if r.opts.Replay != nil {
		tickEvery = time.Duration(r.opts.Replay.Scenario.TickS) * time.Second
	}
	health := r.probeHealthLocked()
	r.mu.Unlock()

	th := cfg.Thresholds.For("")
	in := state.Input{
		Topology:    &cfg.Topology,
		Thresholds:  cfg.Thresholds,
		Joined:      r.binder.All(),
		Now:         now,
		Tick:        tickN,
		TickEvery:   tickEvery,
		Prev:        prev,
		Events:      history.Recent(r.binder.Events(), now, cfg.Topology.Settings.LookbackDuration()),
		Trends:      r.ring.Trends(now, time.Duration(th.TrendWindowMin)*time.Minute),
		Baselines:   r.ring.Baselines(now),
		ProbeHealth: health,
		Replay:      r.opts.Replay != nil,
	}
	snap := state.Evaluate(in)
	frame := history.FromSnapshot(snap)
	r.ring.Add(frame)
	newEvents := r.binder.DrainEvents()
	if persist && !r.opts.ReadOnly {
		if err := r.store.WriteSnapshot(snap); err != nil {
			r.opts.Logf("snapshot: %v", err)
		}
		if err := r.store.AppendFrame(frame); err != nil {
			r.opts.Logf("history: %v", err)
		}
		if err := r.store.AppendEvents(newEvents); err != nil {
			r.opts.Logf("events: %v", err)
		}
	}
	r.mu.Lock()
	r.snap = snap
	r.mu.Unlock()
	r.push(Update{Snapshot: snap})
}

func (r *Runtime) push(u Update) {
	select {
	case r.updates <- u:
	default:
		// The TUI is behind; drop the oldest and retry once.
		select {
		case <-r.updates:
		default:
		}
		select {
		case r.updates <- u:
		default:
		}
	}
}

// watch reloads configuration when files under .wassup/ change.
func (r *Runtime) watch(ctx context.Context) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		r.opts.Logf("fsnotify: %v", err)
		return
	}
	defer w.Close()
	_ = w.Add(r.opts.Dir)
	stateDir := filepath.Join(r.opts.Dir, "state")
	_ = os.MkdirAll(stateDir, 0o755)
	_ = w.Add(stateDir)
	interesting := map[string]bool{
		"topology.yaml": true, "bindings.yaml": true, "thresholds.yaml": true, "findings.yaml": true,
		"layout.json": true, "annotations.json": true,
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			base := filepath.Base(ev.Name)
			if !interesting[base] {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			select {
			case r.reload <- base:
			default:
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			r.opts.Logf("fsnotify: %v", err)
		}
	}
}

// Reload re-reads the configuration now (the TUI calls it after writing).
func (r *Runtime) Reload() {
	select {
	case r.reload <- "topology.yaml":
	default:
	}
}

// doReload applies a debounced set of changed files.
func (r *Runtime) doReload(ctx context.Context, files map[string]bool) {
	if files["annotations.json"] && len(files) == 1 {
		a, err := model.LoadAnnotations(r.opts.Dir)
		if err == nil {
			r.mu.Lock()
			r.annots = a
			r.mu.Unlock()
			r.push(Update{Snapshot: r.Snapshot(), Annotations: &a})
		}
		return
	}
	old := r.Config()
	cfg, err := model.Load(r.opts.Dir)
	if err != nil {
		msg := err.Error()
		if ve, ok := err.(*model.ValidationError); ok && len(ve.Issues) > 0 {
			msg = ve.Issues[0].String()
			if len(ve.Issues) > 1 {
				msg += fmt.Sprintf(" (+%d more)", len(ve.Issues)-1)
			}
		}
		r.push(Update{Snapshot: r.Snapshot(), Toast: "config not applied: " + msg})
		return
	}
	layoutOnly := len(files) == 1 && files["layout.json"]
	moved, added, removed := diffTopology(old, cfg)
	bindingsChanged := !sameBindings(old.Bindings, cfg.Bindings)
	r.mu.Lock()
	r.cfg = cfg
	if a, err := model.LoadAnnotations(r.opts.Dir); err == nil {
		r.annots = a
	}
	r.mu.Unlock()
	if bindingsChanged && r.opts.Replay == nil {
		r.startProbes(ctx)
	}
	toast := ""
	if !layoutOnly {
		toast = fmt.Sprintf("topology updated, %d moved, %d added", moved, added)
		if removed > 0 {
			toast += fmt.Sprintf(", %d removed", removed)
		}
		if files["findings.yaml"] && len(files) == 1 {
			toast = fmt.Sprintf("findings updated, %d open", len(cfg.Findings.Findings))
		}
		if files["thresholds.yaml"] && len(files) == 1 {
			toast = "thresholds updated"
		}
	}
	r.evaluate(false)
	r.push(Update{Snapshot: r.Snapshot(), Config: cfg, Toast: toast, LayoutOnly: layoutOnly})
}

func diffTopology(old, cur *model.Config) (moved, added, removed int) {
	if old == nil {
		return 0, len(cur.Topology.Components), 0
	}
	oldIDs := map[string]model.Component{}
	for _, c := range old.Topology.AllComponents() {
		oldIDs[c.ID] = c
	}
	curIDs := map[string]bool{}
	for _, c := range cur.Topology.AllComponents() {
		curIDs[c.ID] = true
		o, ok := oldIDs[c.ID]
		if !ok {
			added++
			continue
		}
		if o.Group != c.Group || o.Lane != c.Lane || strings.Join(o.RunsOn, ",") != strings.Join(c.RunsOn, ",") || o.Type != c.Type {
			moved++
		}
	}
	for id := range oldIDs {
		if !curIDs[id] {
			removed++
		}
	}
	return
}

func sameBindings(a, b model.Bindings) bool {
	return fmt.Sprint(a.Components) == fmt.Sprint(b.Components) && fmt.Sprint(a.Edges) == fmt.Sprint(b.Edges)
}

// RunOnce starts the probes, waits until every bound element has reported or
// the timeout passes, evaluates once and returns the snapshot. It never
// writes state.
func (r *Runtime) RunOnce(ctx context.Context, timeout time.Duration) (*model.Snapshot, error) {
	r.opts.ReadOnly = true
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r.startProbes(cctx)
	want := map[string]bool{}
	for id := range r.cfg.Bindings.Components {
		want[id] = true
	}
	for id := range r.cfg.Bindings.Edges {
		want[id] = true
	}
	got := map[string]bool{}
	deadline := time.After(timeout)
	settle := time.NewTimer(time.Hour)
	settle.Stop()
loop:
	for {
		select {
		case o := <-r.obs:
			r.binder.Apply(o)
			if o.Err == "" {
				got[o.Target] = true
			}
			if len(got) >= len(want) {
				settle.Reset(300 * time.Millisecond)
			}
		case <-settle.C:
			break loop
		case <-deadline:
			break loop
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// Give probes that failed to start a moment to report health.
	r.evaluate(false)
	return r.Snapshot(), nil
}

// SaveLayout writes layout.json for the current config.
func (r *Runtime) SaveLayout(l model.Layout) error {
	r.mu.Lock()
	r.cfg.Layout = l
	cfg := r.cfg
	r.mu.Unlock()
	return cfg.SaveLayout()
}

// SetAnnotations writes annotations and publishes them.
func (r *Runtime) SetAnnotations(a model.Annotations) error {
	r.mu.Lock()
	r.annots = a
	r.mu.Unlock()
	if r.opts.ReadOnly && r.opts.Replay != nil {
		r.push(Update{Snapshot: r.Snapshot(), Annotations: &a})
		return nil
	}
	return model.SaveAnnotations(r.opts.Dir, a)
}
