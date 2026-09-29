package hatchet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
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
	// countsEvery is how often the task counts are read: counting a busy
	// workflow's day takes the server tens of seconds, and a count a minute
	// old still says how the day went.
	countsEvery = time.Minute
	// countsMaxAge is the oldest a count may be and still be reported.
	countsMaxAge = 3 * time.Minute
	// widenEvery is how long a narrower window that answered is kept before
	// the configured window is tried again.
	widenEvery = time.Hour
)

// narrowerWindows are the windows the task counts fall back to, in order,
// when the server cannot count the configured window in time.
var narrowerWindows = []time.Duration{6 * time.Hour, time.Hour}

var workflowAccess = probe.Access{
	Kind: KindWorkflow,
	Source: "the Hatchet REST API: the workflow list, per-workflow task metrics (read beside the polls, at most once a minute, " +
		"with a timeout of their own), the latest workflow runs and the cron triggers",
	Delivers: "succeeded, failed, active, queued, cancelled over the window that answered, rate (finished runs per second over it), running_s; " +
		"JobFailed when the latest finished run failed, JobRunning while a run is running; a job event per failed run; " +
		"detail: window (the window the counts cover, 24h falling back to 6h and 1h when counting takes too long), counts_at, " +
		"latest_status, last_run, last_success, last_failure, schedule, cron, workflow_id, url, via",
	SpecFields:  withFields("workflow", "window"),
	Needs:       "a Hatchet API token in the environment variable named by token_env (default HATCHET_CLIENT_TOKEN); the tenant id from the spec or from the token" + viaNeeds,
	Implemented: true,
	Tier:        probe.TierToken,
	Facets:      []string{facet.NameScheduledJob},
}

func init() {
	probe.Register(workflowAccess, func() probe.Probe { return &WorkflowProbe{} })
}

// WorkflowProbe is hatchet.workflow, bound to a job component. Spec:
// workflow and url (required, url unless via names a tunnel), token_env,
// tenant, interval, timeout, via, window (default 24h).
type WorkflowProbe struct {
	h probe.Health
	probe.Lifetime
}

// workflowState is what one WorkflowProbe carries between polls.
type workflowState struct {
	cfg      config
	c        *client
	workflow string
	window   time.Duration
	// emitted holds the ids of failed runs already announced as events.
	emitted map[string]bool
	// life is the probe's context: the task counts are read beside the
	// rounds and end with the probe, not with a round.
	life   context.Context
	counts countsCache
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
		life:     context.Background(),
	}, nil
}

// Start implements probe.Probe.
func (p *WorkflowProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	st, err := p.setup(spec)
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	st.life = ctx
	p.Go(func() {
		run(ctx, out, st.cfg.tick, st.cfg.interval, st.c, &p.h, func(ctx context.Context) probe.Observation {
			return p.poll(ctx, st)
		})
	})
	return nil
}

