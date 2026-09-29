package hatchet

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// KindWorkers is the probe kind of the workers probe.
const KindWorkers = "hatchet.workers"

const (
	// defaultLongTask is the age past which a RUNNING task raises TaskRunning.
	defaultLongTask = 10 * time.Minute
	// runningLookback bounds how far back RUNNING tasks are searched.
	runningLookback = 24 * time.Hour
	// p95Window is the window over which p95_s is computed.
	p95Window = time.Hour
	// p95MinSamples is the smallest sample p95_s is reported from.
	p95MinSamples = 10
	// maxLongTasks caps the TaskRunning conditions per observation.
	maxLongTasks = 5
	// runPage is the page size requested for run lists.
	runPage = 200
)

var workersAccess = probe.Access{
	Kind: KindWorkers,
	Source: "the Hatchet REST API: the worker list, running and completed task runs and the tenant task-stats " +
		"(queue-metrics, then step-run-queue-metrics, on servers without it)",
	Delivers: "workers_online, workers_total, pool_used, pool_max (slots), active (running tasks), waiters (queued tasks, + pending where the server reports it), " +
		"running_s (longest running task), p95_s (task duration over the last hour); TaskRunning past long_task, PoolExhausted, " +
		"NotReady when listed workers are all inactive (an empty list concludes nothing: workers_note says so); " +
		"detail: workers, long tasks, queue_source, workers_note, url, via",
	SpecFields:  withFields("name", "long_task"),
	Needs:       "a Hatchet API token in the environment variable named by token_env (default HATCHET_CLIENT_TOKEN); the tenant id from the spec or from the token" + viaNeeds,
	Implemented: true,
	Tier:        probe.TierToken,
	Facets:      []string{facet.NameBackgroundWorker},
}

func init() {
	probe.Register(workersAccess, func() probe.Probe { return &WorkersProbe{} })
}

// WorkersProbe is hatchet.workers, bound to the workload that runs the
// workers. Spec: url (required, unless via names a tunnel), token_env,
// tenant, interval, timeout, via, name (optional prefix filter on the worker
// name), long_task (default 10m).
//
// A worker list with no worker in it says nothing about the fleet: the
// token may belong to another tenant than the workers', or the name filter
// may match none of them, while the pods run jobs. The fleet is then left
// without workers_online and workers_total, so it is never read as not
// ready; only listed workers that are all inactive are.
type WorkersProbe struct {
	h probe.Health
	probe.Lifetime
}

// workersState is what one WorkersProbe carries between polls.
type workersState struct {
	cfg      config
	c        *client
	name     string
	longTask time.Duration
	seen     firstSeen
}

// Kind implements probe.Probe.
func (p *WorkersProbe) Kind() string { return KindWorkers }

// Validate implements probe.Probe.
func (p *WorkersProbe) Validate(spec map[string]any) error {
	if err := validateCommon(spec); err != nil {
		return err
	}
	return validateDurations(spec, "long_task")
}

// Health implements probe.Probe.
func (p *WorkersProbe) Health() probe.ProbeHealth { return p.h.Get() }

// setup parses the spec into a state, or reports a misconfiguration.
func (p *WorkersProbe) setup(spec map[string]any) (*workersState, error) {
	if err := p.Validate(spec); err != nil {
		return nil, err
	}
	cfg, c, err := configure(spec, requirements{token: true, tenant: true})
	if err != nil {
		return nil, err
	}
	return &workersState{
		cfg:      cfg,
		c:        c,
		name:     probe.Str(spec, "name", ""),
		longTask: probe.Dur(spec, "long_task", defaultLongTask),
		seen:     firstSeen{},
	}, nil
}

// Start implements probe.Probe.
func (p *WorkersProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	st, err := p.setup(spec)
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	p.Go(func() {
		run(ctx, out, st.cfg.tick, st.cfg.interval, st.c, &p.h, func(ctx context.Context) probe.Observation {
			return p.poll(ctx, st)
		})
	})
	return nil
}

