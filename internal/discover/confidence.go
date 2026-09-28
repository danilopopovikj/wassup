package discover

import (
	"fmt"
	"sort"

	"github.com/danilopopovikj/wassup/internal/model"
)

// Confidence levels.
const (
	High   = "high"
	Medium = "medium"
	Low    = "low"
)

// What a confidence rests on.
const (
	BasisCluster    = "cluster"
	BasisRepository = "repository"
)

// Confidence says how far a proposed component or edge can be trusted and
// what that rests on, so that a review starts with what is least certain.
type Confidence struct {
	Level string `json:"level"` // high, medium, low
	Basis string `json:"basis"` // cluster, repository
	// Citation is the strongest piece of evidence, as one line.
	Citation string `json:"citation,omitempty"`
	// Caveat says what speaks against it, when something does.
	Caveat string `json:"caveat,omitempty"`
}

// weight orders the sources of evidence by what they prove:
//
//	3  the live cluster shows it: a running workload, a variable on a
//	   running workload, an Ingress backend, a network policy in force
//	2  the repository declares it: a manifest, Helm values, Terraform
//	1  the repository mentions it: code, a .env file
//	0  nothing was seen, it was concluded from something else
func weight(e Evidence) int {
	switch e.Source {
	case "cluster":
		return 3
	case "manifest", "helm", "terraform":
		return 2
	case "code", "dotenv":
		return 1
	}
	return 0
}

// Rate is the rule for confidence, a function of the evidence alone: what
// the cluster shows is high, what the repository declares is medium, what
// is only mentioned in code or a .env file, or concluded, is low. The
// strongest piece of evidence decides and is the one cited.
func Rate(evidence []Evidence) Confidence {
	c := Confidence{Level: Low, Basis: BasisRepository}
	if len(evidence) == 0 {
		return c
	}
	best := strongestFirst(evidence)[0]
	c.Citation = best.String()
	switch weight(best) {
	case 3:
		c.Level, c.Basis = High, BasisCluster
	case 2:
		c.Level = Medium
	}
	return c
}

// strongestFirst returns the evidence in the order it is cited: by weight,
// and among equals what a network policy says before the rest. The order
// within one kind stays as it was found.
func strongestFirst(evidence []Evidence) []Evidence {
	out := append([]Evidence(nil), evidence...)
	sort.SliceStable(out, func(i, j int) bool {
		if wi, wj := weight(out[i]), weight(out[j]); wi != wj {
			return wi > wj
		}
		return out[i].Policy && !out[j].Policy
	})
	return out
}

// rate gives every component and every edge of the proposal its
// confidence, puts the evidence in the order it is cited, and marks the
// outside services that only the code mentions while the network policies
// of the caller allow something else.
func (p *Proposal) rate(f *Findings) {
	p.Confidence = map[string]Confidence{}
	for id, ev := range p.Evidence {
		p.Evidence[id] = strongestFirst(ev)
	}
	for _, c := range p.Topology.Components {
		p.Confidence[c.ID] = Rate(p.Evidence[c.ID])
	}
	for _, e := range p.Topology.Edges {
		p.Confidence[e.ID()] = Rate(p.Evidence[e.ID()])
	}
	for _, l := range f.Links {
		if l.Host == "" || l.From == "" || l.To == "" {
			continue
		}
		id := model.EdgeID(l.From, l.To)
		c, ok := p.Confidence[id]
		to, _ := findComp(p.Topology.Components, l.To)
		if !ok || c.Level != Low || to.Type != "external" {
			continue
		}
		if gap := f.allowlistGap(l.From, l.Host); gap != "" {
			c.Caveat = gap
			p.Confidence[id] = c
			p.Notes = append(p.Notes, fmt.Sprintf("%s -> %s: only the code mentions %s and %s; confirm the call still exists", l.From, l.To, l.Host, gap))
		}
	}
	p.Notes = uniqInOrder(p.Notes)
}

// deriveLive derives the flows of a running workload from the environment
// the cluster reports for it, and from nothing else. The candidate also
// holds what the repository says about its environment; a flow read from
// that is cited to its file, and citing it to the cluster would call proven
// what was only declared.
func (a *accumulator) deriveLive(id string, env map[string]string, cmds []string) {
	c := a.f.candidate(id)
	if c == nil {
		return
	}
	all := c.Env
	c.Env = env
	a.deriveLinks(id, cmds, "cluster", "", 0)
	// Deriving adds candidates, which may move this one in memory.
	if c = a.f.candidate(id); c != nil {
		c.Env = all
	}
}
