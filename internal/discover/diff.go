package discover

import (
	"fmt"
	"sort"
	"strings"

	"github.com/danilopopovikj/wassup/internal/model"
)

// Change is one line of a sync report.
type Change struct {
	Op       string     `json:"op"`   // added, removed, changed
	What     string     `json:"what"` // component, edge, binding
	ID       string     `json:"id"`
	Detail   string     `json:"detail,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

// Delta is what a fresh proposal says compared with the committed files.
type Delta struct {
	Changes []Change `json:"changes"`
	// matched maps proposal ids to current ids (a user may have renamed).
	matched map[string]string
}

// Empty reports whether nothing changed.
func (d *Delta) Empty() bool { return len(d.Changes) == 0 }

// Diff compares a proposal with the current configuration. Components match
// by id, else by label, else by a shared address noted in the current
// component's notes.
func Diff(cur *model.Config, p *Proposal) *Delta {
	d := &Delta{matched: map[string]string{}}
	curByID := map[string]model.Component{}
	curByLabel := map[string]string{}
	for _, c := range cur.Topology.AllComponents() {
		curByID[c.ID] = c
		curByLabel[strings.ToLower(c.DisplayLabel())] = c.ID
	}
	matchRoles := func(pc model.Component, curID string) {
		// a db matched to a db: its instances match by role and position
		cc := curByID[curID]
		if pc.Roles == nil || cc.Roles == nil {
			return
		}
		if pc.Roles.Primary != "" && cc.Roles.Primary != "" {
			d.matched[pc.Roles.Primary] = cc.Roles.Primary
		}
		for i, r := range pc.Roles.Replicas {
			if i < len(cc.Roles.Replicas) {
				d.matched[r] = cc.Roles.Replicas[i]
			}
		}
	}
	for _, c := range p.Topology.Components {
		if _, ok := curByID[c.ID]; ok {
			d.matched[c.ID] = c.ID
			matchRoles(c, c.ID)
			continue
		}
		if id, ok := curByLabel[strings.ToLower(c.Label)]; ok {
			d.matched[c.ID] = id
			matchRoles(c, id)
			continue
		}
		d.Changes = append(d.Changes, Change{Op: "added", What: "component", ID: c.ID, Detail: c.Type + " " + c.Label, Evidence: p.Evidence[c.ID]})
	}
	// Removed: current components the scan did not see, except role instances.
	seen := map[string]bool{}
	for _, id := range d.matched {
		seen[id] = true
	}
	for _, c := range cur.Topology.Components {
		if !seen[c.ID] {
			d.Changes = append(d.Changes, Change{Op: "removed", What: "component", ID: c.ID, Detail: c.Type + " " + c.DisplayLabel() + " not found in the repository"})
		}
	}
	// Type changes.
	for _, c := range p.Topology.Components {
		if id, ok := d.matched[c.ID]; ok {
			if cc := curByID[id]; cc.Type != c.Type && cc.Type != "custom" {
				d.Changes = append(d.Changes, Change{Op: "changed", What: "component", ID: id, Detail: fmt.Sprintf("type %s in the repository, %s in topology.yaml", c.Type, cc.Type)})
			}
		}
	}
	// Edges, mapped through matched ids.
	mapID := func(id string) string {
		if m, ok := d.matched[id]; ok {
			return m
		}
		return id
	}
	curEdges := map[string]model.Edge{}
	for _, e := range cur.Topology.Edges {
		curEdges[e.ID()] = e
	}
	propEdges := map[string]bool{}
	for _, e := range p.Topology.Edges {
		id := model.EdgeID(mapID(e.From), mapID(e.To))
		propEdges[id] = true
		if ce, ok := curEdges[id]; ok {
			if ce.Kind != e.Kind && !(ce.Kind == "tcp" || e.Kind == "tcp") {
				d.Changes = append(d.Changes, Change{Op: "changed", What: "edge", ID: id, Detail: fmt.Sprintf("kind %s in the repository, %s in topology.yaml", e.Kind, ce.Kind), Evidence: p.Evidence[e.ID()]})
			}
			continue
		}
		d.Changes = append(d.Changes, Change{Op: "added", What: "edge", ID: id, Detail: e.Kind, Evidence: p.Evidence[e.ID()]})
	}
	for id, e := range curEdges {
		if !propEdges[id] {
			_, fromKnown := d.matchedReverse(e.From)
			_, toKnown := d.matchedReverse(e.To)
			if fromKnown && toKnown {
				d.Changes = append(d.Changes, Change{Op: "removed", What: "edge", ID: id, Detail: "no longer seen between two components the scan still knows"})
			}
		}
	}
	// Bindings missing on matched components.
	for id, specs := range p.Bindings.Components {
		cid := mapID(id)
		if _, ok := curByID[cid]; !ok {
			continue
		}
		if len(cur.Bindings.Components[cid]) == 0 && len(specs) > 0 {
			var kinds []string
			for _, s := range specs {
				kinds = append(kinds, s.Kind())
			}
			d.Changes = append(d.Changes, Change{Op: "added", What: "binding", ID: cid, Detail: "unbound; proposed " + strings.Join(kinds, ", ")})
		}
	}
	sort.SliceStable(d.Changes, func(i, j int) bool {
		rank := map[string]int{"added": 0, "changed": 1, "removed": 2}
		if rank[d.Changes[i].Op] != rank[d.Changes[j].Op] {
			return rank[d.Changes[i].Op] < rank[d.Changes[j].Op]
		}
		if d.Changes[i].What != d.Changes[j].What {
			return d.Changes[i].What < d.Changes[j].What
		}
		return d.Changes[i].ID < d.Changes[j].ID
	})
	return d
}

func (d *Delta) matchedReverse(curID string) (string, bool) {
	for pid, cid := range d.matched {
		if cid == curID {
			return pid, true
		}
	}
	return "", false
}

// Apply merges additions from the proposal into the configuration. Existing
// components, labels, groups and layout are never touched; removed items go
// only with prune. It returns the number of components and edges added.
func Apply(cur *model.Config, p *Proposal, d *Delta, prune bool) (int, int) {
	mapID := func(id string) string {
		if m, ok := d.matched[id]; ok {
			return m
		}
		return id
	}
	added, addedEdges := 0, 0
	have := map[string]bool{}
	for _, c := range cur.Topology.AllComponents() {
		have[c.ID] = true
	}
	haveGroup := map[string]bool{}
	for _, g := range cur.Topology.Groups {
		haveGroup[g.ID] = true
	}
	for _, ch := range d.Changes {
		if ch.Op != "added" || ch.What != "component" {
			continue
		}
		pc, ok := findComp(p.Topology.Components, ch.ID)
		if !ok || have[pc.ID] {
			continue
		}
		// bring the group along when it is new
		if pc.Group != "" && !haveGroup[pc.Group] {
			for _, g := range p.Topology.Groups {
				if g.ID == pc.Group {
					if g.Parent != "" && !haveGroup[g.Parent] {
						g.Parent = ""
					}
					cur.Topology.Groups = append(cur.Topology.Groups, g)
					haveGroup[g.ID] = true
				}
			}
		}
		// runs_on only on nodes that exist
		var runs []string
		for _, n := range pc.RunsOn {
			if have[mapID(n)] {
				runs = append(runs, mapID(n))
			}
		}
		pc.RunsOn = runs
		cur.Topology.Components = append(cur.Topology.Components, pc)
		have[pc.ID] = true
		added++
		if specs, ok := p.Bindings.Components[pc.ID]; ok {
			if cur.Bindings.Components == nil {
				cur.Bindings.Components = map[string][]model.ProbeSpec{}
			}
			cur.Bindings.Components[pc.ID] = specs
		}
		if pc.Roles != nil {
			for _, inst := range append([]string{pc.Roles.Primary}, pc.Roles.Replicas...) {
				if specs, ok := p.Bindings.Components[inst]; ok {
					cur.Bindings.Components[inst] = specs
				}
			}
		}
		if cur.Layout.Components == nil {
			cur.Layout.Components = map[string]model.Placement{}
		}
	}
	haveEdge := map[string]bool{}
	for _, e := range cur.Topology.Edges {
		haveEdge[e.ID()] = true
	}
	for _, e := range p.Topology.Edges {
		from, to := mapID(e.From), mapID(e.To)
		id := model.EdgeID(from, to)
		if haveEdge[id] || !have[from] || !have[to] {
			continue
		}
		cur.Topology.Edges = append(cur.Topology.Edges, model.Edge{From: from, To: to, Kind: e.Kind, Label: e.Label})
		haveEdge[id] = true
		addedEdges++
		if specs, ok := p.Bindings.Edges[e.ID()]; ok {
			if cur.Bindings.Edges == nil {
				cur.Bindings.Edges = map[string][]model.ProbeSpec{}
			}
			cur.Bindings.Edges[id] = specs
		}
	}
	// Proposed bindings for components that had none.
	for id, specs := range p.Bindings.Components {
		cid := mapID(id)
		if have[cid] && len(cur.Bindings.Components[cid]) == 0 && len(specs) > 0 {
			if cur.Bindings.Components == nil {
				cur.Bindings.Components = map[string][]model.ProbeSpec{}
			}
			cur.Bindings.Components[cid] = specs
		}
	}
	if prune {
		drop := map[string]bool{}
		for _, ch := range d.Changes {
			if ch.Op == "removed" && ch.What == "component" {
				drop[ch.ID] = true
			}
		}
		var comps []model.Component
		for _, c := range cur.Topology.Components {
			if !drop[c.ID] {
				comps = append(comps, c)
			}
		}
		cur.Topology.Components = comps
		var edges []model.Edge
		for _, e := range cur.Topology.Edges {
			if !drop[e.From] && !drop[e.To] {
				edges = append(edges, e)
			}
		}
		cur.Topology.Edges = edges
		for id := range drop {
			delete(cur.Bindings.Components, id)
			delete(cur.Layout.Components, id)
		}
	}
	return added, addedEdges
}
