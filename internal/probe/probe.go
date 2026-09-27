// Package probe defines the one interface every data source implements and
// the registry the TUI, CLI and tests go through.
package probe

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
)

// Observation is what a probe reports about one component or edge.
type Observation struct {
	Target     string             `json:"target"`
	Probe      string             `json:"probe,omitempty"`
	At         time.Time          `json:"at"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Conditions []model.Condition  `json:"conditions,omitempty"`
	Events     []model.Event      `json:"events,omitempty"`
	Detail     map[string]any     `json:"detail,omitempty"`
	// Err is set when the probe could not read its source this round; the
	// binder then treats the observation as absent and the probe as degraded.
	Err string `json:"err,omitempty"`
}

// HealthState is a probe's own health.
type HealthState string

// Health states.
const (
	HealthOK       HealthState = "ok"
	HealthDegraded HealthState = "degraded"
	HealthFailed   HealthState = "failed"
)

// ProbeHealth is a probe's health plus a message.
type ProbeHealth struct {
	State   HealthState `json:"state"`
	Message string      `json:"message,omitempty"`
	At      time.Time   `json:"at,omitempty"`
}

// Probe is one read-only data source. Start must return promptly and push
// observations to out until ctx is done.
type Probe interface {
	Kind() string
	Validate(spec map[string]any) error
	Start(ctx context.Context, spec map[string]any, out chan<- Observation) error
	Health() ProbeHealth
}

// Factory builds a fresh probe instance for one binding.
type Factory func() Probe

// Access documents what a probe needs to read its source.
type Access struct {
	Kind        string   `json:"kind"`
	Source      string   `json:"source"`
	Delivers    string   `json:"delivers"`
	SpecFields  []string `json:"spec_fields"`
	Needs       string   `json:"needs"`
	Implemented bool     `json:"implemented"`
}

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
	access    = map[string]Access{}
)

// Register adds a probe kind to the registry.
func Register(a Access, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	factories[a.Kind] = f
	access[a.Kind] = a
}

// New builds a probe for the given kind.
func New(kind string) (Probe, error) {
	mu.RLock()
	f, ok := factories[kind]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown probe %q", kind)
	}
	return f(), nil
}

// Known reports whether a kind is registered.
func Known(kind string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := factories[kind]
	return ok
}

// Kinds lists registered kinds, sorted.
func Kinds() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AccessFor returns the documentation of a kind.
func AccessFor(kind string) (Access, bool) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := access[kind]
	return a, ok
}

// AllAccess returns every registered probe's documentation, sorted by kind.
func AllAccess() []Access {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Access, 0, len(access))
	for _, a := range access {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Health is a small thread-safe health holder probes can embed.
type Health struct {
	mu sync.Mutex
	h  ProbeHealth
}

// Set records the health.
func (h *Health) Set(s HealthState, msg string) {
	h.mu.Lock()
	h.h = ProbeHealth{State: s, Message: msg, At: time.Now()}
	h.mu.Unlock()
}

// Get returns the health; a never-set health reads as degraded.
func (h *Health) Get() ProbeHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.h.State == "" {
		return ProbeHealth{State: HealthDegraded, Message: "no data yet"}
	}
	return h.h
}

// Send pushes an observation unless ctx is done.
func Send(ctx context.Context, out chan<- Observation, o Observation) bool {
	select {
	case out <- o:
		return true
	case <-ctx.Done():
		return false
	}
}

// RequireString validates that a spec has a non-empty string field.
func RequireString(spec map[string]any, keys ...string) error {
	for _, k := range keys {
		v, ok := spec[k].(string)
		if !ok || v == "" {
			return fmt.Errorf("%q is required", k)
		}
	}
	return nil
}

// Str reads a string from a spec with a default.
func Str(spec map[string]any, key, def string) string {
	if v, ok := spec[key].(string); ok && v != "" {
		return v
	}
	return def
}

// Dur reads a duration from a spec with a default.
func Dur(spec map[string]any, key string, def time.Duration) time.Duration {
	if v, ok := spec[key].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Num reads a number from a spec.
func Num(spec map[string]any, key string) (float64, bool) {
	switch v := spec[key].(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	}
	return 0, false
}

// Stub registers a documented probe kind that is not yet implemented. It
// validates its spec but reports failed health, so bound components draw as
// unbound rather than healthy.
func Stub(a Access, required ...string) {
	a.Implemented = false
	Register(a, func() Probe { return &stub{kind: a.Kind, required: required} })
}

type stub struct {
	kind     string
	required []string
	h        Health
}

func (s *stub) Kind() string { return s.kind }
func (s *stub) Validate(spec map[string]any) error {
	return RequireString(spec, s.required...)
}
func (s *stub) Start(ctx context.Context, spec map[string]any, out chan<- Observation) error {
	s.h.Set(HealthFailed, "probe "+s.kind+" is not implemented in this build")
	return nil
}
func (s *stub) Health() ProbeHealth { return s.h.Get() }
