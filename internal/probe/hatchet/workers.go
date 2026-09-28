package hatchet

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
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
	Kind:   KindWorkers,
	Source: "the Hatchet REST API: the worker list, running and completed task runs and the tenant queue metrics",
	Delivers: "workers_online, workers_total, pool_used, pool_max (slots), active (running tasks), waiters (queued + pending tasks), " +
		"running_s (longest running task), p95_s (task duration over the last hour); TaskRunning past long_task, PoolExhausted, NotReady; " +
		"detail: workers, long tasks",
	SpecFields:  withFields("name", "long_task"),
	Needs:       "a Hatchet API token in the environment variable named by token_env (default HATCHET_CLIENT_TOKEN); the tenant id from the spec or from the token",
	Implemented: true,
}

func init() {
	probe.Register(workersAccess, func() probe.Probe { return &WorkersProbe{} })
}

// WorkersProbe is hatchet.workers, bound to the workload that runs the
// workers. Spec: url (required), token_env, tenant, interval, timeout, name
// (optional prefix filter on the worker name), long_task (default 10m).
type WorkersProbe struct {
	h probe.Health
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
	go run(ctx, out, st.cfg.tick, st.cfg.interval, func(ctx context.Context) probe.Observation {
		return p.poll(ctx, st)
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
	o.Metrics["workers_online"] = float64(online)
	o.Metrics["workers_total"] = float64(len(workers))
	if slotsKnown {
		o.Metrics["pool_used"] = poolUsed
		o.Metrics["pool_max"] = poolMax
	}

	var problems []string
	live := map[string]bool{}

	running, err := st.c.runs(ctx, runQuery{
		since:     now.Add(-runningLookback),
		statuses:  []string{statusRunning},
		onlyTasks: true,
		limit:     runPage,
	})
	if err != nil {
		problems = append(problems, "running tasks: "+err.Error())
	} else {
		o.Metrics["active"] = float64(len(running))
		type aged struct {
			run taskRun
			age time.Duration
		}
		var ages []aged
		for _, r := range running {
			if age, ok := r.age(now); ok {
				ages = append(ages, aged{r, age})
			}
		}
		sort.Slice(ages, func(i, j int) bool { return ages[i].age > ages[j].age })
		if len(ages) > 0 {
			o.Metrics["running_s"] = ages[0].age.Seconds()
		}
		var long []map[string]any
		for _, a := range ages {
			if a.age < st.longTask {
				break
			}
			long = append(long, map[string]any{
				"task": a.run.DisplayName, "id": a.run.TaskExternalID, "running_s": a.age.Seconds(),
			})
			if len(o.Conditions) < maxLongTasks {
				o.Conditions = append(o.Conditions, model.Condition{
					Kind:   model.CondTaskRunning,
					Ref:    a.run.TaskExternalID,
					Since:  firstOf(a.run.StartedAt, a.run.CreatedAt),
					Detail: a.run.DisplayName,
				})
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
		o.Metrics["waiters"] = qm.total().depth()
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
			o.Metrics["p95_s"] = percentile(durations, 95)
		}
		o.Detail["completed_last_hour"] = len(done)
	}

	waiters, haveWaiters := o.Metrics["waiters"]
	if slotsKnown && poolMax > 0 && poolUsed >= poolMax && haveWaiters && waiters > 0 {
		live["pool"] = true
		o.Conditions = append(o.Conditions, model.Condition{
			Kind:   model.CondPoolExhausted,
			Since:  st.seen.mark("pool", now),
			Detail: fmt.Sprintf("all %g slots busy", poolMax),
		})
	}
	if online == 0 {
		live["notready"] = true
		o.Conditions = append(o.Conditions, model.Condition{
			Kind:   model.CondNotReady,
			Since:  st.seen.mark("notready", now),
			Detail: "no active Hatchet workers",
		})
	}
	st.seen.keep(live)

	if len(problems) > 0 {
		p.h.Set(probe.HealthDegraded, joinProblems(problems))
	} else {
		p.h.Set(probe.HealthOK, "")
	}
	return o
}
