// Package state is the six-state engine: it turns observations into exactly
// one state per component and edge, with a plain-language label, a severity
// and the lit paths of the issue lens. It is pure Go with no terminal
// dependency so it can be tested from recorded fixtures.
package state

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/bind"
	"github.com/danilopopovikj/wassup/internal/model"
)

// Input is everything one evaluation needs.
type Input struct {
	Topology   *model.Topology
	Thresholds model.Thresholds
	Joined     map[string]*bind.Joined
	Now        time.Time
	Tick       int64
	TickEvery  time.Duration
	Prev       *model.Snapshot
	// Events are recent change markers (within the lookback window).
	Events []model.Event
	// Trends holds metric slopes in percent per hour per element from history.
	Trends map[string]map[string]float64
	// Baselines holds the usual rate per edge (same time yesterday).
	Baselines map[string]float64
	// ProbeHealth is copied into the snapshot.
	ProbeHealth map[string]string
	Replay      bool
}

type ctx struct {
	in     Input
	t      *model.Topology
	snap   *model.Snapshot
	noData bool // the traffic source is down, so idle edges say "no data"
	// nodeNames maps the name a provider knows a machine by to its id.
	nodeNames map[string]string
}

// Evaluate produces a snapshot from the input.
func Evaluate(in Input) *model.Snapshot {
	if in.TickEvery <= 0 {
		in.TickEvery = 5 * time.Second
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	c := &ctx{in: in, t: in.Topology, snap: &model.Snapshot{
		GeneratedAt: in.Now, Tick: in.Tick, Name: in.Topology.Name, Replay: in.Replay,
		Components: map[string]model.ElementState{}, Edges: map[string]model.ElementState{},
		ProbeHealth: in.ProbeHealth,
	}}

	comps := c.t.AllComponents()
	// Pass 1: components, everything but the flowing/idle decision.
	pending := map[string]bool{}
	for _, comp := range comps {
		es, decided := c.evalComponent(comp)
		if !decided {
			pending[comp.ID] = true
		}
		c.snap.Components[comp.ID] = es
	}
	// Observability down?
	for _, comp := range comps {
		if comp.Type == "observability" {
			es := c.snap.Components[comp.ID]
			if _, ok := model.HasCondition(es.Conditions, model.CondNoData); ok || es.State == model.Failing {
				c.noData = true
			}
		}
	}
	// Pass 2: edges.
	for _, e := range c.t.Edges {
		c.snap.Edges[e.ID()] = c.evalEdge(e)
	}
	// Pass 3: finish components: flowing when they report a rate of their
	// own or an incident edge flows, else idle.
	finish := func(machines bool) {
		for _, comp := range comps {
			if pending[comp.ID] && (comp.Type == "node") == machines {
				c.snap.Components[comp.ID] = c.finish(comp, c.snap.Components[comp.ID])
			}
		}
	}
	finish(false)
	// Roles: a db with roles inherits from its primary and notes replica trouble.
	for _, comp := range c.t.Components {
		if comp.Type == "database" && comp.Roles != nil && comp.Roles.Primary != "" {
			c.rollupRoles(comp)
		}
	}
	// Machines come last, after the databases took the state of their
	// primary: a machine is busy when what runs on it is.
	finish(true)
	// Severity, gauges, notes, since.
	for id, es := range c.snap.Components {
		comp, _ := c.t.Component(id)
		es.Gauges = c.gauges(comp, es)
		es.Severity = c.componentSeverity(comp, &es)
		es.Notes = append(es.Notes, c.markerNotes(id)...)
		es.Since = c.since(id, es.State, c.prevComponent(id))
		if es.Detail == nil {
			es.Detail = map[string]any{}
		}
		if comp.Type == "node" {
			es.Hosted = c.hostedOn(id)
		}
		c.snap.Components[id] = es
	}
	for id, es := range c.snap.Edges {
		es.Severity = model.SeverityOf(es.State)
		if es.Marker == model.MarkerUnbound || es.Marker == model.MarkerNoData {
			es.Severity = model.Info
		}
		es.Since = c.since(id, es.State, c.prevEdge(id))
		c.snap.Edges[id] = es
	}
	c.snap.Issues = c.issues()
	return c.snap
}

func (c *ctx) prevComponent(id string) *model.ElementState {
	if c.in.Prev == nil {
		return nil
	}
	if p, ok := c.in.Prev.Components[id]; ok {
		return &p
	}
	return nil
}

func (c *ctx) prevEdge(id string) *model.ElementState {
	if c.in.Prev == nil {
		return nil
	}
	if p, ok := c.in.Prev.Edges[id]; ok {
		return &p
	}
	return nil
}

func (c *ctx) since(id string, s model.State, prev *model.ElementState) time.Time {
	if prev != nil && prev.State == s && !prev.Since.IsZero() {
		return prev.Since
	}
	return c.in.Now
}

func (c *ctx) th(id string) model.ThresholdSet { return c.in.Thresholds.For(id) }

func (c *ctx) joined(id string) *bind.Joined {
	if c.in.Joined == nil {
		return nil
	}
	return c.in.Joined[id]
}

func (c *ctx) isStale(j *bind.Joined, th model.ThresholdSet) bool {
	if j == nil || j.LastAt.IsZero() {
		return false
	}
	return c.in.Now.Sub(j.LastAt) > time.Duration(th.StaleTicks)*c.in.TickEvery
}

func metric(j *bind.Joined, key string) (float64, bool) {
	if j == nil {
		return 0, false
	}
	v, ok := j.Metrics[key]
	return v, ok
}

func metricOr(j *bind.Joined, key string, def float64) float64 {
	if v, ok := metric(j, key); ok {
		return v
	}
	return def
}

func detailStr(j *bind.Joined, key string) string {
	if j == nil {
		return ""
	}
	if v, ok := j.Detail[key].(string); ok {
		return v
	}
	return ""
}

// evalComponent applies rules 1 to 5. It returns decided=false when the
// component is healthy and the flowing/idle decision waits for the edges.
func (c *ctx) evalComponent(comp model.Component) (model.ElementState, bool) {
	j := c.joined(comp.ID)
	th := c.th(comp.ID)
	es := model.ElementState{Metrics: map[string]float64{}, Detail: map[string]any{}}
	if j != nil {
		for k, v := range j.Metrics {
			es.Metrics[k] = v
		}
		es.Conditions = append(es.Conditions, j.Conditions...)
		for k, v := range j.Detail {
			es.Detail[k] = v
		}
		es.LastData = j.LastAt
	}
	if j != nil && len(j.Errors) > 0 {
		es.Detail["probe_errors"] = append([]string(nil), j.Errors...)
	}
	// Rule 1: unbound / stale. A probe that failed, timed out or may not
	// read its source delivered nothing: the component is drawn unbound and
	// says "no data" with the reason. Nothing is concluded from the failure.
	if j == nil || !j.Bound {
		es.State = model.Idle
		es.Marker = model.MarkerUnbound
		es.Label = "unbound, no probe data"
		if j != nil && len(j.Errors) > 0 {
			es.Label = "no data, " + shortErr(j.Errors[0])
		}
		return es, true
	}
	if c.isStale(j, th) {
		es.Marker = model.MarkerStale
		if p := c.prevComponent(comp.ID); p != nil {
			es.State = p.State
			es.Label = p.Label
		} else {
			es.State = model.Idle
			es.Label = "idle"
		}
		return es, true
	}
	// Rule 2: failing.
	if reason := c.failingReason(comp, j, th); reason != "" {
		es.State = model.Failing
		es.Label = "failing, " + reason
		return es, true
	}
	// Rule 3: blocked.
	if reason := c.blockedReason(comp, j); reason != "" {
		es.State = model.Blocked
		es.Label = reason
		return es, true
	}
	// Rule 4: waiting.
	if reason := c.waitingReason(comp, j, th); reason != "" {
		es.State = model.Waiting
		es.Label = "waiting, " + reason
		es.Queued = firstMetric(j, "depth", "queued", "waiters")
		return es, true
	}
	// Rule 5: processing.
	if reason := c.processingReason(comp, j); reason != "" {
		es.State = model.Processing
		es.Label = "processing " + reason
		return es, true
	}
	return es, false
}

// shortErr makes a probe error fit a label: without the probe's name, and
// when it is long, the last part, which is where a wrapped error says what
// happened ("connection refused"). The whole error stays in the detail.
func shortErr(s string) string {
	if i := strings.Index(s, ": "); i > 0 {
		s = s[i+2:]
	}
	if len(s) > 60 {
		if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
			s = s[i+2:]
		}
	}
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func firstMetric(j *bind.Joined, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := metric(j, k); ok {
			return v
		}
	}
	return 0
}

