// Package model holds the file contract of wassup: the types behind every
// file in .wassup/, their validation, ids and refs.
package model

import (
	"fmt"
	"time"
)

// DirName is the directory at the repo root that holds every wassup file.
const DirName = ".wassup"

// Version is the only supported config file version.
const Version = 1

// Topology is topology.yaml: the logical diagram.
type Topology struct {
	Version    int         `yaml:"version" json:"version"`
	Name       string      `yaml:"name" json:"name"`
	Settings   Settings    `yaml:"settings,omitempty" json:"settings,omitempty"`
	Groups     []Group     `yaml:"groups,omitempty" json:"groups,omitempty"`
	Components []Component `yaml:"components" json:"components"`
	Edges      []Edge      `yaml:"edges,omitempty" json:"edges,omitempty"`
}

// Settings are the few knobs that are not thresholds.
type Settings struct {
	AutoLens bool   `yaml:"auto_lens,omitempty" json:"auto_lens,omitempty"`
	Lookback string `yaml:"lookback,omitempty" json:"lookback,omitempty"` // change marker lookback, default 2h
	Tick     string `yaml:"tick,omitempty" json:"tick,omitempty"`         // metrics tick, default 5s
}

// LookbackDuration returns the change-marker lookback window.
func (s Settings) LookbackDuration() time.Duration {
	if d, err := time.ParseDuration(s.Lookback); err == nil && d > 0 {
		return d
	}
	return 2 * time.Hour
}

// TickDuration returns the metrics tick.
func (s Settings) TickDuration() time.Duration {
	if d, err := time.ParseDuration(s.Tick); err == nil && d > 0 {
		return d
	}
	return 5 * time.Second
}

// Group is a container: cloud, region, cluster, namespace, zone.
type Group struct {
	ID     string `yaml:"id" json:"id"`
	Kind   string `yaml:"kind" json:"kind"`
	Label  string `yaml:"label,omitempty" json:"label,omitempty"`
	Parent string `yaml:"parent,omitempty" json:"parent,omitempty"`
}

// Roles is the db-only primary/replicas declaration.
type Roles struct {
	Primary  string   `yaml:"primary,omitempty" json:"primary,omitempty"`
	Replicas []string `yaml:"replicas,omitempty" json:"replicas,omitempty"`
}

// Component is one box on the diagram.
type Component struct {
	ID     string   `yaml:"id" json:"id"`
	Type   string   `yaml:"type" json:"type"`
	Label  string   `yaml:"label,omitempty" json:"label,omitempty"`
	Group  string   `yaml:"group,omitempty" json:"group,omitempty"`
	Lane   string   `yaml:"lane,omitempty" json:"lane,omitempty"`
	Icon   string   `yaml:"icon,omitempty" json:"icon,omitempty"`
	Engine string   `yaml:"engine,omitempty" json:"engine,omitempty"`
	RunsOn []string `yaml:"runs_on,omitempty" json:"runs_on,omitempty"`
	Roles  *Roles   `yaml:"roles,omitempty" json:"roles,omitempty"`
	Notes  string   `yaml:"notes,omitempty" json:"notes,omitempty"`
	Owner  string   `yaml:"owner,omitempty" json:"owner,omitempty"`
	// Parent is set for db role instances that were expanded from roles.
	Parent string `yaml:"-" json:"parent,omitempty"`
}

// DisplayLabel is the label or the id when no label is set.
func (c Component) DisplayLabel() string {
	if c.Label != "" {
		return c.Label
	}
	return c.ID
}

// Edge is a directed connection between two components.
type Edge struct {
	From  string `yaml:"from" json:"from"`
	To    string `yaml:"to" json:"to"`
	Kind  string `yaml:"kind" json:"kind"`
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
}

// ID is the stable edge id "from->to".
func (e Edge) ID() string { return EdgeID(e.From, e.To) }

// EdgeID builds the edge id used in bindings, snapshots and refs.
func EdgeID(from, to string) string { return from + "->" + to }

