// Package scenario runs a recorded fixture through the binder, the history
// ring and the state engine, tick by tick, exactly as the TUI would. Both the
// tests and `wassup replay` use it.
package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/danilopopovikj/wassup/internal/bind"
	"github.com/danilopopovikj/wassup/internal/history"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/fixture"
	"github.com/danilopopovikj/wassup/internal/state"
)

// Expected is expected.yaml: what the final snapshot must contain.
type Expected struct {
	Components    map[string]ExpectedElem `yaml:"components"`
	Edges         map[string]ExpectedElem `yaml:"edges"`
	Cause         string                  `yaml:"cause"`
	StoryContains []string                `yaml:"story_contains"`
	StoryFirst    string                  `yaml:"story_first,omitempty"`
	NotesContain  map[string][]string     `yaml:"notes_contain,omitempty"`
	LensOn        *bool                   `yaml:"lens_on,omitempty"`
}

// ExpectedElem is the expected state and label of one element.
type ExpectedElem struct {
	State    string `yaml:"state"`
	Label    string `yaml:"label,omitempty"`
	Contains string `yaml:"contains,omitempty"`
	Severity string `yaml:"severity,omitempty"`
	Marker   string `yaml:"marker,omitempty"`
}

// Run is one scenario execution.
type Run struct {
	Dir      string
	Fixture  *fixture.Fixture
	Config   *model.Config
	Expected *Expected
	Binder   *bind.Binder
	Ring     *history.Ring
	Snapshot *model.Snapshot
	Frames   []history.Frame
	// FirstLens is the first tick at which an issue with severity crit appeared.
	FirstCritTick int64
}

