// Package explain assembles everything known about an element into under
// 200 lines: binding, current and recent states, metrics, conditions, last
// events, attached findings, related edges, logs for workloads and top
// waiting queries for databases.
package explain

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/history"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/state"
)

// MaxLines is the hard cap so output pastes into an agent context untrimmed.
const MaxLines = 200

// LogSource fetches container logs for a workload component.
type LogSource func(ctx context.Context, comp model.Component, spec model.ProbeSpec, since time.Duration, tail int) ([]string, error)

// EventSource fetches Kubernetes events for a component.
type EventSource func(ctx context.Context, comp model.Component, spec model.ProbeSpec, since time.Duration) ([]string, error)

// Source is what explain reads from.
type Source struct {
	Cfg      *model.Config
	Snapshot *model.Snapshot // current, may be nil
	Frames   []history.Frame // recent history, oldest first
	Events   []model.Event
	Now      time.Time
	Logs     LogSource   // optional
	K8sEvent EventSource // optional
	// At, when set, explains the history frame at that time instead of now.
	At time.Time
}

// Result is the structured form of an explanation.
type Result struct {
	Ref        string             `json:"ref"`
	ID         string             `json:"id"`
	Kind       string             `json:"kind"`
	Label      string             `json:"label"`
	At         time.Time          `json:"at"`
	Historical bool               `json:"historical,omitempty"`
	State      model.ElementState `json:"state"`
	Bindings   []model.ProbeSpec  `json:"bindings,omitempty"`
	Recent     []string           `json:"recent_states,omitempty"`
	Events     []model.Event      `json:"events,omitempty"`
	Findings   []model.Finding    `json:"findings,omitempty"`
	Edges      map[string]string  `json:"edges,omitempty"`
	Issue      *model.Issue       `json:"issue,omitempty"`
	Logs       []string           `json:"logs,omitempty"`
	K8sEvents  []string           `json:"k8s_events,omitempty"`
	Lines      []string           `json:"-"`
}

// Explain builds the result for a ref.
func Explain(ctx context.Context, src Source, ref model.Ref) (*Result, error) {
	t := &src.Cfg.Topology
	ref, err := t.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if src.Now.IsZero() {
		src.Now = time.Now()
	}
	at := src.Now
	if !ref.At.IsZero() {
		at = ref.At
	} else if !src.At.IsZero() {
		at = src.At
	}
	res := &Result{Ref: ref.String(), ID: ref.ID, Kind: ref.Kind, Label: t.LabelOf(ref.ID), At: at, Edges: map[string]string{}}

	snap := src.Snapshot
	if !ref.At.IsZero() || !src.At.IsZero() {
		if f, ok := frameAt(src.Frames, at); ok {
			snap = history.ToSnapshot(f, t.Name)
			res.Historical = true
		}
	}
	var es model.ElementState
	var found bool
	if snap != nil {
		if ref.IsEdge() {
			es, found = snap.Edges[ref.ID]
		} else {
			es, found = snap.Components[ref.ID]
		}
		for _, is := range snap.Issues {
			for _, pid := range is.Path {
				if pid == ref.ID {
					isCopy := is
					res.Issue = &isCopy
					break
				}
			}
			if res.Issue != nil {
				break
			}
		}
	}
	res.State = es
	if ref.IsEdge() {
		res.Bindings = src.Cfg.Bindings.Edges[ref.ID]
	} else {
		res.Bindings = src.Cfg.Bindings.Components[ref.ID]
	}
	res.Recent = recentStates(src.Frames, ref, at)
	for _, e := range src.Events {
		if e.Target == ref.ID && !e.At.After(at) && e.At.After(at.Add(-24*time.Hour)) {
			res.Events = append(res.Events, e)
		}
	}
	sort.SliceStable(res.Events, func(i, j int) bool { return res.Events[i].At.After(res.Events[j].At) })
	if len(res.Events) > 8 {
		res.Events = res.Events[:8]
	}
	for _, f := range src.Cfg.Findings.Findings {
		if f.Component == ref.ID {
			res.Findings = append(res.Findings, f)
		}
	}
	if snap != nil && !ref.IsEdge() {
		for _, e := range t.Edges {
			if e.From == ref.ID || e.To == ref.ID {
				res.Edges[e.ID()] = snap.Edges[e.ID()].Label
			}
		}
	}
	comp, isComp := t.Component(ref.ID)
	if isComp && !res.Historical {
		if comp.Type == "workload" && src.Logs != nil {
			for _, spec := range res.Bindings {
				if spec.Kind() == "k8s.workload" {
					lines, err := src.Logs(ctx, comp, spec, 15*time.Minute, 50)
					if err != nil {
						res.Logs = []string{"logs unavailable: " + err.Error()}
					} else {
						res.Logs = lines
					}
					break
				}
			}
		}
		if src.K8sEvent != nil && strings.HasPrefix(firstKind(res.Bindings), "k8s.") {
			if lines, err := src.K8sEvent(ctx, comp, res.Bindings[0], 2*time.Hour); err == nil {
				res.K8sEvents = lines
			}
		}
	}
	res.Lines = render(src, res, ref, es, found, comp, isComp)
	return res, nil
}

