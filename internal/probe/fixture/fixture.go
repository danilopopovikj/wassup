// Package fixture is the probe behind `wassup replay`: it plays recorded
// observations from a directory on the scenario's own clock.
package fixture

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// Scenario is scenario.yaml inside a fixture directory.
type Scenario struct {
	Name      string    `yaml:"name" json:"name"`
	Symptom   string    `yaml:"symptom,omitempty" json:"symptom,omitempty"`
	Start     time.Time `yaml:"start" json:"start"`
	DurationS int       `yaml:"duration_s" json:"duration_s"`
	// TickS is the replay tick; defaults to 5.
	TickS int `yaml:"tick_s,omitempty" json:"tick_s,omitempty"`
}

// Record is one line of observations.jsonl. Times are offsets in seconds
// from the scenario start so fixtures stay readable.
type Record struct {
	At         float64            `json:"at"`
	Target     string             `json:"target"`
	Probe      string             `json:"probe,omitempty"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Conditions []RecCondition     `json:"conditions,omitempty"`
	Events     []RecEvent         `json:"events,omitempty"`
	Detail     map[string]any     `json:"detail,omitempty"`
	// Every is an optional repeat interval in seconds: the record is re-sent
	// every N seconds until Until (or the end of the scenario).
	Every float64 `json:"every,omitempty"`
	Until float64 `json:"until,omitempty"`
	// Ramp, when set, linearly interpolates Metrics toward RampTo over the
	// repeat window.
	RampTo map[string]float64 `json:"ramp_to,omitempty"`
}

// RecCondition is a condition with a relative since.
type RecCondition struct {
	Kind   string  `json:"kind"`
	Ref    string  `json:"ref,omitempty"`
	SinceS float64 `json:"since_s"`
	Detail string  `json:"detail,omitempty"`
}

// RecEvent is an event with a relative time.
type RecEvent struct {
	AtS     float64 `json:"at_s"`
	Kind    string  `json:"kind"`
	Target  string  `json:"target,omitempty"`
	Summary string  `json:"summary"`
	Author  string  `json:"author,omitempty"`
	Ref     string  `json:"ref,omitempty"`
}

// Fixture is a loaded scenario directory.
type Fixture struct {
	Dir      string
	Scenario Scenario
	Records  []Record
}

// Load reads scenario.yaml and observations.jsonl from dir.
func Load(dir string) (*Fixture, error) {
	f := &Fixture{Dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, "scenario.yaml"))
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, &f.Scenario); err != nil {
		return nil, fmt.Errorf("scenario.yaml: %w", err)
	}
	if f.Scenario.TickS == 0 {
		f.Scenario.TickS = 5
	}
	if f.Scenario.DurationS == 0 {
		f.Scenario.DurationS = 300
	}
	fh, err := os.Open(filepath.Join(dir, "observations.jsonl"))
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	line := 0
	for sc.Scan() {
		line++
		txt := strings.TrimSpace(sc.Text())
		if txt == "" || strings.HasPrefix(txt, "#") || strings.HasPrefix(txt, "//") {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(txt), &r); err != nil {
			return nil, fmt.Errorf("observations.jsonl:%d: %w", line, err)
		}
		if r.Target == "" {
			return nil, fmt.Errorf("observations.jsonl:%d: target is required", line)
		}
		f.Records = append(f.Records, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return f, nil
}

// Expand turns the compact records into a time-sorted list of observations
// with absolute times. A record holds from its "at" until the next record
// for the same target and probe (or "until", or the end of the scenario),
// re-emitted every tick so the engine sees fresh data. Ramps interpolate
// metrics over the hold window. Events are emitted once, on the first tick.
func (f *Fixture) Expand() []probe.Observation {
	start := f.Scenario.Start
	end := float64(f.Scenario.DurationS)
	tick := float64(f.Scenario.TickS)
	// Find the next record per (target, probe) to know how long each holds.
	type key struct{ t, p string }
	byKey := map[key][]int{}
	for i, r := range f.Records {
		k := key{r.Target, r.Probe}
		byKey[k] = append(byKey[k], i)
	}
	holdUntil := make([]float64, len(f.Records))
	for _, idxs := range byKey {
		sort.SliceStable(idxs, func(a, b int) bool { return f.Records[idxs[a]].At < f.Records[idxs[b]].At })
		for n, i := range idxs {
			u := end
			if n+1 < len(idxs) {
				u = f.Records[idxs[n+1]].At
			}
			if f.Records[i].Until > 0 && f.Records[i].Until < u {
				u = f.Records[i].Until
			}
			holdUntil[i] = u
		}
	}
	var out []probe.Observation
	for i, r := range f.Records {
		every := r.Every
		if every <= 0 {
			every = tick
		}
		var times []float64
		for t := r.At; t < holdUntil[i] || (t == r.At); t += every {
			times = append(times, t)
			if every <= 0 {
				break
			}
		}
		span := holdUntil[i] - r.At
		for n, t := range times {
			o := probe.Observation{
				Target: r.Target,
				Probe:  r.Probe,
				At:     start.Add(time.Duration(t * float64(time.Second))),
				Detail: r.Detail,
			}
			if r.Metrics != nil {
				o.Metrics = map[string]float64{}
				frac := 0.0
				if span > 0 {
					frac = (t - r.At) / span
				}
				for k, v := range r.Metrics {
					if to, ok := r.RampTo[k]; ok {
						o.Metrics[k] = v + (to-v)*frac
					} else {
						o.Metrics[k] = v
					}
				}
			}
			for _, c := range r.Conditions {
				o.Conditions = append(o.Conditions, model.Condition{
					Kind: c.Kind, Ref: c.Ref, Detail: c.Detail,
					Since: start.Add(time.Duration(c.SinceS * float64(time.Second))),
				})
			}
			if n == 0 {
				for _, e := range r.Events {
					tgt := e.Target
					if tgt == "" {
						tgt = r.Target
					}
					o.Events = append(o.Events, model.Event{
						At: start.Add(time.Duration(e.AtS * float64(time.Second))), Kind: e.Kind,
						Target: tgt, Summary: e.Summary, Author: e.Author, Ref: e.Ref,
					})
				}
			}
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// Clock maps scenario time to wall time for a replay.
type Clock struct {
	Start     time.Time // scenario start
	WallStart time.Time
	Speed     float64
}

// Now returns the scenario time for the current wall time.
func (c Clock) Now() time.Time {
	el := time.Since(c.WallStart)
	return c.Start.Add(time.Duration(float64(el) * c.Speed))
}

// Access documents the fixture probe.
var Access = probe.Access{
	Kind: "fixture", Source: "a recorded fixture directory", Delivers: "whatever was recorded",
	SpecFields: []string{"dir", "speed"}, Needs: "read access to the directory", Implemented: true,
}

func init() {
	probe.Register(Access, func() probe.Probe { return &Probe{} })
}

// Probe replays a fixture. Spec: dir (required), speed (float, default 1).
type Probe struct {
	h     probe.Health
	clock Clock
}

// Kind implements probe.Probe.
func (p *Probe) Kind() string { return "fixture" }

// Validate implements probe.Probe.
func (p *Probe) Validate(spec map[string]any) error { return probe.RequireString(spec, "dir") }

// Health implements probe.Probe.
func (p *Probe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *Probe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	fx, err := Load(probe.Str(spec, "dir", ""))
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	speed := 1.0
	if v, ok := probe.Num(spec, "speed"); ok && v > 0 {
		speed = v
	}
	p.clock = Clock{Start: fx.Scenario.Start, WallStart: time.Now(), Speed: speed}
	obs := fx.Expand()
	p.h.Set(probe.HealthOK, fmt.Sprintf("replaying %s", fx.Scenario.Name))
	go func() {
		for _, o := range obs {
			delay := time.Duration(float64(o.At.Sub(p.clock.Start)) / speed)
			wait := time.Until(p.clock.WallStart.Add(delay))
			if wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
			}
			if !probe.Send(ctx, out, o) {
				return
			}
		}
		p.h.Set(probe.HealthOK, "fixture finished, holding last values")
	}()
	return nil
}
