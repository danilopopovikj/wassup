package model

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Remap renames an id across every file in dir: topology, bindings,
// findings, thresholds, layout and open annotations. It returns the list of
// files touched.
func Remap(dir, oldID, newID string) ([]string, error) {
	if oldID == newID {
		return nil, fmt.Errorf("old and new id are the same")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(newID) {
		return nil, fmt.Errorf("new id %q must be a slug: lowercase letters, digits and dashes", newID)
	}
	cfg, err := Load(dir)
	if err != nil {
		if _, ok := err.(*ValidationError); !ok {
			return nil, err
		}
	}
	if _, ok := cfg.Topology.Component(oldID); !ok {
		if _, ok := cfg.Topology.Group(oldID); !ok {
			return nil, fmt.Errorf("no component or group %q in topology", oldID)
		}
	}
	if _, ok := cfg.Topology.Component(newID); ok {
		return nil, fmt.Errorf("id %q already exists", newID)
	}
	var touched []string

	// topology
	t := &cfg.Topology
	for i := range t.Groups {
		if t.Groups[i].ID == oldID {
			t.Groups[i].ID = newID
		}
		if t.Groups[i].Parent == oldID {
			t.Groups[i].Parent = newID
		}
	}
	for i := range t.Components {
		c := &t.Components[i]
		if c.ID == oldID {
			c.ID = newID
		}
		if c.Group == oldID {
			c.Group = newID
		}
		for j := range c.RunsOn {
			if c.RunsOn[j] == oldID {
				c.RunsOn[j] = newID
			}
		}
		if c.Roles != nil {
			if c.Roles.Primary == oldID {
				c.Roles.Primary = newID
			}
			for j := range c.Roles.Replicas {
				if c.Roles.Replicas[j] == oldID {
					c.Roles.Replicas[j] = newID
				}
			}
		}
	}
	for i := range t.Edges {
		if t.Edges[i].From == oldID {
			t.Edges[i].From = newID
		}
		if t.Edges[i].To == oldID {
			t.Edges[i].To = newID
		}
	}
	if err := WriteYAML(filepath.Join(dir, "topology.yaml"), t); err != nil {
		return touched, err
	}
	touched = append(touched, "topology.yaml")

	renameEdgeKey := func(k string) string {
		from, to, ok := strings.Cut(k, "->")
		if !ok {
			return k
		}
		if from == oldID {
			from = newID
		}
		if to == oldID {
			to = newID
		}
		return from + "->" + to
	}

	if cfg.Present["bindings.yaml"] {
		b := &cfg.Bindings
		if specs, ok := b.Components[oldID]; ok {
			delete(b.Components, oldID)
			b.Components[newID] = specs
		}
		edges := map[string][]ProbeSpec{}
		for k, v := range b.Edges {
			edges[renameEdgeKey(k)] = v
		}
		b.Edges = edges
		if err := WriteYAML(filepath.Join(dir, "bindings.yaml"), b); err != nil {
			return touched, err
		}
		touched = append(touched, "bindings.yaml")
	}
	if cfg.Present["findings.yaml"] {
		changed := false
		for i := range cfg.Findings.Findings {
			f := &cfg.Findings.Findings[i]
			if f.Component == oldID {
				f.Component = newID
				changed = true
			} else if nk := renameEdgeKey(f.Component); nk != f.Component {
				f.Component = nk
				changed = true
			}
		}
		if changed {
			if err := WriteYAML(filepath.Join(dir, "findings.yaml"), cfg.Findings); err != nil {
				return touched, err
			}
			touched = append(touched, "findings.yaml")
		}
	}
	if cfg.Present["thresholds.yaml"] {
		if set, ok := cfg.Thresholds.Components[oldID]; ok {
			delete(cfg.Thresholds.Components, oldID)
			cfg.Thresholds.Components[newID] = set
			if err := WriteYAML(filepath.Join(dir, "thresholds.yaml"), cfg.Thresholds); err != nil {
				return touched, err
			}
			touched = append(touched, "thresholds.yaml")
		}
	}
	if cfg.Present["layout.json"] {
		l := &cfg.Layout
		if p, ok := l.Components[oldID]; ok {
			delete(l.Components, oldID)
			l.Components[newID] = p
		}
		for i := range l.Collapsed {
			if l.Collapsed[i] == oldID {
				l.Collapsed[i] = newID
			}
		}
		wp := map[string][][2]int{}
		for k, v := range l.Waypoints {
			wp[renameEdgeKey(k)] = v
		}
		l.Waypoints = wp
		if err := cfg.SaveLayout(); err != nil {
			return touched, err
		}
		touched = append(touched, "layout.json")
	}
	if cfg.Present["state/annotations.json"] {
		changed := false
		for i := range cfg.Annotations.Annotations {
			an := &cfg.Annotations.Annotations[i]
			for j, ref := range an.Path {
				r, err := ParseRef(ref)
				if err != nil {
					continue
				}
				if r.ID == oldID {
					r.ID = newID
					an.Path[j] = r.String()
					changed = true
				} else if r.IsEdge() {
					if nk := renameEdgeKey(r.ID); nk != r.ID {
						r.ID = nk
						an.Path[j] = r.String()
						changed = true
					}
				}
			}
		}
		if changed {
			if err := SaveAnnotations(dir, cfg.Annotations); err != nil {
				return touched, err
			}
			touched = append(touched, "state/annotations.json")
		}
	}
	// Remove stale temp files left by an interrupted write, if any.
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".tmp") && strings.HasPrefix(d.Name(), ".") {
			_ = os.Remove(p)
		}
		return nil
	})
	return touched, nil
}
