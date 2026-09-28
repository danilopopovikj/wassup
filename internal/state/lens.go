package state

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
)

// depthMap is the distance of every component from the nearest entry point,
// following edges forward and runs_on downward. Unreachable components get a
// large depth so they still sort as "deep".
func (c *ctx) depthMap() map[string]float64 {
	depth := map[string]float64{}
	entries := c.t.EntryPoints()
	// Forward BFS from every traffic entry first; jobs seed only what traffic
	// does not reach, so a cron job feeding the workers does not make the
	// workers look shallow.
	var queue, jobs []string
	for _, e := range entries {
		if comp, ok := c.t.Component(e); ok && comp.Type == "job" {
			jobs = append(jobs, e)
			continue
		}
		depth[e] = 0
		queue = append(queue, e)
	}
	next := func(id string) []string {
		var out []string
		for _, e := range c.t.Outgoing(id) {
			out = append(out, e.To)
		}
		if comp, ok := c.t.Component(id); ok {
			out = append(out, comp.RunsOn...)
			if comp.Type == "database" && comp.Roles != nil {
				if comp.Roles.Primary != "" {
					out = append(out, comp.Roles.Primary)
				}
				out = append(out, comp.Roles.Replicas...)
			}
		}
		return out
	}
	bfs := func() {
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			for _, n := range next(id) {
				if _, seen := depth[n]; !seen {
					depth[n] = depth[id] + 1
					queue = append(queue, n)
				}
			}
		}
	}
	bfs()
	for _, j := range jobs {
		if _, seen := depth[j]; !seen {
			depth[j] = 0
			queue = append(queue, j)
		}
	}
	bfs()
	// Nodes sit beneath every workload they host.
	for _, comp := range c.t.Components {
		if comp.Type != "node" {
			continue
		}
		best := -1.0
		for _, w := range c.t.Hosted(comp.ID) {
			if d, ok := depth[w.ID]; ok && d+1 > best {
				best = d + 1
			}
		}
		if best >= 0 {
			depth[comp.ID] = best
		}
	}
	// Role instances sit just below their container.
	for _, comp := range c.t.Components {
		if comp.Type == "database" && comp.Roles != nil {
			if d, ok := depth[comp.ID]; ok {
				if comp.Roles.Primary != "" {
					depth[comp.Roles.Primary] = d + 0.25
				}
				for _, r := range comp.Roles.Replicas {
					depth[r] = d + 0.25
				}
			}
		}
	}
	// Anything unreachable forward: undirected BFS from reached nodes.
	for _, comp := range c.t.AllComponents() {
		if _, ok := depth[comp.ID]; ok {
			continue
		}
		best := math.Inf(1)
		for _, n := range c.t.Neighbors(comp.ID) {
			if d, ok := depth[n]; ok && d+1 < best {
				best = d + 1
			}
		}
		if math.IsInf(best, 1) {
			best = 50
		}
		depth[comp.ID] = best
	}
	return depth
}

// pathTo returns the component ids from the nearest entry point to target,
// walking parents back along decreasing depth.
func (c *ctx) pathTo(target string, depth map[string]float64) []string {
	path := []string{target}
	cur := target
	for i := 0; i < 64; i++ {
		d := depth[cur]
		if d == 0 {
			break
		}
		// Pick the immediate predecessor: the deepest neighbor above cur,
		// preferring one that is itself in trouble.
		best := ""
		bestD := -1.0
		bestSev := model.Severity(-1)
		consider := func(n string) {
			nd, ok := depth[n]
			if !ok || nd >= d {
				return
			}
			sev := c.snap.Components[n].Severity
			if sev < model.Warn {
				sev = model.Info
			} else {
				sev = model.Warn
			}
			switch {
			case best == "", sev > bestSev, sev == bestSev && nd > bestD, sev == bestSev && nd == bestD && n < best:
				best, bestD, bestSev = n, nd, sev
			}
		}
		for _, e := range c.t.Incoming(cur) {
			consider(e.From)
		}
		for _, comp := range c.t.AllComponents() {
			for _, n := range comp.RunsOn {
				if n == cur {
					consider(comp.ID)
				}
			}
			if comp.Type == "database" && comp.Roles != nil {
				if comp.Roles.Primary == cur {
					consider(comp.ID)
				}
				for _, r := range comp.Roles.Replicas {
					if r == cur {
						consider(comp.ID)
					}
				}
			}
		}
		if best == "" {
			for _, n := range c.t.Neighbors(cur) {
				consider(n)
			}
		}
		if best == "" {
			break
		}
		path = append([]string{best}, path...)
		cur = best
	}
	return path
}

