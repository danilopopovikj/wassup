package hatchet

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// KindWorkflow is the probe kind of the workflow probe.
const KindWorkflow = "hatchet.workflow"

const (
	// defaultWindow is the window task counts and latest runs are read over.
	defaultWindow = 24 * time.Hour
	// latestRuns is how many recent runs are read for the run conditions.
	latestRuns = 20
	// errorMessageLen bounds the error message kept in JobFailed's Detail.
	errorMessageLen = 120
)

var workflowAccess = probe.Access{
	Kind:   KindWorkflow,
	Source: "the Hatchet REST API: the workflow list, per-workflow task metrics, the latest workflow runs and the cron triggers",
	Delivers: "succeeded, failed, active, queued, cancelled over the window, running_s; JobFailed when the latest finished run failed, " +
		"JobRunning while a run is running; a job event per failed run; detail: schedule, cron, last_success, last_failure, workflow_id",
	SpecFields:  withFields("workflow", "window"),
	Needs:       "a Hatchet API token in the environment variable named by token_env (default HATCHET_CLIENT_TOKEN); the tenant id from the spec or from the token",
	Implemented: true,
	Facets:      []string{facet.NameScheduledJob},
}

func init() {
	probe.Register(workflowAccess, func() probe.Probe { return &WorkflowProbe{} })
}

// WorkflowProbe is hatchet.workflow, bound to a job component. Spec: url and
// workflow (required), token_env, tenant, interval, timeout, window (default
// 24h).
type WorkflowProbe struct {
	h probe.Health
}

// workflowState is what one WorkflowProbe carries between polls.
type workflowState struct {
	cfg      config
	c        *client
	workflow string
	window   time.Duration
	// emitted holds the ids of failed runs already announced as events.
	emitted map[string]bool
}

// Kind implements probe.Probe.
func (p *WorkflowProbe) Kind() string { return KindWorkflow }

// Validate implements probe.Probe.
func (p *WorkflowProbe) Validate(spec map[string]any) error {
	if err := validateCommon(spec); err != nil {
		return err
	}
	if err := probe.RequireString(spec, "workflow"); err != nil {
		return err
	}
	return validateDurations(spec, "window")
}

// Health implements probe.Probe.
func (p *WorkflowProbe) Health() probe.ProbeHealth { return p.h.Get() }

// setup parses the spec into a state, or reports a misconfiguration.
func (p *WorkflowProbe) setup(spec map[string]any) (*workflowState, error) {
	if err := p.Validate(spec); err != nil {
		return nil, err
	}
	cfg, c, err := configure(spec, requirements{token: true, tenant: true})
	if err != nil {
		return nil, err
	}
	window := probe.Dur(spec, "window", defaultWindow)
	if window <= 0 {
		window = defaultWindow
	}
	return &workflowState{
		cfg:      cfg,
		c:        c,
		workflow: probe.Str(spec, "workflow", ""),
		window:   window,
		emitted:  map[string]bool{},
	}, nil
}

