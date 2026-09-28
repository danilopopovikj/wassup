package discover

import (
	"sort"
	"strings"
)

// One product, one component. A chart installs a product as several
// workloads (a collector, a store, its coordination service, an agent on
// every node); on the diagram they are one box, because the person looking
// at it asks "is tracing up", not "is the ZooKeeper of the tracing store up".
//
// The workloads say what they belong to through their labels
// (app.kubernetes.io/part-of, app.kubernetes.io/instance, the Helm release).
// A release is only folded when it names a product the scanner already
// knows by its chart or image, and that product is one shape of the
// catalog: the application's own chart also puts one instance label on its
// api, its worker and its cache, and those must stay apart.

// productLabels are the labels that name what a workload belongs to, the
// most specific first.
var productLabels = []string{"app.kubernetes.io/part-of", "app.kubernetes.io/instance", "release"}

// workloadKinds are the kinds that run pods for as long as they exist.
var workloadKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true, "Rollout": true}

// tokens splits a name into its words: "signoz-k8s-infra" is signoz, k8s,
// infra.
func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// knownProduct looks for a product the scanner knows among the words of a
// name and returns it with its catalog type. It learns no names of its own:
// it reads the chart and image tables.
func knownProduct(name string) (product, typ string, ok bool) {
	whole := strings.ToLower(name)
	if typ, ok := helmChartMap[whole]; ok && typ != "custom" {
		return whole, typ, true
	}
	for _, t := range tokens(name) {
		if typ, ok := helmChartMap[t]; ok && typ != "custom" {
			return t, typ, true
		}
		for _, it := range imageTypes {
			if it.match == t {
				return t, it.typ, true
			}
		}
	}
	return "", "", false
}

// singleShape reports whether a catalog type is one thing on the diagram. A
// product that is a plain workload (an engine and its API) keeps its parts.
func singleShape(typ string) bool {
	return typ != "" && typ != "workload" && typ != "backgroundworker" && typ != "custom"
}

// hasToken reports whether word is s or one of the words of s.
func hasToken(s, word string) bool {
	if strings.EqualFold(s, word) {
		return true
	}
	for _, t := range tokens(s) {
		if t == word {
			return true
		}
	}
	return false
}

// productKeys returns the values of the labels that say what a workload
// belongs to.
func productKeys(c Candidate) []string {
	var out []string
	for _, l := range productLabels {
		if v := c.Labels[l]; v != "" {
			out = append(out, v)
		}
	}
	if v := c.Extra["release"]; v != "" {
		out = append(out, v)
	}
	return out
}

// collapseProducts folds the workloads of one product into one candidate
// and lists the others in its evidence.
func (a *accumulator) collapseProducts() {
	type product struct{ ns, name, typ string }
	products := map[string]product{}
	learn := func(ns, text string) {
		if name, typ, ok := knownProduct(text); ok && singleShape(typ) {
			products[ns+"/"+name] = product{ns, name, typ}
		}
	}
	for _, c := range a.f.Candidates {
		if !workloadKinds[c.Kind] {
			continue
		}
		for _, k := range productKeys(c) {
			learn(c.Namespace, k)
		}
		// An observability stack is also known by the image of its main
		// workload, when the chart left the labels out.
		if c.Type == "observability" {
			for _, it := range imageTypes {
				if !strings.Contains(it.match, "/") && strings.Contains(strings.ToLower(c.Image), it.match) {
					products[c.Namespace+"/"+it.match] = product{c.Namespace, it.match, it.typ}
				}
			}
		}
	}
	for _, key := range sortedKeys(products) {
		p := products[key]
		var members []Candidate
		for _, c := range a.f.Candidates {
			if !workloadKinds[c.Kind] || c.Namespace != p.ns || !(c.Type == p.typ || !singleShape(c.Type)) {
				continue // another shape of its own (a cache inside the release) stays a component
			}
			belongs := false
			for _, k := range productKeys(c) {
				belongs = belongs || hasToken(k, p.name)
			}
			if p.typ == "observability" {
				belongs = belongs || hasToken(c.Name, p.name) || hasToken(c.Image, p.name)
			}
			if belongs {
				members = append(members, c)
			}
		}
		if len(members) < 2 {
			continue
		}
		// The workload that is the product itself stands for the rest.
		rank := func(c Candidate) int {
			switch {
			case c.Type == p.typ && strings.EqualFold(c.Name, p.name):
				return 0
			case c.Type == p.typ:
				return 1
			case strings.EqualFold(c.Name, p.name):
				return 2
			}
			return 3
		}
		sort.SliceStable(members, func(i, j int) bool {
			ri, rj := rank(members[i]), rank(members[j])
			if ri != rj {
				return ri < rj
			}
			if len(members[i].Name) != len(members[j].Name) {
				return len(members[i].Name) < len(members[j].Name)
			}
			return members[i].ID < members[j].ID
		})
		primary := members[0].ID
		if c := a.f.candidate(primary); c != nil {
			c.Type = p.typ
			if l := labelForImage(p.name); l != "" {
				c.Label = l
			}
			c.Objects = uniq(append(c.Objects, objectRef(c.Kind, c.Namespace, c.Name)))
		}
		for _, m := range members[1:] {
			a.mergeInto(m.ID, primary, "part of "+p.name)
		}
	}
}