// withEdges interleaves edge ids between consecutive components of a path.
func (c *ctx) withEdges(comps []string) []string {
	var out []string
	for i, id := range comps {
		if i > 0 {
			prev := comps[i-1]
			if _, ok := c.t.Edge(model.EdgeID(prev, id)); ok {
				out = append(out, model.EdgeID(prev, id))
			} else if _, ok := c.t.Edge(model.EdgeID(id, prev)); ok {
				out = append(out, model.EdgeID(id, prev))
			}
		}
		out = append(out, id)
	}
	return out
}

func (c *ctx) elem(id string) (model.ElementState, bool) {
	if es, ok := c.snap.Components[id]; ok {
		return es, false
	}
	return c.snap.Edges[id], true
}

// issues implements the cause rule: one issue per independent lit path,
// the cause being the deepest element on the path, then the worst severity,
// then a saturated gauge over a downstream failure, then the nearest change
// marker.
func (c *ctx) issues() []model.Issue {
	depth := c.depthMap()
	edgeDepth := func(id string) float64 {
		e, _ := c.t.Edge(id)
		return depth[e.From] + 0.5
	}
	// Affected elements.
	type aff struct {
		id   string
		edge bool
		sev  model.Severity
	}
	var affected []aff
	for id, es := range c.snap.Components {
		if es.Severity >= model.Warn && es.Marker != model.MarkerUnbound {
			affected = append(affected, aff{id, false, es.Severity})
		}
	}
	for id, es := range c.snap.Edges {
		if es.Severity >= model.Warn && es.Marker != model.MarkerUnbound {
			affected = append(affected, aff{id, true, es.Severity})
		}
	}
	if len(affected) == 0 {
		return nil
	}
	sort.Slice(affected, func(i, j int) bool { return affected[i].id < affected[j].id })

	// Lit path per affected element.
	paths := make([][]string, len(affected))
	for i, a := range affected {
		if a.edge {
			e, _ := c.t.Edge(a.id)
			p := c.pathTo(e.From, depth)
			p = append(p, e.To)
			paths[i] = c.withEdges(p)
		} else {
			p := c.pathTo(a.id, depth)
			// An affected entry point (a failing cron job or workflow) is a
			// symptom of whatever it feeds: walk one hop forward to an
			// affected consumer so the two light up as one issue.
			if depth[a.id] == 0 {
				for _, e := range c.t.Outgoing(a.id) {
					if to := c.snap.Components[e.To]; to.Severity >= model.Warn {
						p = append(p, e.To)
						break
					}
				}
			}
			paths[i] = c.withEdges(p)
		}
	}
	// Merge overlapping paths (union-find).
	parent := make([]int, len(affected))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	setOf := func(p []string) map[string]bool {
		m := map[string]bool{}
		for _, id := range p {
			if es, isEdge := c.elem(id); !isEdge {
				// healthy entry points are shared by every path and never
				// make two issues one
				if depth[id] > 0 || es.Severity >= model.Warn {
					m[id] = true
				}
			} else {
				m[id] = true
			}
		}
		return m
	}
	sets := make([]map[string]bool, len(affected))
	for i := range affected {
		sets[i] = setOf(paths[i])
		sets[i][affected[i].id] = true
	}
	for i := range affected {
		for j := i + 1; j < len(affected); j++ {
			overlap := false
			for id := range sets[i] {
				if sets[j][id] {
					overlap = true
					break
				}
			}
			if overlap {
				parent[find(j)] = find(i)
			}
		}
	}
	groups := map[int][]int{}
	for i := range affected {
		r := find(i)
		groups[r] = append(groups[r], i)
	}
	var out []model.Issue
	for _, members := range groups {
		lit := map[string]bool{}
		var ordered []string
		worst := model.Info
		for _, m := range members {
			for _, id := range paths[m] {
				if !lit[id] {
					lit[id] = true
					ordered = append(ordered, id)
				}
			}
			worst = worst.Max(affected[m].sev)
		}
		// Order lit elements top to bottom by depth.
		dOf := func(id string) float64 {
			if _, isEdge := c.elem(id); isEdge {
				return edgeDepth(id)
			}
			return depth[id]
		}
		sort.SliceStable(ordered, func(i, j int) bool {
			if dOf(ordered[i]) != dOf(ordered[j]) {
				return dOf(ordered[i]) < dOf(ordered[j])
			}
			return ordered[i] < ordered[j]
		})
		// Cause.
		cause := ""
		var causeD float64 = -1
		var causeSev model.Severity
		causeGauge, causeMarker := false, false
		for _, id := range ordered {
			es, _ := c.elem(id)
			if es.Severity < model.Warn {
				continue
			}
			if comp, ok := c.t.Component(id); ok && comp.Parent != "" && lit[comp.Parent] {
				if ps := c.snap.Components[comp.Parent]; ps.Severity >= es.Severity {
					continue // the container speaks for its instance
				}
			}
			d := dOf(id)
			redGauge := false
			for _, g := range es.Gauges {
				if g.Level == "red" {
					redGauge = true
				}
			}
			marker := c.hasMarker(id)
			better := false
			switch {
			case cause == "":
				better = true
			case d > causeD:
				better = true
			case d == causeD && es.Severity > causeSev:
				better = true
			case d == causeD && es.Severity == causeSev && redGauge && !causeGauge:
				better = true
			case d == causeD && es.Severity == causeSev && redGauge == causeGauge && marker && !causeMarker:
				better = true
			}
			if better {
				cause, causeD, causeSev, causeGauge, causeMarker = id, d, es.Severity, redGauge, marker
			}
		}
		// An edge is never blamed over the affected element it feeds: the
		// engineer acts on the destination, the line only carries the symptom.
		if cause != "" {
			if e, isEdge := c.t.Edge(cause); isEdge {
				if to := c.snap.Components[e.To]; lit[e.To] && to.Severity >= c.sevOf(cause) {
					cause = e.To
				}
			}
		}
		// The affected element with the worst severity names the issue.
		issueID := affected[members[0]].id
		for _, m := range members {
			if affected[m].sev > c.sevOf(issueID) {
				issueID = affected[m].id
			}
		}
		out = append(out, model.Issue{
			ID: issueID, Cause: cause, Severity: worst, Path: ordered,
			Story: c.story(ordered, cause),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (c *ctx) sevOf(id string) model.Severity {
	es, _ := c.elem(id)
	return es.Severity
}

func (c *ctx) hasMarker(id string) bool {
	for _, e := range c.in.Events {
		if e.Target == id {
			return true
		}
	}
	return false
}

// story builds one sentence per hop, top to bottom, with the cause last.
func (c *ctx) story(path []string, cause string) []string {
	var lines []string
	causeLine := ""
	pathSet := map[string]bool{}
	for _, id := range path {
		pathSet[id] = true
	}
	for i, id := range path {
		es, isEdge := c.elem(id)
		var s string
		if isEdge {
			s = c.edgeSentence(id, es, path, i)
		} else {
			s = c.componentSentence(id, es, path, i)
		}
		if s == "" {
			continue
		}
		if id == cause {
			causeLine = s
			continue
		}
		lines = append(lines, splitSentences(s)...)
	}
	if causeLine == "" && cause != "" {
		es, _ := c.elem(cause)
		causeLine = c.t.LabelOf(cause) + " is " + es.Label
	}
	causeComp, isComp := c.t.Component(cause)
	if isComp && causeComp.Type == "external" {
		// The cause is outside our control: say so first.
		lines = append(splitSentences(causeLine), lines...)
	} else if causeLine != "" {
		lines = append(lines, splitSentences(causeLine)...)
	}
	if last := c.lastChange(pathSet); last != "" {
		lines = append(lines, last)
	}
	return dedupe(lines)
}

func splitSentences(s string) []string {
	parts := strings.Split(s, "; ")
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// lastChange returns "last change on this path: deploy of api, 09:48, by x".
func (c *ctx) lastChange(path map[string]bool) string {
	var best *model.Event
	for i := range c.in.Events {
		e := &c.in.Events[i]
		if !path[e.Target] {
			continue
		}
		if e.At.After(c.in.Now) {
			continue
		}
		if best == nil || e.At.After(best.At) {
			best = e
		}
	}
	if best == nil {
		return ""
	}
	what := best.Kind
	switch best.Kind {
	case "deploy":
		what = "deploy of " + best.Target
		if best.Summary != "" {
			what = best.Summary
		}
	case "terraform":
		what = "terraform apply"
	case "node":
		what = "reboot of " + Lower(c.t.LabelOf(best.Target))
	case "scale":
		what = "scale of " + Lower(c.t.LabelOf(best.Target))
		if best.Summary != "" {
			what = best.Summary
		}
	case "cert":
		what = "certificate renewal at " + Lower(c.t.LabelOf(best.Target))
	default:
		if best.Summary != "" {
			what = best.Summary
		}
	}
	s := "last change on this path: " + what + ", " + Clock(best.At)
	if best.Author != "" {
		s += ", by " + best.Author
	}
	return s
}

func (c *ctx) componentSentence(id string, es model.ElementState, path []string, i int) string {
	comp, _ := c.t.Component(id)
	label := comp.DisplayLabel()
	// The next edge on the path, if any.
	var nextEdge *model.ElementState
	var nextEdgeID string
	if i+1 < len(path) {
		if e, isEdge := c.elem(path[i+1]); isEdge {
			nextEdge, nextEdgeID = &e, path[i+1]
		}
	}
	rateOf := func() string {
		if es.Rate > 0 {
			return Num(es.Rate) + "/s"
		}
		for _, e := range c.t.Outgoing(id) {
			if ee := c.snap.Edges[e.ID()]; ee.Rate > 0 {
				return Num(ee.Rate) + "/s"
			}
		}
		for _, e := range c.t.Incoming(id) {
			if ee := c.snap.Edges[e.ID()]; ee.Rate > 0 {
				return Num(ee.Rate) + "/s"
			}
		}
		return ""
	}
	switch comp.Type {
	case "loadbalancer", "ingress", "dns":
		var parts []string
		behind := c.behindEntry(id)
		if comp.Type != "loadbalancer" && behind && es.State != model.Failing && es.Severity < model.Warn {
			return ""
		}
		if comp.Type == "dns" && es.State != model.Failing && es.Severity < model.Warn {
			return ""
		}
		if es.State == model.Failing {
			if _, ok := model.HasCondition(es.Conditions, model.CondCertExpired); ok {
				parts = append(parts, "requests arrive")
				cnd, _ := model.HasCondition(es.Conditions, model.CondCertExpired)
				s := "the certificate at the " + Lower(label) + " expired"
				if !cnd.Since.IsZero() {
					s += " " + Ago(c.in.Now, cnd.Since) + " ago"
				}
				parts = append(parts, s)
				parts = append(parts, renewalNotes(es)...)
				return strings.Join(parts, "; ")
			}
			return Lower(label) + " is " + es.Label
		}
		h, okH := es.Metrics["targets_healthy"]
		t, okT := es.Metrics["targets_total"]
		if okH && okT && h < t {
			parts = append(parts, fmt.Sprintf("traffic reaches %s of %s nodes", Num(h), Num(t)))
		} else {
			if r := rateOf(); r != "" {
				parts = append(parts, "requests arrive, "+r)
			} else {
				parts = append(parts, "requests arrive")
			}
			if comp.Type == "loadbalancer" {
				n := c.nodeCount(id)
				if okT {
					parts = append(parts, fmt.Sprintf("load balancer passes them to %s of %s nodes", Num(h), Num(t)))
				} else if n > 0 {
					parts = append(parts, fmt.Sprintf("load balancer passes them to %d %s", n, Plural(float64(n), "node", "nodes")))
				}
			}
		}
		if days, ok := es.Metrics["cert_days"]; ok && es.Severity >= model.Warn && es.State != model.Failing {
			parts = append(parts, fmt.Sprintf("the certificate at the %s expires in %s %s", Lower(label), Num(days), Plural(days, "day", "days")))
			parts = append(parts, renewalNotes(es)...)
		}
		if behind && comp.Type != "loadbalancer" {
			// Drop the generic arrival line; the upstream entry already said it.
			var kept []string
			for _, p := range parts {
				if !strings.HasPrefix(p, "requests arrive") {
					kept = append(kept, p)
				}
			}
			parts = kept
		}
		return strings.Join(parts, "; ")
	case "firewall":
		if ev := c.eventFor(id, "terraform"); ev != nil {
			return "the firewall changed at " + Clock(ev.At)
		}
		if nextEdge != nil && nextEdge.State == model.Blocked {
			return "the firewall blocks the path, " + strings.TrimPrefix(nextEdge.Label, "blocked at firewall ")
		}
		return ""
	case "workload", "worker":
		switch es.State {
		case model.Failing:
			// Failing on a failing node: blame the node in the same sentence.
			for _, n := range comp.RunsOn {
				ns := c.snap.Components[n]
				if ns.State == model.Failing {
					reason := "the node is failing"
					if _, ok := model.HasCondition(ns.Conditions, model.CondMemoryPressure); ok {
						reason = "the node is out of memory"
					}
					return fmt.Sprintf("%s are failing on %s because %s", Lower(pluralLabel(label)), c.t.LabelOf(n), reason)
				}
			}
			return label + " is " + es.Label
		case model.Processing:
			task := ""
			if cnd, ok := model.HasCondition(es.Conditions, model.CondTaskRunning); ok {
				task = cnd.Detail
			}
			if task == "" {
				task = strings.TrimPrefix(es.Label, "processing ")
			}
			el := ""
			if r, ok := es.Metrics["running_s"]; ok {
				el = " for " + Dur(secs(r))
			}
			if stuck, _ := es.Detail["stuck"].(bool); stuck {
				s := fmt.Sprintf("one worker is stuck on %s%s", task, el)
				if others := c.siblingsKeepingUp(comp); others != "" {
					s += "; " + others
				}
				return s
			}
			return fmt.Sprintf("%s is processing %s%s", label, task, el)
		case model.Waiting:
			return label + " is " + es.Label
		default:
			if nextEdge != nil && nextEdge.State == model.Waiting {
				return label + " is " + nextEdge.Label
			}
			_ = nextEdgeID
			return ""
		}
	case "node":
		if es.State == model.Failing {
			if rb, ok := model.HasCondition(es.Conditions, model.CondRebooted); ok {
				return fmt.Sprintf("%s rebooted at %s and did not rejoin the cluster", label, Clock(rb.Since))
			}
			if _, ok := model.HasCondition(es.Conditions, model.CondMemoryPressure); ok {
				return fmt.Sprintf("%s is out of memory, %s", label, strings.TrimPrefix(es.Label, "failing, "))
			}
			return label + " is " + es.Label
		}
		return ""
	case "queue":
		if es.State == model.Waiting {
			if g, ok := es.Metrics["growth_per_min"]; ok && g > 0 {
				return "jobs are queued and growing, " + strings.TrimPrefix(es.Label, "waiting, ")
			}
			name := Lower(label)
			if !strings.HasSuffix(name, "queue") {
				name += " queue"
			}
			return fmt.Sprintf("the %s is %s", name, es.Label)
		}
		return ""
	case "database":
		if comp.Parent != "" {
			for _, pid := range path {
				if pid == comp.Parent && c.snap.Components[pid].Severity >= es.Severity {
					return ""
				}
			}
		}
		cpu, hasCPU := es.Metrics["cpu_pct"]
		used, ok1 := es.Metrics["connections_used"]
		max, ok2 := es.Metrics["connections_max"]
		if es.State == model.Failing {
			return "the database is " + es.Label
		}
		// Waiting on the pool in front of us?
		var inEdges []model.ElementState
		for _, pid := range path {
			if pe, isEdge := c.elem(pid); isEdge {
				if e, ok := c.t.Edge(pid); ok && e.To == id {
					inEdges = append(inEdges, pe)
				}
			}
		}
		for _, pe := range inEdges {
			if pe.State == model.Waiting && ok1 && ok2 {
				s := fmt.Sprintf("database is at %s of %s connections", Num(used), Num(max))
				if hasCPU {
					s += fmt.Sprintf(", CPU %s", Pct(cpu))
					if cpu < 60 {
						s += ", so the pool is the limit, not the database"
					}
				}
				return s
			}
			if strings.Contains(pe.Label, "double normal") && hasCPU {
				return fmt.Sprintf("the database is taking double its normal load, CPU %s", Pct(cpu))
			}
		}
		if es.Severity >= model.Warn {
			var bits []string
			for _, g := range es.Gauges {
				if g.Level == "red" || g.Level == "amber" {
					bits = append(bits, g.Name+" at "+g.Value)
				}
				if g.Detail != "" {
					bits = append(bits, g.Detail)
				}
			}
			if len(bits) > 0 {
				return "the database has " + strings.Join(bits, ", ")
			}
		}
		if comp.Parent != "" && es.State == model.Failing {
			return label + " is " + es.Label
		}
		return ""
	case "cache":
		if es.State == model.Failing {
			if hit, ok := es.Metrics["hit_rate"]; ok {
				return fmt.Sprintf("the cache is full and missing %s", Pct(100-hit))
			}
			return "the cache is " + es.Label
		}
		return ""
	case "observability":
		if es.State == model.Failing {
			return "observability stopped receiving data; traffic rates on this diagram are unknown since then"
		}
		return ""
	case "sync":
		if es.State == model.Failing || es.State == model.Waiting {
			return fmt.Sprintf("%s is %s, so clients stop receiving changes", label, es.Label)
		}
		return ""
	case "external":
		if es.State == model.Failing {
			// Find a failing job upstream on the path.
			for _, pid := range path {
				pc, ok := c.t.Component(pid)
				if ok && pc.Type == "job" && c.snap.Components[pid].State == model.Failing {
					s := fmt.Sprintf("the %s job failed because %s is not responding; nothing inside the system is at fault", Lower(pc.DisplayLabel()), label)
					if sched := detailString(c.snap.Components[pid].Detail, "schedule"); sched != "" {
						s += "; retries " + sched
					}
					return s
				}
			}
			return fmt.Sprintf("%s is not responding, %s; nothing inside the system is at fault", label, strings.TrimPrefix(es.Label, "failing, "))
		}
		return ""
	case "job":
		if es.State == model.Failing {
			// If the cause is external the external sentence covers it.
			for _, pid := range path {
				if pc, ok := c.t.Component(pid); ok && pc.Type == "external" && c.snap.Components[pid].State == model.Failing {
					return ""
				}
			}
			return fmt.Sprintf("the %s job is %s", Lower(label), es.Label)
		}
		return ""
	}
	if es.Severity >= model.Warn {
		return label + " is " + es.Label
	}
	return ""
}

func detailString(d map[string]any, key string) string {
	if d == nil {
		return ""
	}
	if s, ok := d[key].(string); ok {
		return s
	}
	return ""
}

func renewalNotes(es model.ElementState) []string {
	var out []string
	if cnd, ok := model.HasCondition(es.Conditions, model.CondCertRenewalFailed); ok {
		s := "renewal has failed"
		if !cnd.Since.IsZero() {
			s += " since " + Day(cnd.Since)
		}
		if cnd.Detail != "" {
			s += ", " + cnd.Detail
		}
		out = append(out, s)
	}
	return out
}

func pluralLabel(s string) string {
	if strings.HasSuffix(s, "s") {
		return s
	}
	return s + "s"
}

func secs(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// nodeCount returns how many nodes the lb feeds, from its edges or from the
// runs_on of the workloads behind it.
func (c *ctx) nodeCount(lbID string) int {
	seen := map[string]bool{}
	for _, e := range c.t.Outgoing(lbID) {
		to, ok := c.t.Component(e.To)
		if !ok {
			continue
		}
		if to.Type == "node" {
			seen[to.ID] = true
		}
		for _, n := range to.RunsOn {
			seen[n] = true
		}
	}
	return len(seen)
}

// siblingsKeepingUp describes other workers on the same queue.
func (c *ctx) siblingsKeepingUp(comp model.Component) string {
	for _, in := range c.t.Incoming(comp.ID) {
		q, ok := c.t.Component(in.From)
		if !ok || q.Type != "queue" {
			continue
		}
		total, fine := 0, 0
		for _, out := range c.t.Outgoing(q.ID) {
			if out.To == comp.ID {
				continue
			}
			total++
			s := c.snap.Components[out.To].State
			if s == model.Flowing || s == model.Idle || s == model.Processing {
				fine++
			}
		}
		if total > 0 && fine == total {
			return "the others are keeping up"
		}
		if total > 0 {
			return fmt.Sprintf("%d of %d other workers are also in trouble", total-fine, total)
		}
	}
	return ""
}

// behindEntry reports whether another entry-typed component feeds this one.
func (c *ctx) behindEntry(id string) bool {
	for _, e := range c.t.Incoming(id) {
		if from, ok := c.t.Component(e.From); ok && model.IsEntry(from) {
			return true
		}
	}
	return false
}

func (c *ctx) eventFor(id, kind string) *model.Event {
	var best *model.Event
	for i := range c.in.Events {
		e := &c.in.Events[i]
		if e.Target == id && e.Kind == kind && (best == nil || e.At.After(best.At)) {
			best = e
		}
	}
	return best
}

func (c *ctx) edgeSentence(id string, es model.ElementState, path []string, i int) string {
	e, _ := c.t.Edge(id)
	from := c.t.LabelOf(e.From)
	to := c.t.LabelOf(e.To)
	fromComp, _ := c.t.Component(e.From)
	switch es.State {
	case model.Waiting:
		// Spoken by the source component's sentence.
		if fromComp.Type == "workload" {
			return ""
		}
		return fmt.Sprintf("%s is %s", from, es.Label)
	case model.Failing:
		if cnd, ok := model.HasCondition(es.Conditions, model.CondReplicationBroken); ok {
			s := fmt.Sprintf("%s has not been streaming", to)
			if toComp, ok := c.t.Component(e.To); ok && toComp.Type == "sync" {
				slot := "the replication slot"
				if cnd.Ref != "" {
					slot = "the replication slot " + strings.TrimPrefix(cnd.Ref, "slot/")
				}
				s = fmt.Sprintf("%s of %s is inactive", slot, to)
			}
			if !cnd.Since.IsZero() {
				s += " for " + Ago(c.in.Now, cnd.Since)
			}
			if wal, ok := es.Metrics["wal_retained_bytes"]; ok && wal > 0 {
				s += fmt.Sprintf(", %s of WAL is retained on %s", Bytes(wal), Lower(from))
			}
			if src := c.snap.Components[e.From]; true {
				for _, g := range src.Gauges {
					if g.Name == "disk" && g.Detail != "" {
						s += fmt.Sprintf("; %s disk is at %s, %s", Lower(from), g.Value, g.Detail)
					}
				}
			}
			return s
		}
		if v, ok := es.Metrics["error_rate"]; ok && v > 0 {
			return fmt.Sprintf("%s of requests from %s to %s fail", Pct(v), Lower(from), to)
		}
		return fmt.Sprintf("traffic from %s to %s is %s", Lower(from), to, es.Label)
	case model.Blocked:
		if _, ok := model.HasCondition(es.Conditions, model.CondHealthCheckFailing); ok {
			return ""
		}
		if fromComp.Type == "firewall" {
			return ""
		}
		return fmt.Sprintf("the path from %s to %s is %s", Lower(from), to, es.Label)
	}
	return ""
}