// Load reads a scenario directory: topology.yaml, optional bindings.yaml and
// thresholds.yaml, scenario.yaml, observations.jsonl, optional expected.yaml.
func Load(dir string) (*Run, error) {
	fx, err := fixture.Load(dir)
	if err != nil {
		return nil, err
	}
	cfg, err := model.Load(dir)
	if err != nil {
		if _, ok := err.(*model.ValidationError); ok {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		return nil, err
	}
	r := &Run{Dir: dir, Fixture: fx, Config: cfg, Binder: bind.New(), Ring: history.NewRing(24 * time.Hour)}
	if b, err := os.ReadFile(filepath.Join(dir, "expected.yaml")); err == nil {
		var ex Expected
		if err := yaml.Unmarshal(b, &ex); err != nil {
			return nil, fmt.Errorf("expected.yaml: %w", err)
		}
		r.Expected = &ex
	}
	return r, nil
}

// Step evaluates one tick at the given scenario time, having applied every
// observation up to it.
func (r *Run) Step(now time.Time, tick int64, prev *model.Snapshot) *model.Snapshot {
	th := r.Config.Thresholds
	lookback := r.Config.Topology.Settings.LookbackDuration()
	in := state.Input{
		Topology:   &r.Config.Topology,
		Thresholds: th,
		Joined:     r.Binder.All(),
		Now:        now,
		Tick:       tick,
		TickEvery:  time.Duration(r.Fixture.Scenario.TickS) * time.Second,
		Prev:       prev,
		Events:     history.Recent(r.Binder.Events(), now, lookback),
		Trends:     r.Ring.Trends(now, time.Duration(th.For("").TrendWindowMin)*time.Minute),
		Baselines:  r.Ring.Baselines(now),
		Replay:     true,
	}
	snap := state.Evaluate(in)
	f := history.FromSnapshot(snap)
	r.Ring.Add(f)
	r.Frames = append(r.Frames, f)
	return snap
}

// Play runs the whole scenario offline and returns the final snapshot.
func (r *Run) Play() *model.Snapshot {
	obs := r.Fixture.Expand()
	tick := time.Duration(r.Fixture.Scenario.TickS) * time.Second
	start := r.Fixture.Scenario.Start
	end := start.Add(time.Duration(r.Fixture.Scenario.DurationS) * time.Second)
	var prev *model.Snapshot
	var n int64
	i := 0
	for now := start; !now.After(end); now = now.Add(tick) {
		for i < len(obs) && !obs[i].At.After(now) {
			r.Binder.Apply(obs[i])
			i++
		}
		n++
		prev = r.Step(now, n, prev)
		if r.FirstCritTick == 0 {
			for _, is := range prev.Issues {
				if is.Severity == model.Crit {
					r.FirstCritTick = n
					break
				}
			}
		}
	}
	r.Snapshot = prev
	return prev
}

// Observations returns the expanded observations (for the replay probe).
func (r *Run) Observations() []probe.Observation { return r.Fixture.Expand() }

// Check compares the final snapshot with expected.yaml and returns mismatches.
func (r *Run) Check() []string {
	if r.Expected == nil {
		return []string{"no expected.yaml"}
	}
	if r.Snapshot == nil {
		r.Play()
	}
	var out []string
	snap := r.Snapshot
	check := func(kind, id string, ex ExpectedElem, es model.ElementState, ok bool) {
		if !ok {
			out = append(out, fmt.Sprintf("%s %s: missing from snapshot", kind, id))
			return
		}
		if ex.State != "" && string(es.State) != ex.State {
			out = append(out, fmt.Sprintf("%s %s: state %s, want %s (label %q)", kind, id, es.State, ex.State, es.Label))
		}
		if ex.Label != "" && es.Label != ex.Label {
			out = append(out, fmt.Sprintf("%s %s: label %q, want %q", kind, id, es.Label, ex.Label))
		}
		if ex.Contains != "" && !strings.Contains(es.Label, ex.Contains) {
			out = append(out, fmt.Sprintf("%s %s: label %q does not contain %q", kind, id, es.Label, ex.Contains))
		}
		if ex.Severity != "" && es.Severity.String() != ex.Severity {
			out = append(out, fmt.Sprintf("%s %s: severity %s, want %s", kind, id, es.Severity, ex.Severity))
		}
		if ex.Marker != "" && string(es.Marker) != ex.Marker {
			out = append(out, fmt.Sprintf("%s %s: marker %q, want %q", kind, id, es.Marker, ex.Marker))
		}
	}
	ids := sortedKeys(r.Expected.Components)
	for _, id := range ids {
		es, ok := snap.Components[id]
		check("component", id, r.Expected.Components[id], es, ok)
	}
	ids = sortedKeys(r.Expected.Edges)
	for _, id := range ids {
		es, ok := snap.Edges[id]
		check("edge", id, r.Expected.Edges[id], es, ok)
	}
	var story []string
	cause := ""
	if len(snap.Issues) > 0 {
		story = snap.Issues[0].Story
		cause = snap.Issues[0].Cause
	}
	if r.Expected.Cause != "" && cause != r.Expected.Cause {
		out = append(out, fmt.Sprintf("cause %q, want %q", cause, r.Expected.Cause))
	}
	joined := strings.Join(story, "\n")
	for _, want := range r.Expected.StoryContains {
		if !strings.Contains(joined, want) {
			out = append(out, fmt.Sprintf("story lacks %q", want))
		}
	}
	if r.Expected.StoryFirst != "" && (len(story) == 0 || !strings.Contains(story[0], r.Expected.StoryFirst)) {
		first := ""
		if len(story) > 0 {
			first = story[0]
		}
		out = append(out, fmt.Sprintf("first story line %q does not contain %q", first, r.Expected.StoryFirst))
	}
	for id, wants := range r.Expected.NotesContain {
		es := snap.Components[id]
		all := strings.Join(es.Notes, "\n")
		for _, g := range es.Gauges {
			all += "\n" + g.Name + " " + g.Value + " " + g.Detail
		}
		for _, w := range wants {
			if !strings.Contains(all, w) {
				out = append(out, fmt.Sprintf("component %s: notes/gauges %q lack %q", id, strings.ReplaceAll(all, "\n", " | "), w))
			}
		}
	}
	if r.Expected.LensOn != nil {
		on := len(snap.Issues) > 0 && snap.Issues[0].Severity == model.Crit
		if on != *r.Expected.LensOn {
			out = append(out, fmt.Sprintf("lens on = %v, want %v", on, *r.Expected.LensOn))
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Dirs lists scenario directories under root, sorted.
func Dirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(root, e.Name(), "scenario.yaml")); err == nil {
				out = append(out, filepath.Join(root, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
