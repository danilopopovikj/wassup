package model

import (
	"sort"
	"strings"
)

// Component returns a component by id, including db role instances.
func (t *Topology) Component(id string) (Component, bool) {
	for _, c := range t.Components {
		if c.ID == id {
			return c, true
		}
	}
	for _, c := range t.RoleInstances() {
		if c.ID == id {
			return c, true
		}
	}
	return Component{}, false
}

// Group returns a group by id.
func (t *Topology) Group(id string) (Group, bool) {
	for _, g := range t.Groups {
		if g.ID == id {
			return g, true
		}
	}
	return Group{}, false
}

// Edge returns an edge by id.
func (t *Topology) Edge(id string) (Edge, bool) {
	for _, e := range t.Edges {
		if e.ID() == id {
			return e, true
		}
	}
	return Edge{}, false
}

// RoleInstances returns the implicit components created by db roles: one per
// primary and replica, typed db, with Parent set to the declaring db.
func (t *Topology) RoleInstances() []Component {
	var out []Component
	for _, c := range t.Components {
		if c.Type != "db" || c.Roles == nil {
			continue
		}
		mk := func(id, role string) Component {
			return Component{ID: id, Type: "db", Label: id, Group: c.ID, Lane: c.Lane, Engine: c.Engine, Owner: c.Owner, Parent: c.ID, Notes: role}
		}
		if c.Roles.Primary != "" {
			out = append(out, mk(c.Roles.Primary, "primary"))
		}
		for _, r := range c.Roles.Replicas {
			out = append(out, mk(r, "replica"))
		}
	}
	return out
}

// AllComponents returns declared components plus role instances, in a
// stable order.
func (t *Topology) AllComponents() []Component {
	out := make([]Component, 0, len(t.Components))
	out = append(out, t.Components...)
	out = append(out, t.RoleInstances()...)
	return out
}

// HasRoles reports whether the component is a db that declares roles.
func (t *Topology) HasRoles(id string) bool {
	c, ok := t.Component(id)
	return ok && c.Type == "db" && c.Roles != nil && c.Roles.Primary != ""
}

// BoxFor returns the id of the box that draws an element. A db with roles is
// drawn as a container, so its edges anchor on the primary instance.
func (t *Topology) BoxFor(id string) string {
	c, ok := t.Component(id)
	if ok && c.Type == "db" && c.Roles != nil && c.Roles.Primary != "" {
		return c.Roles.Primary
	}
	return id
}

// AllGroups returns declared groups plus the implicit db-role containers.
func (t *Topology) AllGroups() []Group {
	out := append([]Group(nil), t.Groups...)
	for _, c := range t.Components {
		if c.Type == "db" && c.Roles != nil && c.Roles.Primary != "" {
			label := c.DisplayLabel()
			if c.Engine != "" {
				label += " (" + c.Engine + ")"
			}
			out = append(out, Group{ID: c.ID, Kind: "db", Label: label, Parent: c.Group})
		}
	}
	return out
}

// Neighbors returns the ids of components connected to id by any edge, plus
// runs_on relations in both directions.
func (t *Topology) Neighbors(id string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != id && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, e := range t.Edges {
		if e.From == id {
			add(e.To)
		}
		if e.To == id {
			add(e.From)
		}
	}
	for _, c := range t.AllComponents() {
		if c.ID == id {
			for _, n := range c.RunsOn {
				add(n)
			}
		}
		for _, n := range c.RunsOn {
			if n == id {
				add(c.ID)
			}
		}
	}
	return out
}

// Outgoing returns the edges leaving id.
func (t *Topology) Outgoing(id string) []Edge {
	var out []Edge
	for _, e := range t.Edges {
		if e.From == id {
			out = append(out, e)
		}
	}
	return out
}

// Incoming returns the edges entering id.
func (t *Topology) Incoming(id string) []Edge {
	var out []Edge
	for _, e := range t.Edges {
		if e.To == id {
			out = append(out, e)
		}
	}
	return out
}

// Hosted returns the workloads that run on node id.
func (t *Topology) Hosted(nodeID string) []Component {
	var out []Component
	for _, c := range t.Components {
		for _, n := range c.RunsOn {
			if n == nodeID {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// GroupChildren returns component ids directly in group id.
func (t *Topology) GroupChildren(groupID string) []string {
	var out []string
	for _, c := range t.AllComponents() {
		if c.Group == groupID {
			out = append(out, c.ID)
		}
	}
	return out
}

// GroupDescendants returns every component id inside group id, recursively.
func (t *Topology) GroupDescendants(groupID string) []string {
	ids := map[string]bool{}
	var walk func(g string)
	walk = func(g string) {
		for _, id := range t.GroupChildren(g) {
			ids[id] = true
		}
		for _, sub := range t.AllGroups() {
			if sub.Parent == g {
				walk(sub.ID)
			}
		}
	}
	walk(groupID)
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// EntryPoints returns component ids whose type starts a lens path: dns, lb,
// ingress and jobs. An entry-typed component fed by another entry-typed
// component (an ingress behind a load balancer) is a hop, not an entry.
func (t *Topology) EntryPoints() []string {
	var out []string
	for _, c := range t.Components {
		if !IsEntry(c) {
			continue
		}
		chained := false
		for _, e := range t.Incoming(c.ID) {
			if from, ok := t.Component(e.From); ok && IsEntry(from) {
				chained = true
				break
			}
		}
		if !chained {
			out = append(out, c.ID)
		}
	}
	return out
}

// LabelOf returns the display label of a component or the edge text for an edge.
func (t *Topology) LabelOf(id string) string {
	if c, ok := t.Component(id); ok {
		return c.DisplayLabel()
	}
	if e, ok := t.Edge(id); ok {
		if e.Label != "" {
			return e.Label
		}
		return t.LabelOf(e.From) + " → " + t.LabelOf(e.To)
	}
	if g, ok := t.Group(id); ok {
		if g.Label != "" {
			return g.Label
		}
	}
	return id
}

// Slugify turns free text into a stable id.
func Slugify(s string) string {
	var b strings.Builder
	last := '-'
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			last = r
		default:
			if last != '-' {
				b.WriteRune('-')
				last = '-'
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