// Start implements probe.Probe.
func (p *WorkflowProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
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

// poll reads the workflow once and builds the observation.
func (p *WorkflowProbe) poll(ctx context.Context, st *workflowState) probe.Observation {
	now := time.Now()
	ctx, cancel := context.WithTimeout(ctx, st.cfg.timeout*4)
	defer cancel()

	id, err := st.c.workflowID(ctx, st.workflow)
	if err != nil {
		return errObservation(&p.h, KindWorkflow, st.cfg.target, now, err)
	}
	since := now.Add(-st.window)
	counts, err := st.c.taskMetrics(ctx, since, id)
	if err != nil {
		var nf errWorkflowNotFound
		if notFound(err) || errors.As(err, &nf) {
			st.c.forgetWorkflow(st.workflow)
		}
		return errObservation(&p.h, KindWorkflow, st.cfg.target, now, err)
	}

	o := probe.Observation{
		Target: st.cfg.target,
		Probe:  KindWorkflow,
		At:     now,
		Metrics: map[string]float64{
			"succeeded": counts[statusCompleted],
			"failed":    counts[statusFailed],
			"active":    counts[statusRunning],
			"queued":    counts[statusQueued],
			"cancelled": counts[statusCancelled],
		},
		Detail: map[string]any{
			"url":         st.cfg.base,
			"tenant":      st.cfg.tenant,
			"workflow":    st.workflow,
			"workflow_id": id,
			"window":      st.window.String(),
		},
	}

	var problems []string
	runs, err := st.c.runs(ctx, runQuery{
		since:       since,
		onlyTasks:   false,
		workflowIDs: []string{id},
		limit:       latestRuns,
	})
	if err != nil {
		problems = append(problems, "runs: "+err.Error())
	} else {
		p.applyRuns(&o, st, runs, now)
	}

	crons, err := st.c.crons(ctx)
	if err != nil {
		problems = append(problems, "crons: "+err.Error())
	} else {
		var exprs []string
		var entries []map[string]any
		for _, c := range crons {
			if c.WorkflowName != st.workflow {
				continue
			}
			entries = append(entries, map[string]any{"cron": c.Cron, "name": c.Name, "enabled": c.Enabled})
			if c.Enabled {
				exprs = append(exprs, c.Cron)
			}
		}
		if len(entries) > 0 {
			o.Detail["crons"] = entries
		}
		if len(exprs) > 0 {
			sort.Strings(exprs)
			o.Detail["cron"] = exprs[0]
			o.Detail["schedule"] = "cron " + exprs[0]
		}
	}

	if len(problems) > 0 {
		p.h.Set(probe.HealthDegraded, joinProblems(problems))
	} else {
		p.h.Set(probe.HealthOK, "")
	}
	return o
}

// applyRuns folds the latest runs into the observation: JobFailed when the
// most recent finished run failed, JobRunning while a run runs, last_success
// and last_failure, and one job event per newly seen failed run.
func (p *WorkflowProbe) applyRuns(o *probe.Observation, st *workflowState, runs []taskRun, now time.Time) {
	sorted := append([]taskRun(nil), runs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.After(sorted[j].CreatedAt.Time) })

	var latestFinished, lastSuccess, lastFailure, running *taskRun
	present := map[string]bool{}
	for i := range sorted {
		r := &sorted[i]
		present[r.runID()] = true
		if r.finished() && latestFinished == nil {
			latestFinished = r
		}
		switch r.Status {
		case statusCompleted:
			if lastSuccess == nil {
				lastSuccess = r
			}
		case statusFailed:
			if lastFailure == nil {
				lastFailure = r
			}
			if id := r.runID(); id != "" && !st.emitted[id] {
				st.emitted[id] = true
				o.Events = append(o.Events, model.Event{
					At:      firstOf(r.FinishedAt, r.CreatedAt),
					Kind:    "job",
					Target:  st.cfg.target,
					Summary: st.workflow + " run failed",
					Ref:     id,
				})
			}
		case statusRunning:
			if running == nil {
				running = r
			}
		}
	}
	// Forget failed runs that left the latest page; they cannot come back.
	for id := range st.emitted {
		if !present[id] {
			delete(st.emitted, id)
		}
	}

	if latestFinished != nil {
		o.Detail["latest_status"] = latestFinished.Status
	}
	if lastSuccess != nil {
		o.Detail["last_success"] = firstOf(lastSuccess.FinishedAt, lastSuccess.CreatedAt)
	}
	if lastFailure != nil {
		o.Detail["last_failure"] = firstOf(lastFailure.FinishedAt, lastFailure.CreatedAt)
	}
	job := facet.ScheduledJobFacet{Active: facet.N(o.Metrics["active"]), Succeeded: facet.N(o.Metrics["succeeded"]), Failed: facet.N(o.Metrics["failed"]), Queued: facet.N(o.Metrics["queued"])}
	if latestFinished != nil && latestFinished.Status == statusFailed {
		job.LastFailure = &facet.Failure{Ref: "run/" + latestFinished.runID(), At: firstOf(latestFinished.FinishedAt, latestFinished.CreatedAt), Reason: truncate(latestFinished.ErrorMessage, errorMessageLen)}
	}
	if running != nil {
		job.Running = &facet.Task{ID: "run/" + running.runID(), Name: running.DisplayName, Started: firstOf(running.StartedAt, running.CreatedAt)}
	}
	facet.EmitScheduledJob(o, job, now)
	if v, ok := o.Metrics["running_s"]; ok && v < 0 {
		o.Metrics["running_s"] = 0
	}
}