// failingReason implements rule 2 and builds the phrase after "failing, ".
func (c *ctx) failingReason(comp model.Component, j *bind.Joined, th model.ThresholdSet) string {
	now := c.in.Now
	conds := j.Conditions
	has := func(k string) (model.Condition, bool) { return model.HasCondition(conds, k) }

	switch comp.Type {
	case "workload", "backgroundworker":
		if cnd, ok := has(model.CondCrashLoopBackOff); ok {
			return c.failingReplicaPhrase(j) + c.restartPhrase(j, cnd)
		}
		if _, ok := has(model.CondOOMKilled); ok {
			killed := metricOr(j, "killed", 0)
			s := fmt.Sprintf("%s %s killed, out of memory", Num(killed), Plural(killed, "pod", "pods"))
			if ev := metricOr(j, "evicted", 0); ev > 0 {
				s += fmt.Sprintf(", %s evicted", Num(ev))
			}
			if killed == 0 {
				s = "pods killed, out of memory"
			}
			return s
		}
		if _, ok := has(model.CondImagePullBackOff); ok {
			return c.replicaPhrase(j) + ", image cannot be pulled"
		}
		if ready, ok := metric(j, "replicas_ready"); ok {
			if desired := metricOr(j, "replicas_desired", 0); desired > 0 && ready == 0 {
				return fmt.Sprintf("0 of %s replicas ready", Num(desired))
			}
		}
	case "node":
		if cnd, ok := has(model.CondNotReady); ok {
			s := "not ready"
			if rb, ok := has(model.CondRebooted); ok {
				s += ", rebooted " + Clock(rb.Since)
			} else if !cnd.Since.IsZero() {
				s += " since " + Clock(cnd.Since)
			}
			return s
		}
		if _, ok := has(model.CondMemoryPressure); ok {
			s := fmt.Sprintf("memory at %s", Pct(metricOr(j, "mem_pct", 0)))
			if k := metricOr(j, "killed", 0); k > 0 {
				s += fmt.Sprintf(", %s %s killed", Num(k), Plural(k, "pod", "pods"))
			}
			return s
		}
		if _, ok := has(model.CondDiskPressure); ok {
			return fmt.Sprintf("disk at %s, under pressure", Pct(metricOr(j, "disk_pct", 0)))
		}
	case "ingress", "dns":
		if cnd, ok := has(model.CondCertExpired); ok {
			s := "certificate expired"
			if !cnd.Since.IsZero() {
				s += " " + Ago(now, cnd.Since) + " ago"
			}
			return s
		}
	case "observability":
		if cnd, ok := has(model.CondNoData); ok {
			s := "no data received"
			if !cnd.Since.IsZero() {
				s += " for " + Ago(now, cnd.Since)
			}
			return s
		}
		if v, ok := metric(j, "ingest_rate"); ok && v == 0 {
			return "no data received"
		}
	case "external":
		if cnd, ok := has(model.CondTimeout); ok {
			s := "timing out"
			if !cnd.Since.IsZero() {
				s += " for " + Ago(now, cnd.Since)
			}
			return s
		}
		if v, ok := metric(j, "timeout_rate"); ok && v > th.TimeoutRatePct {
			return fmt.Sprintf("%s timeouts", Pct(v))
		}
		if v, ok := metric(j, "error_rate"); ok && v > th.ErrorRatePct {
			return fmt.Sprintf("%s errors", Pct(v))
		}
	case "scheduledjob":
		if cnd, ok := has(model.CondJobFailed); ok {
			s := "last run failed"
			if !cnd.Since.IsZero() {
				s += " " + Clock(cnd.Since)
			}
			if cnd.Detail != "" {
				s += ", " + cnd.Detail
			}
			return s
		}
	case "cache":
		mem := metricOr(j, "mem_pct", 0)
		hit, hasHit := metric(j, "hit_rate")
		if _, ok := has(model.CondCacheFull); ok || (mem >= 99.5 && hasHit && hit < th.HitRateMinPct) {
			s := "memory full"
			if hasHit {
				s += fmt.Sprintf(", %s misses", Pct(100-hit))
			}
			return s
		}
		if hasHit && hit < th.HitRateMinPct && mem < 99.5 {
			return fmt.Sprintf("%s misses", Pct(100-hit))
		}
	case "database":
		if cnd, ok := has(model.CondReplicationBroken); ok {
			return replicationPhrase(now, j, cnd, false)
		}
	case "syncengine":
		if cnd, ok := has(model.CondReplicationBroken); ok {
			return replicationPhrase(now, j, cnd, true)
		}
		if cnd, ok := has(model.CondNotReady); ok {
			s := "not ready"
			if cnd.Detail != "" {
				s += ", " + cnd.Detail
			}
			return s
		}
		if cnd, ok := has(model.CondConnectionRefused); ok {
			s := "not reachable"
			if cnd.Detail != "" {
				s += ", " + cnd.Detail
			}
			return s
		}
	case "loadbalancer":
		if _, ok := has(model.CondTargetUnhealthy); ok {
			if h, ok := metric(j, "targets_healthy"); ok && h == 0 {
				return "no healthy targets"
			}
		}
	case "storage":
		if cnd, ok := has(model.CondNotReady); ok {
			s := "not ready"
			if cnd.Detail != "" {
				s += ", " + cnd.Detail
			}
			return s
		}
		if _, ok := has(model.CondConnectionRefused); ok {
			return "not reachable"
		}
	}
	// Type-independent failing rules.
	if _, ok := has(model.CondTargetUnhealthy); ok && comp.Type == "node" {
		return "health check failing at the load balancer"
	}
	if disk := metricOr(j, "disk_pct", 0); disk >= th.DiskFailingPct {
		return fmt.Sprintf("disk at %s", Pct(disk))
	}
	if _, ok := has(model.CondDiskFull); ok {
		return "disk full"
	}
	if _, ok := has(model.CondCrashLoopBackOff); ok {
		return "crashing" + c.restartPhrase(j, model.Condition{})
	}
	if _, ok := has(model.CondNotReady); ok {
		return "not ready"
	}
	if _, ok := has(model.CondCertExpired); ok {
		return "certificate expired"
	}
	return ""
}

