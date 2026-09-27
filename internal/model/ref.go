package model

import (
	"fmt"
	"strings"
	"time"
)

// RefScheme is the scheme of element references copied from the TUI.
const RefScheme = "wassup://"

// Ref is a parsed wassup:// reference.
type Ref struct {
	Kind string // component type, "edge" or "group"
	ID   string // component id, "from->to" for edges, group id
	At   time.Time
}

// IsEdge reports whether the ref points at an edge.
func (r Ref) IsEdge() bool { return r.Kind == "edge" }

// IsGroup reports whether the ref points at a group.
func (r Ref) IsGroup() bool { return r.Kind == "group" }

// String formats the ref.
func (r Ref) String() string {
	s := RefScheme + r.Kind + "/" + r.ID
	if !r.At.IsZero() {
		s += "@" + r.At.UTC().Format(time.RFC3339)
	}
	return s
}

// ComponentRef builds a ref for a component.
func ComponentRef(c Component) Ref { return Ref{Kind: c.Type, ID: c.ID} }

// EdgeRef builds a ref for an edge.
func EdgeRef(from, to string) Ref { return Ref{Kind: "edge", ID: EdgeID(from, to)} }

// ParseRef parses "wassup://type/id[@timestamp]". A bare id is accepted and
// resolved by the caller against the topology.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("empty ref")
	}
	var r Ref
	if i := strings.LastIndex(s, "@"); i > 0 {
		ts := s[i+1:]
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t2, err2 := time.Parse("15:04", ts)
			if err2 != nil {
				return Ref{}, fmt.Errorf("bad timestamp %q in ref", ts)
			}
			now := time.Now()
			t = time.Date(now.Year(), now.Month(), now.Day(), t2.Hour(), t2.Minute(), 0, 0, time.Local)
		}
		r.At = t
		s = s[:i]
	}
	if !strings.HasPrefix(s, RefScheme) {
		// bare id
		if strings.Contains(s, "->") {
			return Ref{Kind: "edge", ID: s, At: r.At}, nil
		}
		return Ref{Kind: "", ID: s, At: r.At}, nil
	}
	rest := strings.TrimPrefix(s, RefScheme)
	kind, id, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		return Ref{}, fmt.Errorf("ref %q must look like %s<type>/<id>", s, RefScheme)
	}
	r.Kind, r.ID = kind, id
	return r, nil
}

// Resolve fills the kind of a bare ref from the topology and checks the id exists.
func (t *Topology) Resolve(r Ref) (Ref, error) {
	if r.IsEdge() {
		for _, e := range t.Edges {
			if e.ID() == r.ID {
				return r, nil
			}
		}
		return r, fmt.Errorf("no edge %q in topology", r.ID)
	}
	if r.IsGroup() {
		for _, g := range t.Groups {
			if g.ID == r.ID {
				return r, nil
			}
		}
		return r, fmt.Errorf("no group %q in topology", r.ID)
	}
	if c, ok := t.Component(r.ID); ok {
		r.Kind = c.Type
		return r, nil
	}
	if r.Kind == "" {
		for _, g := range t.Groups {
			if g.ID == r.ID {
				r.Kind = "group"
				return r, nil
			}
		}
	}
	return r, fmt.Errorf("no component %q in topology", r.ID)
}