func firstKind(specs []model.ProbeSpec) string {
	if len(specs) == 0 {
		return ""
	}
	return specs[0].Kind()
}

func frameAt(frames []history.Frame, at time.Time) (history.Frame, bool) {
	var best history.Frame
	ok := false
	for _, f := range frames {
		if !f.At.After(at) {
			best, ok = f, true
		}
	}
	return best, ok
}

// recentStates lists state transitions over the last 30 minutes.
func recentStates(frames []history.Frame, ref model.Ref, at time.Time) []string {
	var out []string
	var last string
	cut := at.Add(-30 * time.Minute)
	for _, f := range frames {
		if f.At.Before(cut) || f.At.After(at) {
			continue
		}
		var fe history.FrameElem
		var ok bool
		if ref.IsEdge() {
			fe, ok = f.E[ref.ID]
		} else {
			fe, ok = f.C[ref.ID]
		}
		if !ok {
			continue
		}
		key := fe.S + "|" + fe.L
		if key == last {
			continue
		}
		last = key
		out = append(out, fmt.Sprintf("%s %s: %s", f.At.Format("15:04:05"), fe.S, fe.L))
	}
	if len(out) > 12 {
		out = append(out[:1], out[len(out)-11:]...)
	}
	return out
}

func render(src Source, res *Result, ref model.Ref, es model.ElementState, found bool, comp model.Component, isComp bool) []string {
	t := &src.Cfg.Topology
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	add("%s  %s", res.Ref, res.Label)
	if ref.IsEdge() {
		e, _ := t.Edge(ref.ID)
		add("%s edge from %s to %s", e.Kind, t.LabelOf(e.From), t.LabelOf(e.To))
	} else if isComp {
		desc := comp.Type
		if comp.Group != "" {
			desc += " in " + comp.Group
		}
		if len(comp.RunsOn) > 0 {
			desc += ", runs on " + strings.Join(comp.RunsOn, ", ")
		}
		if comp.Owner != "" {
			desc += ", owner " + comp.Owner
		}
		add("%s", desc)
		if comp.Notes != "" && comp.Parent == "" {
			add("notes: %s", comp.Notes)
		}
	}
	when := "now"
	if res.Historical {
		when = "at " + res.At.Format(time.RFC3339) + " (history frame, as the user was scrubbing)"
	}
	add("")
	if !found {
		add("state (%s): unknown, no snapshot entry for this element", when)
	} else {
		m := ""
		if es.Marker != "" {
			m = " [" + string(es.Marker) + "]"
		}
		add("state (%s): %s%s", when, es.State, m)
		add("label: %s", es.Label)
		since := ""
		if !es.Since.IsZero() {
			since = fmt.Sprintf(", since %s (%s)", es.Since.Format("15:04:05"), state.Ago(res.At, es.Since))
		}
		add("severity: %s%s", es.Severity, since)
		if !es.LastData.IsZero() {
			add("last data: %s ago", state.Ago(res.At, es.LastData))
		}
		for _, n := range es.Notes {
			add("note: %s", n)
		}
	}
	if len(res.Bindings) == 0 {
		add("")
		add("binding: none. This element is unbound; add a probe in bindings.yaml.")
	} else {
		add("")
		for _, b := range res.Bindings {
			add("binding: %s %s", b.Kind(), specText(b))
		}
	}
	if len(es.Gauges) > 0 {
		add("")
		add("gauges:")
		for _, g := range es.Gauges {
			line := fmt.Sprintf("  %-9s %-10s %s", g.Name, g.Value, g.Level)
			if g.Trend != "" {
				line += " trend " + g.Trend
			}
			if g.Detail != "" {
				line += ", " + g.Detail
			}
			add("%s", line)
		}
	}
	if len(es.Metrics) > 0 {
		add("")
		add("metrics:")
		keys := make([]string, 0, len(es.Metrics))
		for k := range es.Metrics {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add("  %-22s %s", k, state.Num(es.Metrics[k]))
		}
	}
	if len(es.Conditions) > 0 {
		add("")
		add("conditions:")
		for _, c := range es.Conditions {
			line := "  " + c.Kind
			if c.Ref != "" {
				line += " " + c.Ref
			}
			if !c.Since.IsZero() {
				line += ", since " + c.Since.Format("15:04:05")
			}
			if c.Detail != "" {
				line += ": " + c.Detail
			}
			add("%s", line)
		}
	}
	if len(res.Recent) > 0 {
		add("")
		add("recent states (last 30 min):")
		for _, r := range res.Recent {
			add("  %s", r)
		}
	}
	if len(res.Events) > 0 {
		add("")
		add("change markers:")
		for _, e := range res.Events {
			line := fmt.Sprintf("  %s %s %s", e.At.Format("01-02 15:04"), e.Kind, e.Summary)
			if e.Author != "" {
				line += " by " + e.Author
			}
			add("%s", line)
		}
	}
	if len(res.Findings) > 0 {
		add("")
		add("findings attached:")
		for _, f := range res.Findings {
			loc := ""
			if f.File != "" {
				loc = " (" + f.File
				if f.Line > 0 {
					loc += fmt.Sprintf(":%d", f.Line)
				}
				loc += ")"
			}
			add("  [%s] %s%s", f.Severity, f.Title, loc)
			if f.SuggestedFix != "" {
				add("    fix: %s", f.SuggestedFix)
			}
		}
	}
	if len(res.Edges) > 0 {
		add("")
		add("edges:")
		keys := make([]string, 0, len(res.Edges))
		for k := range res.Edges {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add("  %-28s %s", k, res.Edges[k])
		}
	}
	if res.Issue != nil {
		add("")
		add("lens story (cause: %s):", t.LabelOf(res.Issue.Cause))
		for _, s := range res.Issue.Story {
			add("  %s", s)
		}
	}
	if len(es.Hosted) > 0 {
		add("")
		add("runs here:")
		for _, h := range es.Hosted {
			line := "  " + h.Label + ", " + string(h.State)
			if h.Known {
				line += fmt.Sprintf(", %d pods, %d ready", h.Pods, h.Ready)
				if h.Restarts > 0 {
					line += fmt.Sprintf(", %d restarts", h.Restarts)
				}
			} else {
				line += " (from runs_on, no pod placement reported)"
			}
			add("%s", line)
		}
	}
	// Detail: top waiting queries, kills, tasks, anything a probe left.
	if len(es.Detail) > 0 {
		keys := make([]string, 0, len(es.Detail))
		for k := range es.Detail {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			add("")
			add("detail:")
		}
		for _, k := range keys {
			switch v := es.Detail[k].(type) {
			case []string:
				add("  %s:", k)
				for i, s := range v {
					if i >= 10 {
						add("    … %d more", len(v)-10)
						break
					}
					add("    %s", s)
				}
			case []any:
				add("  %s:", k)
				for i, s := range v {
					if i >= 10 {
						add("    … %d more", len(v)-10)
						break
					}
					add("    %v", s)
				}
			default:
				add("  %-14s %v", k, v)
			}
		}
	}
	if len(res.K8sEvents) > 0 {
		add("")
		add("kubernetes events:")
		for i, l := range res.K8sEvents {
			if i >= 10 {
				break
			}
			add("  %s", l)
		}
	}
	// Logs take whatever budget is left, up to 50 lines.
	if len(res.Logs) > 0 {
		add("")
		budget := MaxLines - len(out) - 1
		if budget > 50 {
			budget = 50
		}
		if budget < 5 {
			budget = 5
		}
		start := 0
		if len(res.Logs) > budget {
			start = len(res.Logs) - budget
		}
		add("logs (last %d lines):", len(res.Logs)-start)
		for _, l := range res.Logs[start:] {
			add("  %s", l)
		}
	}
	if len(out) > MaxLines {
		out = append(out[:MaxLines-1], fmt.Sprintf("… trimmed to %d lines", MaxLines))
	}
	return out
}

func specText(spec model.ProbeSpec) string {
	keys := make([]string, 0, len(spec))
	for k := range spec {
		if k == "probe" || strings.HasPrefix(k, "_") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, spec[k]))
	}
	return strings.Join(parts, " ")
}

// EventsFor lists change markers touching an element, newest first.
func EventsFor(events []model.Event, id string, since time.Duration, now time.Time) []model.Event {
	var out []model.Event
	for _, e := range events {
		if (id == "" || e.Target == id) && e.At.After(now.Add(-since)) && !e.At.After(now) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}