// ProbeSpec is one probe attached to a component or edge. The "probe" key
// names the probe kind; every other key is the probe's own configuration.
type ProbeSpec map[string]any

// Kind returns the probe kind or "".
func (p ProbeSpec) Kind() string {
	if k, ok := p["probe"].(string); ok {
		return k
	}
	return ""
}

// String returns a string field or "".
func (p ProbeSpec) String(key string) string {
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

// Bindings is bindings.yaml.
type Bindings struct {
	Version    int                    `yaml:"version" json:"version"`
	Components map[string][]ProbeSpec `yaml:"components,omitempty" json:"components,omitempty"`
	Edges      map[string][]ProbeSpec `yaml:"edges,omitempty" json:"edges,omitempty"`
}

// Finding is one repo or infra issue written by Claude Code.
type Finding struct {
	ID           string    `yaml:"id" json:"id"`
	Severity     string    `yaml:"severity" json:"severity"` // critical, high, medium, low
	Title        string    `yaml:"title" json:"title"`
	Component    string    `yaml:"component,omitempty" json:"component,omitempty"`
	File         string    `yaml:"file,omitempty" json:"file,omitempty"`
	Line         int       `yaml:"line,omitempty" json:"line,omitempty"`
	Evidence     string    `yaml:"evidence,omitempty" json:"evidence,omitempty"`
	SuggestedFix string    `yaml:"suggested_fix,omitempty" json:"suggested_fix,omitempty"`
	FoundAt      time.Time `yaml:"found_at,omitempty" json:"found_at,omitempty"`
}

// Findings is findings.yaml.
type Findings struct {
	Version  int       `yaml:"version" json:"version"`
	Findings []Finding `yaml:"findings" json:"findings"`
}

// ThresholdSet holds every threshold the state engine consults. Zero means
// "use the default".
type ThresholdSet struct {
	GaugeAmberPct   float64 `yaml:"gauge_amber_pct,omitempty" json:"gauge_amber_pct,omitempty"`
	GaugeRedPct     float64 `yaml:"gauge_red_pct,omitempty" json:"gauge_red_pct,omitempty"`
	DiskFailingPct  float64 `yaml:"disk_failing_pct,omitempty" json:"disk_failing_pct,omitempty"`
	ErrorRatePct    float64 `yaml:"error_rate_pct,omitempty" json:"error_rate_pct,omitempty"`
	TimeoutRatePct  float64 `yaml:"timeout_rate_pct,omitempty" json:"timeout_rate_pct,omitempty"`
	QueueDepth      float64 `yaml:"queue_depth,omitempty" json:"queue_depth,omitempty"`
	LagBytes        float64 `yaml:"lag_bytes,omitempty" json:"lag_bytes,omitempty"`
	CertWarnDays    float64 `yaml:"cert_warn_days,omitempty" json:"cert_warn_days,omitempty"`
	HitRateMinPct   float64 `yaml:"hit_rate_min_pct,omitempty" json:"hit_rate_min_pct,omitempty"`
	StaleTicks      int     `yaml:"stale_ticks,omitempty" json:"stale_ticks,omitempty"`
	StuckTaskFactor float64 `yaml:"stuck_task_factor,omitempty" json:"stuck_task_factor,omitempty"`
	RateSpikeFactor float64 `yaml:"rate_spike_factor,omitempty" json:"rate_spike_factor,omitempty"`
	TrendWindowMin  float64 `yaml:"trend_window_min,omitempty" json:"trend_window_min,omitempty"`
}

// DefaultThresholds are the shipped defaults.
var DefaultThresholds = ThresholdSet{
	GaugeAmberPct:   80,
	GaugeRedPct:     90,
	DiskFailingPct:  95,
	ErrorRatePct:    5,
	TimeoutRatePct:  1,
	QueueDepth:      10,
	LagBytes:        64 << 20,
	CertWarnDays:    14,
	HitRateMinPct:   50,
	StaleTicks:      3,
	StuckTaskFactor: 10,
	RateSpikeFactor: 1.8,
	TrendWindowMin:  30,
}

// Merge returns t with every zero field filled from base.
func (t ThresholdSet) Merge(base ThresholdSet) ThresholdSet {
	pick := func(a, b float64) float64 {
		if a != 0 {
			return a
		}
		return b
	}
	out := ThresholdSet{
		GaugeAmberPct:   pick(t.GaugeAmberPct, base.GaugeAmberPct),
		GaugeRedPct:     pick(t.GaugeRedPct, base.GaugeRedPct),
		DiskFailingPct:  pick(t.DiskFailingPct, base.DiskFailingPct),
		ErrorRatePct:    pick(t.ErrorRatePct, base.ErrorRatePct),
		TimeoutRatePct:  pick(t.TimeoutRatePct, base.TimeoutRatePct),
		QueueDepth:      pick(t.QueueDepth, base.QueueDepth),
		LagBytes:        pick(t.LagBytes, base.LagBytes),
		CertWarnDays:    pick(t.CertWarnDays, base.CertWarnDays),
		HitRateMinPct:   pick(t.HitRateMinPct, base.HitRateMinPct),
		StuckTaskFactor: pick(t.StuckTaskFactor, base.StuckTaskFactor),
		RateSpikeFactor: pick(t.RateSpikeFactor, base.RateSpikeFactor),
		TrendWindowMin:  pick(t.TrendWindowMin, base.TrendWindowMin),
		StaleTicks:      t.StaleTicks,
	}
	if out.StaleTicks == 0 {
		out.StaleTicks = base.StaleTicks
	}
	return out
}

// Thresholds is thresholds.yaml.
type Thresholds struct {
	Version    int                     `yaml:"version" json:"version"`
	Defaults   ThresholdSet            `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Components map[string]ThresholdSet `yaml:"components,omitempty" json:"components,omitempty"`
}

// For returns the effective thresholds for a component or edge id.
func (t Thresholds) For(id string) ThresholdSet {
	base := t.Defaults.Merge(DefaultThresholds)
	if t.Components == nil {
		return base
	}
	if c, ok := t.Components[id]; ok {
		return c.Merge(base)
	}
	return base
}

// Placement is a saved box position in layout.json.
type Placement struct {
	X    int  `json:"x"`
	Y    int  `json:"y"`
	W    int  `json:"w,omitempty"`
	H    int  `json:"h,omitempty"`
	Auto bool `json:"auto,omitempty"`
}

// Layout is layout.json: positions the user owns.
type Layout struct {
	Version    int                  `json:"version"`
	Components map[string]Placement `json:"components,omitempty"`
	Collapsed  []string             `json:"collapsed,omitempty"`
	Waypoints  map[string][][2]int  `json:"waypoints,omitempty"`
	GraphSplit int                  `json:"graph_split,omitempty"` // percent of width for the graph
	// Detail is the box detail level: minimal, normal (default) or full.
	Detail string `json:"detail,omitempty"`
}

// Detail levels for the diagram.
const (
	DetailMinimal = "minimal"
	DetailNormal  = "normal"
	DetailFull    = "full"
)

// DetailLevel returns the effective level.
func (l Layout) DetailLevel() string {
	switch l.Detail {
	case DetailMinimal, DetailFull:
		return l.Detail
	}
	return DetailNormal
}

// Annotation is one highlighted path with a note.
type Annotation struct {
	ID         string    `json:"id"`
	Path       []string  `json:"path"`
	Note       string    `json:"note"`
	Confidence string    `json:"confidence,omitempty"` // low, medium, high
	Source     string    `json:"source,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
}

// Annotations is state/annotations.json.
type Annotations struct {
	Version     int          `json:"version"`
	Annotations []Annotation `json:"annotations"`
}

// Live returns the annotations that have not expired at now.
func (a Annotations) Live(now time.Time) []Annotation {
	var out []Annotation
	for _, an := range a.Annotations {
		if an.ExpiresAt.IsZero() || an.ExpiresAt.After(now) {
			out = append(out, an)
		}
	}
	return out
}

// Condition is a normalized fact about an element ("CrashLoopBackOff").
type Condition struct {
	Kind   string    `json:"kind"`
	Ref    string    `json:"ref,omitempty"`
	Since  time.Time `json:"since,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Event is a change marker: deploy, terraform apply, reboot, scale, cert renewal, switchover.
type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // deploy, terraform, node, cert, scale, switchover, eviction, job
	Target  string    `json:"target,omitempty"`
	Summary string    `json:"summary"`
	Author  string    `json:"author,omitempty"`
	Ref     string    `json:"ref,omitempty"` // commit, resource address
}

// Letter is the single letter drawn on the timeline for this event kind.
func (e Event) Letter() string {
	switch e.Kind {
	case "deploy":
		return "D"
	case "terraform":
		return "T"
	case "node":
		return "N"
	case "cert":
		return "C"
	case "scale":
		return "S"
	case "switchover":
		return "W"
	case "eviction":
		return "E"
	case "job":
		return "J"
	}
	return "·"
}

// ElementState is the snapshot entry of a component or an edge.
type ElementState struct {
	State      State              `json:"state"`
	Marker     Marker             `json:"marker,omitempty"`
	Label      string             `json:"label"`
	Severity   Severity           `json:"severity"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Conditions []Condition        `json:"conditions,omitempty"`
	Since      time.Time          `json:"since,omitempty"`
	LastData   time.Time          `json:"last_data,omitempty"`
	Gauges     []Gauge            `json:"gauges,omitempty"`
	Notes      []string           `json:"notes,omitempty"`
	Detail     map[string]any     `json:"detail,omitempty"`
	// Edge-only fields.
	Rate      float64 `json:"rate,omitempty"`
	Queued    float64 `json:"queued,omitempty"`
	ErrorRate float64 `json:"error_rate,omitempty"`
	Unit      string  `json:"unit,omitempty"`
}

// Gauge is one horizontal bar inside a box.
type Gauge struct {
	Name   string  `json:"name"`
	Short  string  `json:"short,omitempty"`
	Pct    float64 `json:"pct"`             // 0..100, -1 when unknown
	Value  string  `json:"value"`           // "84%" or "92/100"
	Level  string  `json:"level"`           // ok, amber, red, none
	Trend  string  `json:"trend,omitempty"` // "", "up", "down"
	Detail string  `json:"detail,omitempty"`
}

// Snapshot is state/snapshot.json.
type Snapshot struct {
	GeneratedAt time.Time               `json:"generated_at"`
	Tick        int64                   `json:"tick"`
	Name        string                  `json:"name,omitempty"`
	Replay      bool                    `json:"replay,omitempty"`
	Components  map[string]ElementState `json:"components"`
	Edges       map[string]ElementState `json:"edges"`
	ProbeHealth map[string]string       `json:"probe_health,omitempty"`
	Issues      []Issue                 `json:"issues,omitempty"`
}

// Issue is one lit path computed by the cause rule.
type Issue struct {
	ID       string   `json:"id"`       // the affected element id
	Cause    string   `json:"cause"`    // the element named as the most probable cause
	Severity Severity `json:"severity"` // worst severity on the path
	Path     []string `json:"path"`     // element ids (components and edges) top to bottom
	Story    []string `json:"story"`    // one sentence per hop, cause last
}

// Plain returns the spec as nested plain maps and slices, so probes can type
// switch on map[string]any regardless of how YAML decoded nested mappings.
func (p ProbeSpec) Plain() map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = plainValue(v)
	}
	return out
}

func plainValue(v any) any {
	switch t := v.(type) {
	case ProbeSpec:
		return t.Plain()
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = plainValue(val)
		}
		return m
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[fmt.Sprint(k)] = plainValue(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = plainValue(val)
		}
		return s
	}
	return v
}
