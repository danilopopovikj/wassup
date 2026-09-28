// Package bind joins live observations to components and edges. It keeps the
// latest observation per (target, probe) and merges them into one view per
// element.
package bind

import (
	"sort"
	"sync"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// Joined is everything known about one element right now.
type Joined struct {
	Target     string
	Bound      bool // at least one probe delivered data
	LastAt     time.Time
	Metrics    map[string]float64
	Conditions []model.Condition
	Detail     map[string]any
	Probes     []string
	Errors     []string
	// Sources is what each probe delivered on its own, in the order of
	// Probes, so a report can say which probe failed and which number came
	// from where.
	Sources []Source
}

// Source is the latest observation of one probe about one element.
type Source struct {
	Probe   string
	At      time.Time
	Err     string
	Metrics map[string]float64
}

// Binder is safe for concurrent use.
type Binder struct {
	mu      sync.RWMutex
	latest  map[string]map[string]probe.Observation // target -> probe -> obs
	events  []model.Event
	seenEv  map[string]bool
	pending []model.Event
}

// New returns an empty binder.
func New() *Binder {
	return &Binder{latest: map[string]map[string]probe.Observation{}, seenEv: map[string]bool{}}
}

// Apply records an observation. Newly seen events are queued for the
// history writer and returned by DrainEvents.
func (b *Binder) Apply(o probe.Observation) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if o.At.IsZero() {
		o.At = time.Now()
	}
	m := b.latest[o.Target]
	if m == nil {
		m = map[string]probe.Observation{}
		b.latest[o.Target] = m
	}
	key := o.Probe
	if key == "" {
		key = "-"
	}
	m[key] = o
	for _, e := range o.Events {
		k := e.At.UTC().Format(time.RFC3339) + "|" + e.Kind + "|" + e.Target + "|" + e.Summary
		if b.seenEv[k] {
			continue
		}
		b.seenEv[k] = true
		if e.Target == "" {
			e.Target = o.Target
		}
		b.events = append(b.events, e)
		b.pending = append(b.pending, e)
	}
}

// Seed preloads events (from state/events.jsonl) so they are not re-appended.
func (b *Binder) Seed(events []model.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range events {
		k := e.At.UTC().Format(time.RFC3339) + "|" + e.Kind + "|" + e.Target + "|" + e.Summary
		if b.seenEv[k] {
			continue
		}
		b.seenEv[k] = true
		b.events = append(b.events, e)
	}
	sort.SliceStable(b.events, func(i, j int) bool { return b.events[i].At.Before(b.events[j].At) })
}

// DrainEvents returns events seen since the last drain.
func (b *Binder) DrainEvents() []model.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.pending
	b.pending = nil
	return out
}

// Events returns every event seen, oldest first.
func (b *Binder) Events() []model.Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := append([]model.Event(nil), b.events...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// Get returns the merged view of one element, or nil when nothing has been
// observed for it.
func (b *Binder) Get(target string) *Joined {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.join(target)
}

func (b *Binder) join(target string) *Joined {
	m := b.latest[target]
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	j := &Joined{Target: target, Metrics: map[string]float64{}, Detail: map[string]any{}}
	// Older observations first so a newer one wins per metric key.
	sort.SliceStable(keys, func(x, y int) bool { return m[keys[x]].At.Before(m[keys[y]].At) })
	seenCond := map[string]bool{}
	for _, k := range keys {
		o := m[k]
		j.Probes = append(j.Probes, k)
		j.Sources = append(j.Sources, Source{Probe: k, At: o.At, Err: o.Err, Metrics: o.Metrics})
		if o.Err != "" {
			j.Errors = append(j.Errors, k+": "+o.Err)
			continue
		}
		j.Bound = true
		if o.At.After(j.LastAt) {
			j.LastAt = o.At
		}
		for mk, mv := range o.Metrics {
			j.Metrics[mk] = mv
		}
		for _, c := range o.Conditions {
			ck := c.Kind + "|" + c.Ref
			if seenCond[ck] {
				continue
			}
			seenCond[ck] = true
			j.Conditions = append(j.Conditions, c)
		}
		for dk, dv := range o.Detail {
			j.Detail[dk] = dv
		}
	}
	sort.SliceStable(j.Conditions, func(x, y int) bool { return j.Conditions[x].Kind < j.Conditions[y].Kind })
	return j
}

// All returns the merged view of every observed element.
func (b *Binder) All() map[string]*Joined {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]*Joined, len(b.latest))
	for t := range b.latest {
		if j := b.join(t); j != nil {
			out[t] = j
		}
	}
	return out
}

// Targets returns every observed element id.
func (b *Binder) Targets() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.latest))
	for t := range b.latest {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Reset forgets every observation (used when topology changes ids).
func (b *Binder) Reset() {
	b.mu.Lock()
	b.latest = map[string]map[string]probe.Observation{}
	b.mu.Unlock()
}