// failingReplicaPhrase says how many replicas are in trouble: "1 of 3 replicas".
func (c *ctx) failingReplicaPhrase(j *bind.Joined) string {
	ready, ok1 := metric(j, "replicas_ready")
	desired, ok2 := metric(j, "replicas_desired")
	if ok1 && ok2 && desired > 0 && desired >= ready {
		bad := desired - ready
		if bad == 0 {
			bad = 1
		}
		return fmt.Sprintf("%s of %s replicas", Num(bad), Num(desired))
	}
	return "replicas"
}

func (c *ctx) replicaPhrase(j *bind.Joined) string {
	ready, ok1 := metric(j, "replicas_ready")
	desired, ok2 := metric(j, "replicas_desired")
	if ok1 && ok2 && desired > 0 {
		return fmt.Sprintf("%s of %s replicas", Num(ready), Num(desired))
	}
	return "replicas"
}

func (c *ctx) restartPhrase(j *bind.Joined, cnd model.Condition) string {
	r, ok := metric(j, "restarts")
	if !ok || r == 0 {
		return " restarting"
	}
	win := metricOr(j, "restart_window_s", 300)
	return fmt.Sprintf(", %s %s in %s", Num(r), Plural(r, "restart", "restarts"), Dur(time.Duration(win*float64(time.Second))))
}

// replicationPhrase reads "replica not streaming for 3 h, 40 GB WAL retained",
// or, for a sync engine, "sync slot inactive for 2 h, 12 GB WAL retained".
func replicationPhrase(now time.Time, j *bind.Joined, cnd model.Condition, sync bool) string {
	s := "replica not streaming"
	if sync {
		s = "sync slot inactive"
	}
	if !cnd.Since.IsZero() {
		s += " for " + Ago(now, cnd.Since)
	}
	if wal, ok := metric(j, "wal_retained_bytes"); ok && wal > 0 {
		s += ", " + Bytes(wal) + " WAL retained"
	} else if lag, ok := metric(j, "lag_bytes"); ok && lag > 0 {
		s += ", " + Bytes(lag) + " behind"
	}
	return s
}

// blockedReason implements rule 3.
func (c *ctx) blockedReason(comp model.Component, j *bind.Joined) string {
	if cnd, ok := model.HasCondition(j.Conditions, model.CondFirewallDenied); ok {
		return firewallPhrase(cnd)
	}
	if cnd, ok := model.HasCondition(j.Conditions, model.CondConnectionRefused); ok {
		s := "blocked, connection refused"
		if cnd.Detail != "" {
			s += " at " + cnd.Detail
		}
		return s
	}
	return ""
}

func firewallPhrase(cnd model.Condition) string {
	s := "blocked at firewall"
	if !cnd.Since.IsZero() {
		s += " since " + Clock(cnd.Since)
	}
	if cnd.Detail != "" {
		if strings.Contains(s, "since") {
			s += ", rule " + cnd.Detail
		} else {
			s += " rule " + cnd.Detail
		}
	}
	return s
}

