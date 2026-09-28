package hatchet

import (
	"context"
	"sort"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// KindQueue is the probe kind of the queue probe.
const KindQueue = "hatchet.queue"

// queuedLookback bounds how far back the oldest queued task is searched.
const queuedLookback = 24 * time.Hour

// topQueues caps the queues listed in detail.
const topQueues = 10

var queueAccess = probe.Access{
	Kind:   KindQueue,
	Source: "the Hatchet REST API: tenant queue metrics, the worker list and the queued task runs",
	Delivers: "depth (queued + pending), pending, running, active, growth_per_min, consumers (active workers), " +
		"oldest_age_s (age of the oldest queued task); detail: queues, total, the queue or workflow filter, url",
	SpecFields:  withFields("queue", "workflow"),
	Needs:       "a Hatchet API token in the environment variable named by token_env (default HATCHET_CLIENT_TOKEN); the tenant id from the spec or from the token",
	Implemented: true,
	Facets:      []string{facet.NameQueue},
}

func init() {
	probe.Register(queueAccess, func() probe.Probe { return &QueueProbe{} })
}

// QueueProbe is hatchet.queue, bound to a queue component. Spec: url
// (required), token_env, tenant, interval, timeout, and at most one of queue
// (a name in the queues map) or workflow (a name in the workflow map); with
// neither, the tenant total is reported.
type QueueProbe struct {
	h probe.Health
}

// queueState is what one QueueProbe carries between polls.
type queueState struct {
	cfg      config
	c        *client
	queue    string
	workflow string
	ring     depthRing
}

// Kind implements probe.Probe.
func (p *QueueProbe) Kind() string { return KindQueue }

// Validate implements probe.Probe.
func (p *QueueProbe) Validate(spec map[string]any) error {
	if err := validateCommon(spec); err != nil {
		return err
	}
	if probe.Str(spec, "queue", "") != "" && probe.Str(spec, "workflow", "") != "" {
		return errBothFilters
	}
	return nil
}

// errBothFilters is the Validate error for queue and workflow together.
var errBothFilters = errorString("set queue or workflow, not both")

// errorString is a constant error.
type errorString string

func (e errorString) Error() string { return string(e) }

// Health implements probe.Probe.
func (p *QueueProbe) Health() probe.ProbeHealth { return p.h.Get() }

// setup parses the spec into a state, or reports a misconfiguration.
func (p *QueueProbe) setup(spec map[string]any) (*queueState, error) {
	if err := p.Validate(spec); err != nil {
		return nil, err
	}
	cfg, c, err := configure(spec, requirements{token: true, tenant: true})
	if err != nil {
		return nil, err
	}
	return &queueState{
		cfg:      cfg,
		c:        c,
		queue:    probe.Str(spec, "queue", ""),
		workflow: probe.Str(spec, "workflow", ""),
	}, nil
}

// Start implements probe.Probe.
func (p *QueueProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	st, err := p.setup(spec)
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	go run(ctx, out, st.cfg.tick, st.cfg.interval, func(ctx context.Context) probe.Observation {
		return p.poll(ctx, st)
	})
	return nil
}

// poll reads the queue once and builds the observation.
func (p *QueueProbe) poll(ctx context.Context, st *queueState) probe.Observation {
	now := time.Now()
	ctx, cancel := context.WithTimeout(ctx, st.cfg.timeout*3)
	defer cancel()

	qm, err := st.c.queueMetrics(ctx)
	if err != nil {
		return errObservation(&p.h, KindQueue, st.cfg.target, now, err)
	}
	var s shape
	listed := true
	switch {
	case st.queue != "":
		s, listed = qm.queue(st.queue)
	case st.workflow != "":
		s, listed, err = qm.workflow(st.workflow)
		if err != nil {
			return errObservation(&p.h, KindQueue, st.cfg.target, now, err)
		}
	default:
		s = qm.total()
	}

	o := probe.Observation{
		Target:  st.cfg.target,
		Probe:   KindQueue,
		At:      now,
		Metrics: map[string]float64{},
		Detail: map[string]any{
			"url":    st.cfg.base,
			"tenant": st.cfg.tenant,
			"queues": topN(qm.Queues, topQueues),
			"legacy": qm.legacy,
		},
	}
	q := facet.QueueFacet{Depth: facet.N(s.queued)}
	if s.known {
		q.Pending, q.Running = facet.N(s.pending), facet.N(s.running)
	}
	if t := qm.total(); t.known {
		o.Detail["total"] = map[string]any{"queued": t.queued, "pending": t.pending, "running": t.running}
	} else {
		o.Detail["total"] = map[string]any{"queued": t.queued}
	}
	if st.queue != "" {
		o.Detail["queue"] = st.queue
		o.Detail["listed"] = listed
	}
	if st.workflow != "" {
		o.Detail["workflow"] = st.workflow
		o.Detail["listed"] = listed
	}
	st.ring.push(now, s.depth())
	if g, ok := st.ring.growth(); ok {
		q.GrowthPerMin = facet.N(g)
	}

	var problems []string
	filter := st.queue
	if filter == "" {
		filter = st.workflow
	}
	workers, err := st.c.workers(ctx)
	if err != nil {
		problems = append(problems, "workers: "+err.Error())
	} else {
		consumers := 0
		for _, w := range workers {
			if w.active() && (filter == "" || w.serves(filter)) {
				consumers++
			}
		}
		q.Consumers = facet.NI(consumers)
	}

	if age, scope, err := p.oldestQueued(ctx, st, now); err != nil {
		problems = append(problems, "queued runs: "+err.Error())
	} else if scope != "" {
		q.Oldest, q.HasOldest = age, true
		o.Detail["oldest_scope"] = scope
	}
	facet.EmitQueue(&o, q, now)

	if len(problems) > 0 {
		p.h.Set(probe.HealthDegraded, joinProblems(problems))
	} else {
		p.h.Set(probe.HealthOK, "")
	}
	return o
}

// oldestQueued returns the age of the oldest QUEUED task. With a workflow
// filter the search is scoped to that workflow; otherwise it is tenant-wide
// (Hatchet does not expose the queue of a task). scope is "" when there is
// no queued task.
func (p *QueueProbe) oldestQueued(ctx context.Context, st *queueState, now time.Time) (time.Duration, string, error) {
	q := runQuery{
		since:     now.Add(-queuedLookback),
		statuses:  []string{statusQueued},
		onlyTasks: true,
		limit:     200,
	}
	scope := "tenant"
	if st.workflow != "" {
		id, err := st.c.workflowID(ctx, st.workflow)
		if err != nil {
			return 0, "", err
		}
		q.workflowIDs = []string{id}
		scope = "workflow"
	}
	rows, err := st.c.runs(ctx, q)
	if err != nil {
		return 0, "", err
	}
	var oldest time.Time
	for _, r := range rows {
		if r.CreatedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || r.CreatedAt.Before(oldest) {
			oldest = r.CreatedAt.Time
		}
	}
	if oldest.IsZero() {
		return 0, "", nil
	}
	age := now.Sub(oldest)
	if age < 0 {
		age = 0
	}
	return age, scope, nil
}

// topN returns the n largest entries of a queue map as a name-sorted list.
func topN(m map[string]float64, n int) []map[string]any {
	type kv struct {
		k string
		v float64
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	if len(all) > n {
		all = all[:n]
	}
	out := make([]map[string]any, 0, len(all))
	for _, e := range all {
		out = append(out, map[string]any{"name": e.k, "queued": e.v})
	}
	return out
}

// joinProblems joins partial-read problems into one health message.
func joinProblems(ps []string) string {
	msg := ""
	for i, p := range ps {
		if i > 0 {
			msg += "; "
		}
		msg += p
	}
	return msg
}
