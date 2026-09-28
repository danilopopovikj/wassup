package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/app"
	"github.com/danilopopovikj/wassup/internal/bind"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// probeSelection says which bindings a run of `wassup probe` starts.
type probeSelection struct {
	// Tier is the highest tier that runs.
	Tier int
	// ID, when set, is the one element whose bindings run.
	ID string
}

// tierOf returns the tier of a binding. A kind that is not known has none
// to look up and runs with the first tier, where its error shows at once.
func tierOf(spec model.ProbeSpec) int {
	acc, _ := probe.AccessFor(spec.Kind())
	return acc.Tier
}

// runs reports whether a binding is part of the run.
func (s probeSelection) runs(target string, spec model.ProbeSpec) bool {
	if s.ID != "" && target != s.ID {
		return false
	}
	return tierOf(spec) <= s.Tier
}

// readsCluster reports whether a binding of the run reads the cluster,
// itself or on the way to its source.
func (s probeSelection) readsCluster(cfg *model.Config) bool {
	reads := func(target string, specs []model.ProbeSpec) bool {
		for _, spec := range specs {
			if !s.runs(target, spec) {
				continue
			}
			acc, _ := probe.AccessFor(spec.Kind())
			if len(acc.RBAC) > 0 || strings.HasPrefix(spec.String("via"), "k8s.") {
				return true
			}
		}
		return false
	}
	for id, specs := range cfg.Bindings.Components {
		if reads(id, specs) {
			return true
		}
	}
	for id, specs := range cfg.Bindings.Edges {
		if reads(id, specs) {
			return true
		}
	}
	return false
}

// split returns the bindings of an element that run and the kinds of those
// that wait for a later tier.
func (s probeSelection) split(target string, specs []model.ProbeSpec) (run []model.ProbeSpec, later []string) {
	for _, spec := range specs {
		switch {
		case s.runs(target, spec):
			run = append(run, spec)
		case s.ID == "" || target == s.ID:
			later = append(later, fmt.Sprintf("%s (tier %d)", spec.Kind(), tierOf(spec)))
		}
	}
	return run, later
}

// rows is probeRows over the bindings of the run. An element whose bindings
// all wait for a later tier is reported as later: it was not asked, so it
// is neither bound nor a fault. With an id the one element is reported, with
// everything its probes said.
func (s probeSelection) rows(cfg *model.Config, snap *model.Snapshot, joined map[string]*bind.Joined, inst []app.Instance) []probeRow {
	picked := *cfg
	picked.Bindings = model.Bindings{Components: map[string][]model.ProbeSpec{}, Edges: map[string][]model.ProbeSpec{}}
	later := map[string][]string{}
	for id, specs := range cfg.Bindings.Components {
		run, l := s.split(id, specs)
		if len(run) > 0 {
			picked.Bindings.Components[id] = run
		}
		later[id] = l
	}
	for id, specs := range cfg.Bindings.Edges {
		run, l := s.split(id, specs)
		if len(run) > 0 {
			picked.Bindings.Edges[id] = run
		}
		later[id] = l
	}
	var out []probeRow
	for _, r := range probeRows(&picked, snap, joined, inst) {
		if s.ID != "" && r.ID != s.ID {
			continue
		}
		if l := later[r.ID]; len(l) > 0 && r.Probes == "" {
			r.Bound = false
			r.State = string(model.Idle)
			r.Later = strings.Join(l, ", ")
			r.Label = "not asked yet, " + r.Later
		}
		if j := joined[r.ID]; j != nil && r.Later == "" {
			r.Advice = advice(j.Detail)
			if s.ID != "" {
				r.Conditions, r.Detail = j.Conditions, j.Detail
			}
		}
		out = append(out, r)
	}
	return out
}

// advice returns the notes of a detail: the values whose key ends in _note,
// which is where a probe says what it could not see and what would let it.
func advice(detail map[string]any) []string {
	var out []string
	for _, k := range sortedKeys(detail) {
		if note, ok := detail[k].(string); ok && strings.HasSuffix(k, "_note") && note != "" {
			out = append(out, note)
		}
	}
	return out
}

// elementID resolves what the user typed to the id of a component or an
// edge: the id itself, from->to, or a wassup:// ref.
func elementID(cfg *model.Config, arg string) (string, error) {
	id := arg
	if strings.HasPrefix(arg, "wassup://") {
		ref, err := model.ParseRef(arg)
		if err != nil {
			return "", err
		}
		id = ref.ID
	}
	if _, ok := cfg.Topology.Component(id); ok {
		return id, nil
	}
	for _, e := range cfg.Topology.Edges {
		if e.ID() == id {
			return id, nil
		}
	}
	var near []string
	for _, c := range cfg.Topology.AllComponents() {
		if strings.Contains(c.ID, id) || strings.Contains(strings.ToLower(c.Label), strings.ToLower(id)) {
			near = append(near, c.ID)
		}
	}
	for _, e := range cfg.Topology.Edges {
		if strings.Contains(e.ID(), id) {
			near = append(near, e.ID())
		}
	}
	if len(near) > 0 {
		sort.Strings(near)
		return "", fmt.Errorf("no component or edge %q; did you mean %s", arg, strings.Join(near, ", "))
	}
	return "", fmt.Errorf("no component or edge %q; `wassup validate --json` lists the ids", arg)
}

// progressLine is one probe that finished, as it is printed while the
// others still run.
func progressLine(p app.Progress) string {
	line := fmt.Sprintf("[%d/%d] %-7s %-18s %-24s %s", p.Done, p.Total, p.Status, p.Kind, p.Target, p.Elapsed.Round(100*time.Millisecond))
	if p.Error != "" {
		line += "  " + firstLine(p.Error)
	}
	return line
}

// firstLine cuts a message to its first line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// printValues prints everything the probes of one element said.
func printValues(r probeRow) {
	if r.Note != "" {
		fmt.Println("  note: " + r.Note)
	}
	for _, res := range r.Results {
		fmt.Printf("\n  %s: %s\n", res.Probe, res.Status)
		if res.Error != "" {
			for _, l := range strings.Split(res.Error, "\n") {
				fmt.Println("    " + l)
			}
		}
		for _, k := range sortedKeys(res.Metrics) {
			fmt.Printf("    %-28s %s\n", k, number(res.Metrics[k]))
		}
	}
	if len(r.Conditions) > 0 {
		fmt.Println("\n  conditions:")
		for _, c := range r.Conditions {
			line := "    " + c.Kind
			if c.Detail != "" {
				line += ": " + c.Detail
			}
			fmt.Println(line)
		}
	}
	if len(r.Detail) > 0 {
		fmt.Println("\n  detail:")
		for _, k := range sortedKeys(r.Detail) {
			fmt.Printf("    %-28s %v\n", k, r.Detail[k])
		}
	}
	fmt.Println()
}

// number prints a value without the noise of a float that is whole.
func number(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.3f", v)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