// poll reads the workflow once and builds the observation. The task counts
// come from countsCache: a read that is due starts beside the round, the
// round waits for it as long as one call may take, and a read that takes
// longer lands in a later round. The runs list and the crons are read every
// round; the counts failing never fails the observation.
func (p *WorkflowProbe) poll(ctx context.Context, st *workflowState) probe.Observation {
	now := time.Now()
	ctx, cancel := context.WithTimeout(ctx, st.cfg.timeout*4)
	defer cancel()

	id, err := st.c.workflowID(ctx, st.workflow)
	if err != nil {
		return errObservation(&p.h, KindWorkflow, st.cfg.target, now, err)
	}
	done := st.counts.refresh(st, id, now)
	wait := time.NewTimer(st.cfg.timeout)
	select {
	case <-done:
	case <-wait.C:
	case <-ctx.Done():
	}
	wait.Stop()

	o := probe.Observation{
		Target:  st.cfg.target,
		Probe:   KindWorkflow,
		At:      now,
		Metrics: map[string]float64{},
		Detail: map[string]any{
			"url":         st.cfg.base,
			"tenant":      st.cfg.tenant,
			"workflow":    st.workflow,
			"workflow_id": id,
		},
	}
	st.cfg.noteVia(o.Detail)

	var problems []string
	job := facet.ScheduledJobFacet{}
	if tc, err := st.counts.current(time.Now()); err != nil {
		problems = append(problems, "task counts: "+err.Error())
	} else if tc != nil {
		tc.apply(&o, &job, st.window, st.c.slow.Timeout)
	} else {
		o.Detail["counts_note"] = "the task counts over " + st.window.String() + " are still being read"
	}

	runs, err := st.c.runs(ctx, runQuery{
		since:       now.Add(-st.window),
		onlyTasks:   false,
		workflowIDs: []string{id},
		limit:       latestRuns,
	})
	if err != nil {
		problems = append(problems, "runs: "+err.Error())
	} else {
		p.applyRuns(&o, st, runs, &job)
	}
	facet.EmitScheduledJob(&o, job, now)
	if v, ok := o.Metrics["running_s"]; ok && v < 0 {
		o.Metrics["running_s"] = 0
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

// taskCounts is one read of task-metrics: tasks per status over window, as
// the server counted them at at.
type taskCounts struct {
	byStatus map[string]float64
	window   time.Duration
	at       time.Time
}

// apply writes the counts into the observation and the job: the counts,
// the rate of finished runs over the window they cover, and that window.
// configured is the window the binding asked for; a narrower one is said,
// with the timeout the wider ones ran out of.
func (tc *taskCounts) apply(o *probe.Observation, job *facet.ScheduledJobFacet, configured, timeout time.Duration) {
	succeeded, failed, cancelled := tc.byStatus[statusCompleted], tc.byStatus[statusFailed], tc.byStatus[statusCancelled]
	job.Succeeded, job.Failed = facet.N(succeeded), facet.N(failed)
	job.Active, job.Queued = facet.N(tc.byStatus[statusRunning]), facet.N(tc.byStatus[statusQueued])
	job.Rate = facet.N((succeeded + failed + cancelled) / tc.window.Seconds())
	o.Metrics["queued"] = tc.byStatus[statusQueued]
	o.Metrics["cancelled"] = cancelled
	o.Detail["window"] = tc.window.String()
	o.Detail["counts_at"] = tc.at
	if tc.window < configured {
		o.Detail["window_note"] = "counting " + configured.String() + " takes this server longer than " +
			timeout.String() + "; the counts cover the last " + tc.window.String()
	}
}

// countsCache reads the task counts of one workflow beside the rounds, at
// most once every countsEvery, and keeps the latest answer. The window that
// answered is remembered, so a server that cannot count the configured
// window is not asked for it every minute; after widenEvery it is asked
// again.
type countsCache struct {
	mu sync.Mutex
	// last is the latest counts read; err is the error of the latest read,
	// nil when it succeeded.
	last *taskCounts
	err  error
	// started is when the latest read started; running is closed when it
	// ends, and nil while none runs.
	started time.Time
	running chan struct{}
	// window is the window that answered last, zero before any did;
	// narrowed is when it fell short of the configured one.
	window   time.Duration
	narrowed time.Time
}

// ended is what refresh returns when no read is due: a closed channel.
var ended = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// refresh starts a read when one is due and none runs, and returns a
// channel that is closed when the read in flight, if any, ends.
func (cc *countsCache) refresh(st *workflowState, id string, now time.Time) <-chan struct{} {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.running != nil {
		return cc.running
	}
	if !cc.started.IsZero() && now.Sub(cc.started) < countsEvery {
		return ended
	}
	windows := cc.windowsFrom(st.window, now)
	cc.started = now
	done := make(chan struct{})
	cc.running = done
	st.c.pending.Add(1)
	go func() {
		defer st.c.pending.Done()
		defer close(done)
		tc, err := readCounts(st, id, windows)
		cc.mu.Lock()
		defer cc.mu.Unlock()
		cc.running = nil
		cc.err = err
		if err == nil {
			cc.last = tc
			if tc.window < windows[0] {
				cc.narrowed = tc.at
			}
			cc.window = tc.window
		}
	}()
	return done
}

// windowsFrom returns the windows a read tries, widest first: from the one
// that answered last, or from the configured one when none did yet or the
// narrower one has been kept for widenEvery.
func (cc *countsCache) windowsFrom(configured time.Duration, now time.Time) []time.Duration {
	all := []time.Duration{configured}
	for _, w := range narrowerWindows {
		if w < configured {
			all = append(all, w)
		}
	}
	if cc.window == 0 || cc.window >= configured || now.Sub(cc.narrowed) >= widenEvery {
		return all
	}
	for i, w := range all {
		if w <= cc.window {
			return all[i:]
		}
	}
	return all
}

// current returns the counts to report at now: the latest read when it
// succeeded and is recent, nil while none has answered yet, and the error
// of the latest read when it failed. Counts older than countsMaxAge are
// not reported: they would say how a day went that is no longer today.
func (cc *countsCache) current(now time.Time) (*taskCounts, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.err != nil {
		return nil, cc.err
	}
	if cc.last == nil {
		return nil, nil
	}
	if now.Sub(cc.last.at) > countsMaxAge {
		return nil, fmt.Errorf("the latest counts are from %s, the reads since have not ended", cc.last.at.Format(time.RFC3339))
	}
	return cc.last, nil
}

// readCounts reads task-metrics over each window in turn until one answers
// in time. Only a timeout moves on to a narrower window; any other error
// ends the read. A workflow the server no longer knows is forgotten, so the
// next round looks its id up again.
func readCounts(st *workflowState, id string, windows []time.Duration) (*taskCounts, error) {
	ctx, cancel := st.c.longRound(st.life, time.Duration(len(windows))*st.c.slow.Timeout)
	defer cancel()
	var tried []string
	for _, w := range windows {
		at := time.Now()
		counts, err := st.c.taskMetrics(ctx, at.Add(-w), id)
		if err == nil {
			return &taskCounts{byStatus: counts, window: w, at: at}, nil
		}
		var nf errWorkflowNotFound
		if notFound(err) || errors.As(err, &nf) {
			st.c.forgetWorkflow(st.workflow)
		}
		if !timedOut(err) || ctx.Err() != nil {
			return nil, err
		}
		tried = append(tried, w.String())
	}
	return nil, fmt.Errorf("counting over %s took longer than %s each time", strings.Join(tried, ", "), st.c.slow.Timeout)
}

// applyRuns folds the latest runs into the observation and the job:
// JobFailed when the most recent finished run failed, JobRunning while a run
// runs, latest_status, last_run, last_success and last_failure, and one job
// event per newly seen failed run.
func (p *WorkflowProbe) applyRuns(o *probe.Observation, st *workflowState, runs []taskRun, job *facet.ScheduledJobFacet) {
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
		o.Detail["last_run"] = firstOf(latestFinished.FinishedAt, latestFinished.CreatedAt)
	}
	if lastSuccess != nil {
		o.Detail["last_success"] = firstOf(lastSuccess.FinishedAt, lastSuccess.CreatedAt)
	}
	if lastFailure != nil {
		o.Detail["last_failure"] = firstOf(lastFailure.FinishedAt, lastFailure.CreatedAt)
	}
	if latestFinished != nil && latestFinished.Status == statusFailed {
		job.LastFailure = &facet.Failure{Ref: "run/" + latestFinished.runID(), At: firstOf(latestFinished.FinishedAt, latestFinished.CreatedAt), Reason: truncate(latestFinished.ErrorMessage, errorMessageLen)}
	}
	if running != nil {
		job.Running = &facet.Task{ID: "run/" + running.runID(), Name: running.DisplayName, Started: firstOf(running.StartedAt, running.CreatedAt)}
	}
}
