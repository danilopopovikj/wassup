package discover

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/danilopopovikj/wassup/internal/discover/redact"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

// Network policies as evidence. An egress allowlist (Cilium toFQDNs) names
// the outside services a workload may reach. Whoever runs the system wrote
// it and the cluster enforces it, so it is a better list than the hosts a
// scan finds in code and .env files, which also holds what is no longer
// called. A policy allows; it does not prove a call. Its hosts are
// therefore drawn from the workloads the policy names by their labels, and
// only listed when it applies to a whole namespace.

// EgressPolicy is the egress side of a network policy, with where it was
// seen.
type EgressPolicy struct {
	k8s.NetworkPolicyInfo
	Source string `json:"source"` // manifest, cluster
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`

	// lines maps a host or a pattern to the line it is written on.
	lines map[string]int
}

// policyKinds are the kinds of object that hold egress rules.
var policyKinds = map[string]bool{"NetworkPolicy": true, "CiliumNetworkPolicy": true, "CiliumClusterwideNetworkPolicy": true}

// ref names the policy: "CiliumNetworkPolicy shop/api-egress".
func (p EgressPolicy) ref() string {
	if p.Namespace == "" {
		return p.Kind + " " + p.Name
	}
	return p.Kind + " " + p.Namespace + "/" + p.Name
}

// where says where the policy was seen, for a note.
func (p EgressPolicy) where() string {
	switch {
	case p.File == "":
		return "in the cluster"
	case p.Line > 0:
		return fmt.Sprintf("%s:%d", p.File, p.Line)
	}
	return p.File
}

// who words the pods the policy applies to.
func (p EgressPolicy) who() string {
	switch {
	case p.Expressions:
		return "the pods it chooses by an expression"
	case p.Selector != "":
		return "the pods " + p.Selector
	case p.Namespace != "":
		return "every pod of namespace " + p.Namespace
	}
	return "every pod"
}

// named reports whether the policy names the pods it applies to by their
// labels. One that applies to a whole namespace says what may be reached,
// not who reaches it.
func (p EgressPolicy) named() bool {
	return p.Selector != "" && !p.Expressions
}

// evidence cites the policy for something it allows, at the line where
// that is written when the policy comes from a file.
func (p EgressPolicy) evidence(what string) Evidence {
	line := p.Line
	if l, ok := p.lines[what]; ok {
		line = l
	}
	return Evidence{Source: p.Source, File: p.File, Line: line, Policy: true,
		Note: redact.Text(p.ref() + " allows " + p.who() + " to reach " + what)}
}

// appliesTo reports whether the policy applies to the pods of a candidate:
// a workload of the policy's namespace whose pods carry every label of the
// selector. A selector with expressions is not evaluated and applies to
// nothing that is known.
func (p EgressPolicy) appliesTo(c Candidate) bool {
	if c.Kind == "" || p.Expressions {
		return false
	}
	if p.Namespace != "" && c.Namespace != "" && p.Namespace != c.Namespace {
		return false
	}
	for k, v := range selectorMap(p.Selector) {
		if c.Labels[k] != v {
			return false
		}
	}
	return true
}

// allows reports whether the policy allows a host, by name or by pattern.
func (p EgressPolicy) allows(host string) bool {
	host = strings.ToLower(host)
	for _, h := range p.Hosts {
		if h == host {
			return true
		}
	}
	for _, pat := range p.Patterns {
		if patternMatches(pat, host) {
			return true
		}
	}
	return false
}

// patternMatches reports whether a host matches a matchPattern: "*" stands
// for any run of letters, digits, "-" and "_" within one label, "**." at
// the start for one or more labels, and "*" alone for every name.
func patternMatches(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if pattern == "*" {
		return true
	}
	expr := "^"
	if rest, ok := strings.CutPrefix(pattern, "**."); ok {
		expr += `(?:[-a-z0-9_]+\.)+`
		pattern = rest
	}
	for i, part := range strings.Split(pattern, "*") {
		if i > 0 {
			expr += `[-a-z0-9_]*`
		}
		expr += regexp.QuoteMeta(part)
	}
	re, err := regexp.Compile(expr + "$")
	return err == nil && re.MatchString(host)
}

// specific reports whether a pattern names a domain ("*.payments.example")
// rather than everything ("*", "*.com").
func specific(pattern string) bool {
	literal := strings.TrimLeft(pattern, "*.")
	return strings.Contains(literal, ".") && !strings.Contains(literal, "*")
}

// scalarLines maps every scalar of a YAML document to the first line it is
// written on.
func scalarLines(n *yaml.Node, out map[string]int) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		if _, seen := out[strings.ToLower(n.Value)]; !seen && n.Value != "" {
			out[strings.ToLower(n.Value)] = n.Line
		}
		return
	}
	for _, c := range n.Content {
		scalarLines(c, out)
	}
}

// scanPolicy reads a network policy from a manifest.
func scanPolicy(a *accumulator, rel string, line int, node *yaml.Node) {
	var obj map[string]any
	if err := node.Decode(&obj); err != nil {
		return
	}
	infos := k8s.EgressPolicies(obj)
	if len(infos) == 0 {
		return
	}
	lines := map[string]int{}
	scalarLines(node, lines)
	for _, info := range infos {
		if hasPlaceholder(info.Name) || hasPlaceholder(info.Namespace) {
			continue
		}
		if info.Kind != "CiliumClusterwideNetworkPolicy" {
			info.Namespace = a.namespaceOf(rel, info.Namespace)
		}
		a.policy(EgressPolicy{NetworkPolicyInfo: info, Source: "manifest", File: rel, Line: line, lines: lines})
	}
}

// policy records a network policy, once.
func (a *accumulator) policy(p EgressPolicy) {
	for _, have := range a.f.Policies {
		if have.Source == p.Source && have.File == p.File && have.Line == p.Line && have.ref() == p.ref() && have.Selector == p.Selector {
			return
		}
	}
	a.f.Policies = append(a.f.Policies, p)
}

// sameEvidence reports whether two pieces of evidence say the same thing
// from the same place.
func sameEvidence(a, b Evidence) bool {
	return a.Source == b.Source && a.File == b.File && a.Line == b.Line && a.Note == b.Note
}

// addEvidence appends a piece of evidence unless it is there already.
func addEvidence(list []Evidence, ev Evidence) []Evidence {
	for _, have := range list {
		if sameEvidence(have, ev) {
			return list
		}
	}
	return append(list, ev)
}

// cite adds evidence to a flow and, when the flow ends at an outside
// service, to that service: its confidence follows what is known of it.
func (a *accumulator) cite(l *Link, ev Evidence) {
	l.Evidence = addEvidence(l.Evidence, ev)
	if c := a.f.candidate(l.To); c != nil && c.Type == "external" {
		c.Evidence = addEvidence(c.Evidence, ev)
	}
}

// flowTo returns the flow from a component to a host, resolved or not.
func (a *accumulator) flowTo(from, host string) *Link {
	for i := range a.f.Links {
		if a.f.Links[i].From == from && a.f.Links[i].Host == host {
			return &a.f.Links[i]
		}
	}
	return nil
}

// allowed records that a policy allows from to reach host: as evidence on
// the flow when it is known already, as a new flow otherwise. Policies are
// applied again when the cluster is added, so it never records twice.
func (a *accumulator) allowed(from, host string, ev Evidence) {
	if from != "repo" {
		from = a.f.canonical(model.Slugify(from))
	}
	if l := a.flowTo(from, host); l != nil {
		a.cite(l, ev)
		return
	}
	a.f.Links = append(a.f.Links, Link{From: from, Host: host, Kind: "external", Evidence: []Evidence{ev}})
}

// applyPolicies turns what the network policies allow into evidence. A
// host a policy names is an outside service, and a flow from every workload
// the policy names by its labels. A policy for a whole namespace only lists
// the service and confirms the flows that are known already: it does not
// say which workload calls. A pattern confirms the hosts it matches and is
// otherwise noted, like address ranges and selectors: they name no single
// service, and drawing one would be a guess.
func (a *accumulator) applyPolicies() {
	for _, p := range a.f.Policies {
		var applies []string
		for _, c := range a.f.Candidates {
			if p.appliesTo(c) {
				applies = append(applies, c.ID)
			}
		}
		for _, host := range p.Hosts {
			if !usableHost(host) {
				continue
			}
			ev := p.evidence(host)
			if p.named() && len(applies) > 0 {
				for _, id := range applies {
					a.allowed(id, host, ev)
				}
				continue
			}
			a.allowed("repo", host, ev)
			for _, id := range applies {
				if l := a.flowTo(id, host); l != nil {
					a.cite(l, ev)
				}
			}
		}
		var rest []string
		for _, pat := range p.Patterns {
			rest = append(rest, "names like "+pat)
			if !specific(pat) {
				continue
			}
			for _, id := range applies {
				for i := range a.f.Links {
					if l := &a.f.Links[i]; l.From == id && l.Host != "" && patternMatches(pat, l.Host) {
						a.cite(l, p.evidence(pat))
					}
				}
			}
		}
		if len(p.CIDRs) > 0 {
			rest = append(rest, "the ranges "+strings.Join(p.CIDRs, ", "))
		}
		for _, e := range p.Endpoints {
			rest = append(rest, e)
		}
		if p.Open {
			a.note("%s (%s) allows %s to reach every destination: it is no list of what they call", p.ref(), p.where(), p.who())
		}
		if len(rest) > 0 {
			a.note("%s (%s) allows %s to reach %s: not drawn, a pattern, a range or a selector names no single service", p.ref(), p.where(), p.who(), strings.Join(rest, "; "))
		}
	}
}

// allowlistGap says what speaks against a call from a workload to an
// outside host: network policies apply to the workload, they are lists of
// names, and none of them allows the host. It returns "" when no policy
// applies or when the policies cannot tell (one allows every destination
// or an address range, one chooses its pods by an expression).
func (f *Findings) allowlistGap(from, host string) string {
	c := f.candidate(from)
	if c == nil {
		return ""
	}
	applying := 0
	for _, p := range f.Policies {
		if p.Expressions && (p.Namespace == "" || c.Namespace == "" || p.Namespace == c.Namespace) {
			return ""
		}
		if !p.appliesTo(*c) {
			continue
		}
		if p.Open || len(p.CIDRs) > 0 || p.allows(host) {
			return ""
		}
		applying++
	}
	if applying == 0 {
		return ""
	}
	return "no network policy that applies to " + from + " allows it"
}