// mergeInto folds the candidate from into the candidate to: its addresses,
// its evidence and the flows that touched it now belong to to. The id is
// remembered, so that what is found later under the old id lands on to.
func (a *accumulator) mergeInto(from, to, why string) {
	a.reindex()
	fi, ok := a.byID[from]
	ti, ok2 := a.byID[to]
	if !ok || !ok2 || from == to {
		return
	}
	src := a.f.Candidates[fi]
	dst := &a.f.Candidates[ti]
	dst.Addresses = uniq(append(dst.Addresses, src.Addresses...))
	dst.Objects = uniq(append(dst.Objects, src.Objects...))
	if src.Kind != "" {
		dst.Objects = uniq(append(dst.Objects, objectRef(src.Kind, src.Namespace, src.Name)))
		dst.Evidence = append(dst.Evidence, Evidence{Source: "inference", Note: strings.TrimSpace(src.Kind+" "+src.Namespace+"/"+src.Name) + " is " + why})
	}
	dst.Evidence = append(dst.Evidence, src.Evidence...)
	if dst.Extra == nil {
		dst.Extra = map[string]string{}
	}
	for _, k := range []string{"hosts", "tables", "replication_slot"} {
		if v := src.Extra[k]; v != "" && dst.Extra[k] == "" {
			dst.Extra[k] = v
		}
	}
	a.f.Candidates = append(a.f.Candidates[:fi], a.f.Candidates[fi+1:]...)
	if a.f.merged == nil {
		a.f.merged = map[string]string{}
	}
	a.f.merged[from] = to
	a.reindex()
	// Flows follow the component; a flow inside the product is not a flow.
	var links []Link
	seen := map[string]bool{}
	for _, l := range a.f.Links {
		if l.From == from {
			l.From = to
		}
		if l.To == from {
			l.To = to
		}
		if l.From == l.To && l.To != "" {
			continue
		}
		key := l.From + "\x00" + l.To + "\x00" + l.Host
		if seen[key] {
			continue
		}
		seen[key] = true
		links = append(links, l)
	}
	a.f.Links = links
}

// mergeCodeOnly folds a component known only from the code into the
// workload that runs it. The code scanner sees that shapes are read from
// "electric" and an Ingress object says "the ingress"; the cluster calls
// the same things bookstore-electric and traefik. When exactly one workload
// of that type exists, they are the same component.
func (a *accumulator) mergeCodeOnly() {
	for _, pseudo := range []struct{ id, typ string }{{"electric", "syncengine"}, {ingressID, "ingress"}} {
		c := a.f.candidate(pseudo.id)
		if c == nil || c.Type != pseudo.typ || c.Kind != "" || c.Image != "" || c.Name != "" || len(c.Addresses) > 0 {
			continue
		}
		var real []string
		for _, o := range a.f.Candidates {
			if o.ID != pseudo.id && o.Type == pseudo.typ && workloadKinds[o.Kind] {
				real = append(real, o.ID)
			}
		}
		if len(real) == 1 {
			a.mergeInto(pseudo.id, real[0], "the same component")
		}
	}
}