// waitingReason implements rule 4 and returns the phrase after "waiting, ".
func (c *ctx) waitingReason(comp model.Component, j *bind.Joined, th model.ThresholdSet) string {
	if depth, ok := metric(j, "depth"); ok && depth > th.QueueDepth {
		s := fmt.Sprintf("%s queued", Num(depth))
		if oldest, ok := metric(j, "oldest_age_s"); ok && oldest > 0 {
			s += ", oldest " + Dur(time.Duration(oldest*float64(time.Second)))
		} else if consumer := c.consumerOf(comp.ID); consumer != "" {
			s += ", at " + consumer
		}
		return s
	}
	used, ok1 := metric(j, "pool_used")
	max, ok2 := metric(j, "pool_max")
	_, exhausted := model.HasCondition(j.Conditions, model.CondPoolExhausted)
	if (ok1 && ok2 && max > 0 && used >= max) || exhausted {
		if w := metricOr(j, "waiters", 0); w > 0 || exhausted {
			if comp.Type == "workload" || comp.Type == "backgroundworker" {
				// a worker pool: every slot busy, work piling up behind it
				return fmt.Sprintf("all %s slots busy, %s queued", Num(max), Num(w))
			}
			return fmt.Sprintf("%s queued, at the pool", Num(w))
		}
	}
	if lag, ok := metric(j, "lag_bytes"); ok && lag > th.LagBytes {
		return Bytes(lag) + " behind the primary"
	}
	return ""
}

// consumerOf returns the lowercase label of the workload consuming a queue.
func (c *ctx) consumerOf(queueID string) string {
	for _, e := range c.t.Outgoing(queueID) {
		if to, ok := c.t.Component(e.To); ok {
			return Lower(to.DisplayLabel())
		}
	}
	return ""
}

// processingReason implements rule 5 and returns "task, 18 min".
func (c *ctx) processingReason(comp model.Component, j *bind.Joined) string {
	for _, k := range []string{model.CondTaskRunning, model.CondJobRunning, model.CondMigration, model.CondBackup, model.CondVacuum} {
		if cnd, ok := model.HasCondition(j.Conditions, k); ok {
			name := cnd.Detail
			if name == "" {
				name = strings.ToLower(k)
				if k == model.CondJobRunning {
					name = "run"
				}
			}
			el := ""
			if !cnd.Since.IsZero() {
				el = ", " + Ago(c.in.Now, cnd.Since)
			} else if r, ok := metric(j, "running_s"); ok {
				el = ", " + Dur(time.Duration(r*float64(time.Second)))
			}
			return name + el
		}
	}
	if comp.Type == "scheduledjob" {
		if a := metricOr(j, "active", 0); a > 0 {
			name := detailStr(j, "task")
			if name == "" {
				name = "run"
			}
			el := ""
			if r, ok := metric(j, "running_s"); ok {
				el = ", " + Dur(time.Duration(r*float64(time.Second)))
			}
			return name + el
		}
	}
	if r, ok := metric(j, "running_s"); ok && r > 0 {
		if task := detailStr(j, "task"); task != "" {
			return task + ", " + Dur(time.Duration(r*float64(time.Second)))
		}
	}
	return ""
}

// NoRate is the label of an element that is bound and in order while
// nothing reports what goes through it: whether it is idle is then not
// known, and the label does not claim it.
const NoRate = "no rate measured"