// poll reads the workers once and builds the observation.
func (p *WorkersProbe) poll(ctx context.Context, st *workersState) probe.Observation {
	now := time.Now()
	ctx, cancel := context.WithTimeout(ctx, st.cfg.timeout*4)
	defer cancel()

	all, err := st.c.workers(ctx)
	if err != nil {
		return errObservation(&p.h, KindWorkers, st.cfg.target, now, err)
	}
	var workers []worker
	for _, w := range all {
		if st.name == "" || strings.HasPrefix(w.Name, st.name) {
			workers = append(workers, w)
		}
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })

	o := probe.Observation{
		Target:  st.cfg.target,
		Probe:   KindWorkers,
		At:      now,
		Metrics: map[string]float64{},
		Detail:  map[string]any{"url": st.cfg.base, "tenant": st.cfg.tenant},
	}
	st.cfg.noteVia(o.Detail)
	if st.name != "" {
		o.Detail["name"] = st.name
	}
	online := 0
	var poolUsed, poolMax float64
	slotsKnown := false
	list := make([]map[string]any, 0, len(workers))
	for _, w := range workers {
		if w.active() {
			online++
		}
		entry := map[string]any{"name": w.Name, "status": w.Status, "id": w.Metadata.ID}
		if !w.LastHeartbeatAt.IsZero() {
			entry["last_heartbeat"] = w.LastHeartbeatAt.Time
		}
		if used, max, ok := w.slots(); ok {
			entry["slots"] = fmt.Sprintf("%g/%g", used, max)
			if w.active() {
				poolUsed += used
				poolMax += max
				slotsKnown = true
			}
		}
		list = append(list, entry)
	}
	o.Detail["workers"] = list
	pool := facet.BackgroundWorkerFacet{
		LongTask:       st.longTask,
		NotReadyDetail: "no active Hatchet workers",
		Since:          func(key string, at time.Time) time.Time { return st.seen.mark(key, at) },
	}
	if len(workers) > 0 {
		pool.Online, pool.Total = facet.NI(online), facet.NI(len(workers))
	} else {
		o.Detail["workers_note"] = noWorkersNote(st.name)
	}
	if slotsKnown {
		pool.SlotsUsed, pool.SlotsMax = facet.N(poolUsed), facet.N(poolMax)
	}

	var problems []string

	running, err := st.c.runs(ctx, runQuery{
		since:     now.Add(-runningLookback),
		statuses:  []string{statusRunning},
		onlyTasks: true,
		limit:     runPage,
	})
	if err != nil {
		problems = append(problems, "running tasks: "+err.Error())
	} else {
		pool.Active = facet.NI(len(running))
		var long []map[string]any
		for _, r := range running {
			started := firstOf(r.StartedAt, r.CreatedAt)
			pool.Running = append(pool.Running, facet.Task{ID: r.TaskExternalID, Name: r.DisplayName, Started: started})
			if age, ok := r.age(now); ok && age >= st.longTask {
				long = append(long, map[string]any{"task": r.DisplayName, "id": r.TaskExternalID, "running_s": age.Seconds()})
			}
		}
		if len(long) > 0 {
			o.Detail["long_tasks"] = long
		}
	}

	qm, err := st.c.queueMetrics(ctx)
	if err != nil {
		problems = append(problems, "queue metrics: "+err.Error())
	} else {
		pool.Backlog = facet.N(qm.total().depth())
		o.Detail["queue_source"] = qm.source
	}

	done, err := st.c.runs(ctx, runQuery{
		since:     now.Add(-p95Window),
		statuses:  []string{statusCompleted},
		onlyTasks: true,
		limit:     runPage,
	})
	if err != nil {
		problems = append(problems, "completed tasks: "+err.Error())
	} else {
		var durations []float64
		for _, r := range done {
			if d, ok := r.duration(); ok && d >= 0 {
				durations = append(durations, d.Seconds())
			}
		}
		if len(durations) >= p95MinSamples {
			pool.TypicalDuration = time.Duration(percentile(durations, 95) * float64(time.Second))
		}
		o.Detail["completed_last_hour"] = len(done)
	}

	// The facet writes the canonical form: metrics, TaskRunning,
	// PoolExhausted, NotReady, the same for every worker backend.
	facet.EmitBackgroundWorker(&o, pool, now)
	live := map[string]bool{}
	for _, c := range o.Conditions {
		switch c.Kind {
		case model.CondPoolExhausted:
			live[facet.KeyPoolExhausted] = true
		case model.CondNotReady:
			live[facet.KeyNoWorkers] = true
		}
	}
	st.seen.keep(live)

	if len(problems) > 0 {
		p.h.Set(probe.HealthDegraded, joinProblems(problems))
	} else {
		p.h.Set(probe.HealthOK, "")
	}
	return o
}

// noWorkersNote is the detail of a worker list with no worker in it.
func noWorkersNote(name string) string {
	if name != "" {
		return fmt.Sprintf("Hatchet lists no worker whose name starts with %q for this tenant; check name, "+
			"or the token may belong to another tenant than the workers'", name)
	}
	return "Hatchet lists no worker for this tenant; the token may belong to another tenant than the workers'"
}