// measured reports whether the metrics of an element say how much goes
// through it: a rate, or one of the measures of work its type names.
func measured(m map[string]float64, work []string) bool {
	if _, ok := m["rate"]; ok {
		return true
	}
	for _, k := range work {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// edgeWork is what says how much goes through an edge besides a rate: a
// replication edge is measured by how far its consumer is behind.
var edgeWork = []string{"lag_bytes"}

// finish decides between flowing and idle for a component that is in
// order. Its own rate is what its label says, in the unit of its type; what
// it is sent stands in where it reports none, and what it sends where
// nothing is sent to it. An idle component says when it last saw work,
// where a probe knows; one that nothing measures says so and looks it.
func (c *ctx) finish(comp model.Component, es model.ElementState) model.ElementState {
	if comp.Type == "scheduledjob" {
		return c.finishJob(comp, es)
	}
	rate, unit, flowing := c.incidentFlow(comp.ID)
	if own, ok := es.Metrics["rate"]; ok && own > 0 {
		rate, unit, flowing = own, model.RateUnitOf(comp.Type), true
	}
	known := measured(es.Metrics, model.Catalog[comp.Type].Work) || c.incidentMeasured(comp.ID)
	if comp.Type == "node" {
		busy, hostedKnown := c.busyOn(comp.ID)
		flowing = flowing || busy
		known = known || hostedKnown
	}
	switch {
	case flowing:
		es.State, es.Rate, es.Unit = model.Flowing, rate, unit
		es.Label = "flowing"
		if rate > 0 {
			es.Label = "flowing, " + Rate(rate, unit)
		}
	case known:
		es.State, es.Label = model.Idle, "idle"
		if last, what := c.lastWork(comp, es); !last.IsZero() {
			es.Label += ", " + what + " " + Ago(c.in.Now, last) + " ago"
		}
	case comp.Type == "node":
		// a machine that is up and runs nothing anybody counts
		es.State, es.Label, es.Marker = model.Idle, "up", model.MarkerUnmetered
	default:
		es.State, es.Label, es.Marker = model.Idle, NoRate, model.MarkerUnmetered
	}
	return es
}

// finishJob decides for a scheduled job that runs nothing now: it is idle
// between its runs, however often it runs, and says when it last ran, or
// that it did not run in the window its counts cover. A running job was
// decided before, by the processing rule.
func (c *ctx) finishJob(comp model.Component, es model.ElementState) model.ElementState {
	last, _ := c.lastWork(comp, es)
	switch {
	case !last.IsZero():
		es.State, es.Label = model.Idle, "idle, ran "+Ago(c.in.Now, last)+" ago"
	case measured(es.Metrics, []string{"succeeded", "failed"}) && countWindow(es) > 0:
		// counted over a known window and none there: say how long
		es.State, es.Label = model.Idle, "idle, no run in "+Dur(countWindow(es))
	case measured(es.Metrics, model.Catalog[comp.Type].Work):
		es.State, es.Label = model.Idle, "idle"
	default:
		es.State, es.Label, es.Marker = model.Idle, NoRate, model.MarkerUnmetered
	}
	return es
}

// countWindow is the window a job's counts cover, when its probe says.
func countWindow(es model.ElementState) time.Duration {
	if w, ok := es.Detail["window"].(string); ok {
		if d, err := time.ParseDuration(w); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

// lastWork is when a component last saw work, and how to say it: when a
// scheduled job last ran, else the latest call a probe of the component or
// of an edge that ends at it saw.
func (c *ctx) lastWork(comp model.Component, es model.ElementState) (time.Time, string) {
	if comp.Type == "scheduledjob" {
		var last time.Time
		for _, k := range []string{"last_run", "last_success", "last_failure"} {
			if t := timeOf(es.Detail[k]); t.After(last) {
				last = t
			}
		}
		return last, "ran"
	}
	last, what := timeOf(es.Detail["last_seen"]), "last call"
	for _, e := range c.t.Edges {
		if e.To != comp.ID {
			continue
		}
		if t := timeOf(c.snap.Edges[e.ID()].Detail["last_seen"]); t.After(last) {
			last, what = t, lastNoun(e.Kind)
		}
	}
	return last, what
}

// lastNoun names the latest unit of work of an edge kind.
func lastNoun(kind string) string {
	switch kind {
	case "sql":
		return "last query"
	case "queue":
		return "last job"
	}
	return "last call"
}

// timeOf reads a time a probe left in a detail: a time, or RFC 3339 text.
func timeOf(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		if at, err := time.Parse(time.RFC3339, t); err == nil {
			return at
		}
	}
	return time.Time{}
}

// incidentMeasured reports whether a rate was read for one of the edges
// that touch a component.
func (c *ctx) incidentMeasured(id string) bool {
	for _, e := range c.t.Edges {
		if (e.From == id || e.To == id) && measured(c.snap.Edges[e.ID()].Metrics, edgeWork) {
			return true
		}
	}
	return false
}

// busyOn reports whether work goes through something that runs on a
// machine, and whether that is known for everything that runs on it. A
// machine that holds nothing knows nothing.
func (c *ctx) busyOn(nodeID string) (busy, known bool) {
	hosted := c.hostedOn(nodeID)
	known = len(hosted) > 0
	for _, h := range hosted {
		if h.Known && h.Pods == 0 {
			continue // none of it is here
		}
		switch h.State {
		case model.Flowing, model.Waiting, model.Processing:
			busy = true
		}
		// Unbound, without data, or in order with nothing counting: what goes
		// through it is not known, and so not what goes through the machine.
		if h.Marker == model.MarkerUnbound || h.Marker == model.MarkerNoData || c.snap.Components[h.ID].Marker == model.MarkerUnmetered {
			known = false
		}
	}
	return busy, known
}

// incidentFlow reports whether any incident edge flows, and the rate a
// component's label says: what is sent to it, added up in one unit, or,
// when nothing sent to it is counted, the busiest of what it sends. An API
// that serves 6 req/s and runs 50 queries/s serves 6 req/s; the queries are
// the database's work, and its edge says them.
func (c *ctx) incidentFlow(id string) (float64, string, bool) {
	in := map[string]float64{}
	var out float64
	var outUnit string
	flowing := false
	for _, e := range c.t.Edges {
		if e.From != id && e.To != id {
			continue
		}
		es := c.snap.Edges[e.ID()]
		if es.State != model.Flowing {
			continue
		}
		flowing = true
		if e.Kind == "replication" || e.Kind == "tcp" || es.Rate <= 0 {
			continue // lag and health-check connections are not a rate worth a label
		}
		if e.To == id {
			in[es.Unit] += es.Rate
		} else if es.Rate > out {
			out, outUnit = es.Rate, es.Unit
		}
	}
	best, unit := 0.0, ""
	for u, r := range in {
		if r > best || (r == best && u < unit) {
			best, unit = r, u
		}
	}
	if best > 0 {
		return best, unit, flowing
	}
	return out, outUnit, flowing
}

// evalEdge applies the edge rules.
func (c *ctx) evalEdge(e model.Edge) model.ElementState {
	id := e.ID()
	j := c.joined(id)
	th := c.th(id)
	es := model.ElementState{Metrics: map[string]float64{}, Unit: model.RateUnit(e.Kind), Detail: map[string]any{}}
	if j != nil && j.Bound {
		for k, v := range j.Metrics {
			es.Metrics[k] = v
		}
		es.Conditions = append(es.Conditions, j.Conditions...)
		for k, v := range j.Detail {
			es.Detail[k] = v
		}
		es.LastData = j.LastAt
		if u, ok := j.Detail["unit"].(string); ok && u != "" && e.Kind != "replication" {
			es.Unit = u // a probe that knows what it counts says it: queries, not transactions
		}
	}
	src := c.snap.Components[e.From]
	dst := c.snap.Components[e.To]
	dstComp, _ := c.t.Component(e.To)
	dstLabel := "the " + Lower(dstComp.DisplayLabel())
	if dstComp.Type == "external" || dstComp.Type == "node" || dstComp.Type == "workload" || dstComp.Type == "backgroundworker" {
		dstLabel = Lower(dstComp.DisplayLabel())
	}

	// Rule 1: no data of its own, or unbound ends. An edge whose probes all
	// failed says so: it must not read idle, and it must not take the flow
	// of its ends as if it had been measured.
	own := j != nil && j.Bound && !c.isStale(j, th)
	if j != nil && len(j.Errors) > 0 {
		es.Detail["probe_errors"] = append([]string(nil), j.Errors...)
		if !j.Bound {
			es.State = model.Idle
			es.Marker = model.MarkerNoData
			es.Label = "no data, " + shortErr(j.Errors[0])
			return es
		}
	}
	if (src.Marker == model.MarkerUnbound || dst.Marker == model.MarkerUnbound) && !own {
		es.State = model.Idle
		es.Marker = model.MarkerUnbound
		es.Label = "unbound"
		return es
	}
	if own || len(es.Conditions) > 0 {
		// Rule 2: blocked.
		if cnd, ok := model.HasCondition(es.Conditions, model.CondFirewallDenied); ok {
			es.State = model.Blocked
			es.Label = firewallPhrase(cnd)
			return es
		}
		if cnd, ok := model.HasCondition(es.Conditions, model.CondHealthCheckFailing); ok {
			es.State = model.Blocked
			es.Label = "blocked, health check failing"
			if !cnd.Since.IsZero() {
				es.Label += " since " + Clock(cnd.Since)
			}
			return es
		}
		if cnd, ok := model.HasCondition(es.Conditions, model.CondConnectionRefused); ok {
			es.State = model.Blocked
			es.Label = "blocked, connection refused at " + dstLabel
			if cnd.Detail != "" {
				es.Label = "blocked, connection refused, " + cnd.Detail
			}
			return es
		}
	}
	if fw, ok := model.HasCondition(dst.Conditions, model.CondFirewallDenied); ok && dst.State == model.Blocked {
		es.State = model.Blocked
		es.Label = firewallPhrase(fw)
		return es
	}
	if own || len(es.Conditions) > 0 {
		// Rule 3: failing.
		if cnd, ok := model.HasCondition(es.Conditions, model.CondReplicationBroken); ok {
			es.State = model.Failing
			es.Label = "failing, " + replicationPhrase(c.in.Now, j, cnd, dstComp.Type == "syncengine")
			return es
		}
		if v, ok := es.Metrics["error_rate"]; ok && v > th.ErrorRatePct {
			es.State = model.Failing
			es.ErrorRate = v
			es.Label = fmt.Sprintf("failing, %s errors", Pct(v))
			return es
		}
		if v, ok := es.Metrics["timeout_rate"]; ok && v > th.TimeoutRatePct {
			es.State = model.Failing
			es.Label = fmt.Sprintf("failing, %s timeouts", Pct(v))
			return es
		}
		if _, ok := model.HasCondition(es.Conditions, model.CondTimeout); ok {
			es.State = model.Failing
			es.Label = "failing, timing out"
			return es
		}
		// Rule 4: waiting.
		queued := 0.0
		for _, k := range []string{"queued", "waiters", "pending"} {
			if v, ok := es.Metrics[k]; ok && v > queued {
				queued = v
			}
		}
		if queued > 0 {
			es.State = model.Waiting
			es.Queued = queued
			es.Label = fmt.Sprintf("waiting, %s queued, at %s", Num(queued), dstLabel)
			return es
		}
		// Rule 5: flowing. Replication edges flow when the replica streams;
		// their "rate" is the lag in bytes.
		if e.Kind == "replication" {
			if lag, ok := es.Metrics["lag_bytes"]; ok {
				es.State = model.Flowing
				es.Rate = lag
				es.Label = "flowing, " + Bytes(lag) + " lag"
				return es
			}
		}
		if r, ok := es.Metrics["rate"]; ok && r > 0 {
			es.State = model.Flowing
			es.Rate = r
			es.Label = "flowing, " + Rate(r, es.Unit)
			if e.Kind == "cache" {
				if hit, ok := es.Metrics["hit_rate"]; ok {
					es.Label += fmt.Sprintf(", %s miss", Pct(100-hit))
				} else if hit, ok := dst.Metrics["hit_rate"]; ok {
					es.Label += fmt.Sprintf(", %s miss", Pct(100-hit))
				}
			}
			if base := c.baseline(id, es); base > 0 && r >= th.RateSpikeFactor*base {
				es.Label += ", double normal"
				es.Notes = append(es.Notes, "usual "+Rate(base, es.Unit))
			}
			return es
		}
	}
	// Derived flow: an edge without its own rate takes the destination's, or
	// the source's, when Kubernetes connectivity is all we have.
	if !own {
		if c.noData {
			es.State = model.Idle
			es.Marker = model.MarkerNoData
			es.Label = "no data"
			return es
		}
		if r := c.derivedRate(e, src, dst); r > 0 {
			es.State = model.Flowing
			es.Rate = r
			es.Label = "flowing, " + Rate(r, es.Unit)
			return es
		}
	}
	// Idle is what was measured at nothing. An edge nobody measures does not
	// say idle: that would read as no traffic where the traffic is not known.
	es.State = model.Idle
	if own && measured(es.Metrics, edgeWork) {
		es.Label = "idle"
		if last := timeOf(es.Detail["last_seen"]); !last.IsZero() {
			es.Label += ", " + lastNoun(e.Kind) + " " + Ago(c.in.Now, last) + " ago"
		}
		return es
	}
	es.Label, es.Marker = NoRate, model.MarkerUnmetered
	return es
}

func (c *ctx) baseline(edgeID string, es model.ElementState) float64 {
	if b, ok := es.Metrics["rate_baseline"]; ok && b > 0 {
		return b
	}
	if c.in.Baselines != nil {
		return c.in.Baselines[edgeID]
	}
	return 0
}

// derivedRate uses component-level rate metrics when an edge has none.
func (c *ctx) derivedRate(e model.Edge, src, dst model.ElementState) float64 {
	if r, ok := dst.Metrics["rate"]; ok && r > 0 && len(c.t.Incoming(e.To)) == 1 {
		return r
	}
	if r, ok := src.Metrics["rate"]; ok && r > 0 && len(c.t.Outgoing(e.From)) == 1 {
		return r
	}
	return 0
}

// rollupRoles makes a db with roles inherit its primary's state and note
// replica trouble.
func (c *ctx) rollupRoles(comp model.Component) {
	es := c.snap.Components[comp.ID]
	prim := c.snap.Components[comp.Roles.Primary]
	if es.Marker == model.MarkerUnbound && prim.Marker != model.MarkerUnbound {
		es = prim
		es.Detail = map[string]any{"primary": comp.Roles.Primary}
	}
	for _, r := range comp.Roles.Replicas {
		rs := c.snap.Components[r]
		if rs.State == model.Failing || rs.State == model.Waiting {
			es.Notes = append(es.Notes, r+" "+rs.Label)
		}
	}
	c.snap.Components[comp.ID] = es
}

// gauges builds the gauges of a component from its type.
func (c *ctx) gauges(comp model.Component, es model.ElementState) []model.Gauge {
	spec, ok := model.Catalog[comp.Type]
	if !ok {
		return nil
	}
	th := c.th(comp.ID)
	var out []model.Gauge
	trends := c.in.Trends[comp.ID]
	for _, g := range spec.Gauges {
		gauge := model.Gauge{Name: g.Name, Short: g.Short, Pct: -1, Level: "none"}
		if gauge.Short == "" {
			gauge.Short = g.Name
		}
		switch {
		case g.Used != "" && g.Max != "":
			u, ok1 := es.Metrics[g.Used]
			m, ok2 := es.Metrics[g.Max]
			if ok1 && ok2 {
				gauge.Value = fmt.Sprintf("%s/%s", Num(u), Num(m))
				if m > 0 {
					gauge.Pct = 100 * u / m
					if g.Name == "targets" || g.Name == "ready" || g.Name == "workers" {
						// fewer healthy is worse: invert the level logic
						gauge.Level = "ok"
						if u < m {
							gauge.Level = "amber"
						}
						if u <= m/2 || u == 0 {
							gauge.Level = "red"
						}
					} else {
						gauge.Level = level(gauge.Pct, th)
					}
				}
			} else if ok1 {
				gauge.Value = Num(u)
			} else {
				continue
			}
		case g.RateOnly:
			v, ok := es.Metrics[g.Metric]
			if !ok {
				continue
			}
			gauge.Value = Num(v) + g.Unit
			if strings.HasSuffix(g.Metric, "_bytes") {
				gauge.Value = Bytes(v)
			}
			gauge.Level = "ok"
			if g.Metric == "restarts" && v > 0 {
				gauge.Level = "amber"
			}
			if g.Metric == "failed" && v > 0 {
				gauge.Level = "red"
			}
		case g.Countdown:
			v, ok := es.Metrics[g.Metric]
			if !ok {
				continue
			}
			gauge.Value = fmt.Sprintf("%s %s", Num(v), Plural(v, "day", "days"))
			gauge.Pct = 100 - 100*v/90
			if gauge.Pct < 0 {
				gauge.Pct = 0
			}
			if gauge.Pct > 100 {
				gauge.Pct = 100
			}
			gauge.Level = "ok"
			if v <= th.CertWarnDays {
				gauge.Level = "amber"
			}
			if v <= 3 {
				gauge.Level = "red"
			}
		default:
			v, ok := es.Metrics[g.Metric]
			if !ok {
				continue
			}
			gauge.Pct = v
			gauge.Value = fmt.Sprintf("%.0f%%", v)
			if g.Metric == "hit_rate" {
				gauge.Level = "ok"
				if v < th.HitRateMinPct {
					gauge.Level = "red"
				}
			} else {
				gauge.Level = level(v, th)
			}
		}
		if trends != nil && g.Metric != "" {
			if slope, ok := trends[g.Metric]; ok {
				if slope > 0.5 {
					gauge.Trend = "up"
				} else if slope < -0.5 {
					gauge.Trend = "down"
				}
				if g.Metric == "disk_pct" && slope > 0.5 && gauge.Pct >= 0 && gauge.Pct < 100 {
					hours := (100 - gauge.Pct) / slope
					gauge.Detail = "full in about " + Dur(time.Duration(hours*float64(time.Hour)))
				}
			}
		}
		out = append(out, gauge)
	}
	return out
}

func level(pct float64, th model.ThresholdSet) string {
	switch {
	case pct >= th.GaugeRedPct:
		return "red"
	case pct >= th.GaugeAmberPct:
		return "amber"
	}
	return "ok"
}

// componentSeverity: failing and blocked are crit, waiting is warn, gauges
// above the red line raise a healthy component to warn, and a few
// type-specific early warnings.
func (c *ctx) componentSeverity(comp model.Component, es *model.ElementState) model.Severity {
	sev := model.SeverityOf(es.State)
	if es.Marker == model.MarkerUnbound {
		return model.Info
	}
	th := c.th(comp.ID)
	for _, g := range es.Gauges {
		if g.Level == "red" {
			sev = sev.Max(model.Warn)
		}
		if g.Detail != "" {
			es.Notes = append(es.Notes, g.Detail)
			sev = sev.Max(model.Warn)
		}
	}
	if days, ok := es.Metrics["cert_days"]; ok && days <= th.CertWarnDays && es.State != model.Failing {
		sev = sev.Max(model.Warn)
		es.Notes = append(es.Notes, fmt.Sprintf("cert expires in %s %s", Num(days), Plural(days, "day", "days")))
		if cnd, ok := model.HasCondition(es.Conditions, model.CondCertRenewalFailed); ok {
			n := "renewal has failed"
			if !cnd.Since.IsZero() {
				n += " since " + Day(cnd.Since)
			}
			if cnd.Detail != "" {
				n += ", " + cnd.Detail
			}
			es.Notes = append(es.Notes, n)
		}
	}
	if h, ok := es.Metrics["targets_healthy"]; ok {
		if t, ok := es.Metrics["targets_total"]; ok && t > 0 {
			es.Notes = append(es.Notes, fmt.Sprintf("%s of %s targets healthy", Num(h), Num(t)))
			if h < t {
				sev = sev.Max(model.Warn)
			}
		}
	}
	if es.State == model.Processing {
		if r, ok := es.Metrics["running_s"]; ok {
			if p95, ok := es.Metrics["p95_s"]; ok && p95 > 0 && r > th.StuckTaskFactor*p95 {
				sev = sev.Max(model.Warn)
				es.Notes = append(es.Notes, fmt.Sprintf("%s× the usual %s", Num(r/p95), Dur(time.Duration(p95*float64(time.Second)))))
				es.Detail["stuck"] = true
			}
		}
	}
	if comp.Type == "external" && es.State == model.Failing {
		es.Notes = append(es.Notes, "outside our control")
	}
	for _, e := range c.t.Incoming(comp.ID) {
		if ee, ok := c.snap.Edges[e.ID()]; ok && strings.Contains(ee.Label, "double normal") {
			sev = sev.Max(model.Warn)
			es.Notes = append(es.Notes, "double its normal load")
			break
		}
	}
	if comp.Type == "queue" && es.State == model.Waiting {
		if g, ok := es.Metrics["growth_per_min"]; ok && g > 0 {
			es.Notes = append(es.Notes, fmt.Sprintf("growing %s per minute", Num(g)))
		}
	}
	return sev
}

// markerNotes are the change stamps drawn on a component.
func (c *ctx) markerNotes(id string) []string {
	var out []string
	var evs []model.Event
	for _, e := range c.in.Events {
		if e.Target == id {
			evs = append(evs, e)
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.After(evs[j].At) })
	for i, e := range evs {
		if i >= 2 {
			break
		}
		s := e.Kind + " " + Clock(e.At)
		if e.Ref != "" {
			s += " " + short(e.Ref, 7)
		}
		out = append(out, s)
	}
	return out
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// WorstSeverity returns the worst severity in a snapshot with counts.
func WorstSeverity(s *model.Snapshot) (model.Severity, int, int) {
	worst := model.Info
	warn, crit := 0, 0
	count := func(es model.ElementState) {
		switch es.Severity {
		case model.Warn:
			warn++
		case model.Crit:
			crit++
		}
		worst = worst.Max(es.Severity)
	}
	for _, es := range s.Components {
		count(es)
	}
	for _, es := range s.Edges {
		count(es)
	}
	return worst, warn, crit
}

// Gauges rebuilds the gauges of a component from an element state (used
// when replaying compact history frames that carry metrics but no gauges).
func Gauges(th model.Thresholds, comp model.Component, es model.ElementState) []model.Gauge {
	c := &ctx{in: Input{Thresholds: th}}
	return c.gauges(comp, es)
}

// hostedOn lists what runs on a node: every component whose runs_on names
// it, plus any whose live placement (the pods the provider saw on this
// node) does, with the per-node counts when known; a component that says
// where its pods are and does not name this node has none here. The row takes the
// component's state, except that live counts tell the truth locally: a
// failing workload whose pods on this node are all ready and quiet reads
// flowing here, and a pod that is not ready on this node fails here even
// when the workload as a whole still has enough replicas.
func (c *ctx) hostedOn(nodeID string) []model.Hosted {
	var out []model.Hosted
	seen := map[string]bool{}
	add := func(comp model.Component) {
		if seen[comp.ID] {
			return
		}
		seen[comp.ID] = true
		ws := c.snap.Components[comp.ID]
		h := model.Hosted{ID: comp.ID, Label: comp.DisplayLabel(), State: ws.State, Marker: ws.Marker}
		if p, ok := c.t.Component(comp.Parent); ok && comp.Parent != "" {
			h.Label = p.DisplayLabel() + " " + comp.Notes // "Main database primary"
		}
		// A workload that says where its pods are says where they are not
		// as well: on a machine it does not name there is none.
		if all := c.placementOf(comp.ID); all != nil {
			pl := all[nodeID]
			h.Known, h.Pods, h.Ready, h.Restarts = true, pl.Pods, pl.Ready, pl.Restarts
			h.CPUPct, h.MemPct = pl.CPU, pl.Mem
			if r, ok := c.ratesByNode(comp.ID)[nodeID]; ok && pl.Pods > 0 {
				h.Rate = &r
			}
			switch {
			case pl.Pods == 0:
				h.State = model.Idle
			case pl.Ready < pl.Pods:
				h.State = model.Failing
			case ws.State == model.Failing && pl.Restarts == 0:
				h.State = model.Flowing
			}
		}
		out = append(out, h)
	}
	for _, comp := range c.t.Hosted(nodeID) {
		add(comp)
	}
	var extra []model.Component
	for _, comp := range c.t.Components {
		if seen[comp.ID] {
			continue
		}
		if _, ok := c.placementOf(comp.ID)[nodeID]; ok {
			extra = append(extra, comp)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].ID < extra[j].ID })
	for _, comp := range extra {
		add(comp)
	}
	return out
}

// placement is what a provider reported about one component on one node.
type placement struct {
	Pods, Ready, Restarts int
	CPU, Mem              *float64 // nil when not read
}

// ratesByNode reads the "rate_by_node" detail a probe of the component
// itself leaves (a router scraped pod by pod), keyed by the id of the node.
// The rate of an edge is not the rate of the pods at its end: a router on
// one machine sends to an API on every machine.
func (c *ctx) ratesByNode(compID string) map[string]float64 {
	j := c.joined(compID)
	if j == nil {
		return nil
	}
	out := map[string]float64{}
	switch m := j.Detail["rate_by_node"].(type) {
	case map[string]float64:
		for node, r := range m {
			out[c.nodeID(node)] = r
		}
	case map[string]any:
		for node, v := range m {
			if r, ok := v.(float64); ok {
				out[c.nodeID(node)] = r
			}
		}
	}
	return out
}

// pctOf reads an optional percentage of a placement.
func pctOf(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case int:
		f := float64(n)
		return &f
	}
	return nil
}

// nodeID returns the id on the diagram of the machine a provider knows by
// name: the node whose probe reported that name, then the node of that id,
// then the slug, so "worker-node-2" finds worker-node-2.
func (c *ctx) nodeID(name string) string {
	if c.nodeNames == nil {
		c.nodeNames = map[string]string{}
		for _, comp := range c.t.AllComponents() {
			if comp.Type != "node" {
				continue
			}
			if n := detailStr(c.joined(comp.ID), "name"); n != "" {
				c.nodeNames[n] = comp.ID
			}
		}
	}
	if id, ok := c.nodeNames[name]; ok {
		return id
	}
	if _, ok := c.t.Component(name); ok {
		return name
	}
	return model.SlugifyID(name)
}

// placementOf reads the "placement" detail a workload probe leaves: node
// name -> pods, ready, restarts, keyed here by the id of the node.
func (c *ctx) placementOf(compID string) map[string]placement {
	j := c.joined(compID)
	if j == nil {
		return nil
	}
	raw, ok := j.Detail["placement"]
	if !ok {
		return nil
	}
	out := map[string]placement{}
	each := func(node string, fields map[string]any) {
		out[c.nodeID(node)] = placement{Pods: toInt(fields["pods"]), Ready: toInt(fields["ready"]), Restarts: toInt(fields["restarts"]),
			CPU: pctOf(fields["cpu_pct"]), Mem: pctOf(fields["mem_pct"])}
	}
	switch m := raw.(type) {
	case map[string]any:
		for node, v := range m {
			switch f := v.(type) {
			case map[string]any:
				each(node, f)
			case map[string]int:
				fa := map[string]any{}
				for k, n := range f {
					fa[k] = n
				}
				each(node, fa)
			}
		}
	case map[string]map[string]int:
		for node, f := range m {
			fa := map[string]any{}
			for k, n := range f {
				fa[k] = n
			}
			each(node, fa)
		}
	}
	return out
}

// toInt reads a JSON or Go number.
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
