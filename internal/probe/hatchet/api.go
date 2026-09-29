package hatchet

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// counts is one queue's or workflow's numbers in queue-metrics.
type counts struct {
	NumPending float64 `json:"numPending"`
	NumQueued  float64 `json:"numQueued"`
	NumRunning float64 `json:"numRunning"`
}

// Where the queue numbers were read, newest endpoint first.
const (
	sourceTaskStats = "task-stats"
	sourceQueue     = "queue-metrics"
	sourceStepRun   = "step-run-queue-metrics"
)

// taskStatusStat is the queued or the running part of one task in
// task-stats. Queues breaks the queued count down by queue; the running
// part has none. Oldest is when the oldest of these tasks was inserted.
type taskStatusStat struct {
	Total  float64            `json:"total"`
	Oldest apiTime            `json:"oldest"`
	Queues map[string]float64 `json:"queues"`
}

// taskStat is one task (a step readable id) in task-stats.
type taskStat struct {
	Queued  *taskStatusStat `json:"queued"`
	Running *taskStatusStat `json:"running"`
}

// queueMetrics is what the tenant's queues hold, read from task-stats
// (Tasks set), from queue-metrics (Total and Workflow set) or from
// step-run-queue-metrics (Queues only, legacy set). Queues is the queued
// count per queue in every case.
type queueMetrics struct {
	Queues   map[string]float64 `json:"queues"`
	Total    *counts            `json:"total"`
	Workflow map[string]counts  `json:"workflow"`
	Tasks    map[string]taskStat
	source   string
	legacy   bool
}

// shape is what a queue looks like: queued and, when the server knows them,
// pending, running and when the oldest queued task was inserted.
type shape struct {
	queued, pending, running float64
	// hasPending and hasRunning are true when the server reported them.
	hasPending, hasRunning bool
	// oldest is zero when the source does not say.
	oldest time.Time
}

// depth is the queue depth: queued plus pending.
func (s shape) depth() float64 { return s.queued + s.pending }

// add folds one task of task-stats into the shape.
func (s *shape) add(t taskStat) {
	s.hasRunning = true
	if t.Running != nil {
		s.running += t.Running.Total
	}
	if t.Queued != nil {
		s.queued += t.Queued.Total
		s.older(t.Queued.Oldest.Time)
	}
}

// older keeps the earlier of the shape's oldest time and t.
func (s *shape) older(t time.Time) {
	if !t.IsZero() && (s.oldest.IsZero() || t.Before(s.oldest)) {
		s.oldest = t
	}
}

// total is the tenant-wide shape.
func (q queueMetrics) total() shape {
	switch {
	case q.Tasks != nil:
		var s shape
		s.hasRunning = true
		for _, t := range q.Tasks {
			s.add(t)
		}
		return s
	case q.Total != nil:
		return shape{queued: q.Total.NumQueued, pending: q.Total.NumPending, running: q.Total.NumRunning, hasPending: true, hasRunning: true}
	}
	var s shape
	for _, n := range q.Queues {
		s.queued += n
	}
	return s
}

// queue is the shape of one named queue; listed is false when the server
// did not mention it (an unknown or empty queue). Running is never known
// per queue. With task-stats the oldest queued time is known only when every
// task waiting in the queue waits in no other queue: task-stats gives one
// oldest time per task, not per queue.
func (q queueMetrics) queue(name string) (s shape, listed bool) {
	n, ok := q.Queues[name]
	s.queued = n
	if q.Tasks == nil {
		return s, ok
	}
	var oldest time.Time
	for _, t := range q.Tasks {
		if t.Queued == nil {
			continue
		}
		if _, in := t.Queued.Queues[name]; !in {
			continue
		}
		if len(t.Queued.Queues) > 1 || t.Queued.Oldest.IsZero() {
			return s, ok
		}
		if oldest.IsZero() || t.Queued.Oldest.Before(oldest) {
			oldest = t.Queued.Oldest.Time
		}
	}
	s.oldest = oldest
	return s, ok
}

// workflow is the shape of one named workflow. With task-stats it is the sum
// of the tasks whose name is the workflow's or starts with it: task-stats is
// keyed by task, and a workflow's tasks carry its name, or the task of a
// single-task workflow is the workflow. Legacy servers do not report
// per-workflow numbers, which is an error rather than a zero.
func (q queueMetrics) workflow(name string) (s shape, listed bool, err error) {
	if q.Tasks != nil {
		for task, t := range q.Tasks {
			if task == name || strings.HasPrefix(task, name) {
				s.add(t)
				listed = true
			}
		}
		s.hasRunning = true
		return s, listed, nil
	}
	if q.Workflow == nil {
		return shape{}, false, fmt.Errorf("this Hatchet server has no per-workflow queue metrics (step-run-queue-metrics only)")
	}
	c, ok := q.Workflow[name]
	return shape{queued: c.NumQueued, pending: c.NumPending, running: c.NumRunning, hasPending: true, hasRunning: true}, ok, nil
}

// queueMetrics reads what the tenant's queues hold from the newest endpoint
// the server has: task-stats, else queue-metrics, else
// step-run-queue-metrics. An endpoint that is not on the server (gone) is
// skipped; any other error ends the read. queue-metrics comes before
// step-run-queue-metrics because it knows pending, running and workflows.
func (c *client) queueMetrics(ctx context.Context) (queueMetrics, error) {
	var tasks map[string]taskStat
	err := c.get(ctx, c.tenantPath("/task-stats"), nil, &tasks)
	if err == nil {
		return fromTaskStats(tasks), nil
	}
	if !gone(err) {
		return queueMetrics{}, err
	}
	var qm queueMetrics
	err = c.get(ctx, c.tenantPath("/queue-metrics"), nil, &qm)
	if err == nil {
		qm.source = sourceQueue
		return qm, nil
	}
	if !gone(err) {
		return qm, err
	}
	var legacy struct {
		Queues map[string]float64 `json:"queues"`
	}
	if err := c.get(ctx, c.tenantPath("/step-run-queue-metrics"), nil, &legacy); err != nil {
		return queueMetrics{}, err
	}
	return queueMetrics{Queues: legacy.Queues, source: sourceStepRun, legacy: true}, nil
}

// fromTaskStats builds the queue metrics of a task-stats answer: the queued
// count per queue is the sum over the tasks.
func fromTaskStats(tasks map[string]taskStat) queueMetrics {
	if tasks == nil {
		tasks = map[string]taskStat{}
	}
	qm := queueMetrics{Queues: map[string]float64{}, Tasks: tasks, source: sourceTaskStats}
	for _, t := range tasks {
		if t.Queued == nil {
			continue
		}
		for name, n := range t.Queued.Queues {
			qm.Queues[name] += n
		}
	}
	return qm
}

// perTask lists the tasks of task-stats with their queued and running
// counts, the busiest first, at most n of them.
func (q queueMetrics) perTask(n int) []map[string]any {
	type row struct {
		name            string
		queued, running float64
	}
	rows := make([]row, 0, len(q.Tasks))
	for name, t := range q.Tasks {
		var r row
		r.name = name
		if t.Queued != nil {
			r.queued = t.Queued.Total
		}
		if t.Running != nil {
			r.running = t.Running.Total
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if a, b := rows[i].queued+rows[i].running, rows[j].queued+rows[j].running; a != b {
			return a > b
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"name": r.name, "queued": r.queued, "running": r.running})
	}
	return out
}

// Worker status values.
const (
	workerActive   = "ACTIVE"
	workerInactive = "INACTIVE"
	workerPaused   = "PAUSED"
)

// worker is one row of GET /tenants/{tenant}/worker in either shape: the
// newer slotConfig map or the older maxRuns/availableRuns pair.
type worker struct {
	Metadata struct {
		ID string `json:"id"`
	} `json:"metadata"`
	Name            string   `json:"name"`
	Status          string   `json:"status"`
	LastHeartbeatAt apiTime  `json:"lastHeartbeatAt"`
	Actions         []string `json:"actions"`
	SlotConfig      map[string]struct {
		Available float64 `json:"available"`
		Limit     float64 `json:"limit"`
	} `json:"slotConfig"`
	MaxRuns       *float64 `json:"maxRuns"`
	AvailableRuns *float64 `json:"availableRuns"`
}

// slots returns the used and maximum slots of the worker; ok is false when
// the row carries no slot information at all.
func (w worker) slots() (used, max float64, ok bool) {
	if len(w.SlotConfig) > 0 {
		for _, s := range w.SlotConfig {
			max += s.Limit
			used += s.Limit - s.Available
		}
		if used < 0 {
			used = 0
		}
		return used, max, true
	}
	if w.MaxRuns != nil && w.AvailableRuns != nil {
		used = *w.MaxRuns - *w.AvailableRuns
		if used < 0 {
			used = 0
		}
		return used, *w.MaxRuns, true
	}
	return 0, 0, false
}

// active reports whether the worker is heartbeating.
func (w worker) active() bool { return w.Status == workerActive }

// serves reports whether the worker registered an action for the given
// queue or workflow name (actions are "<workflow>:<step>" ids).
func (w worker) serves(name string) bool {
	for _, a := range w.Actions {
		if strings.HasPrefix(a, name) {
			return true
		}
	}
	return false
}

// workers lists the tenant's workers.
func (c *client) workers(ctx context.Context) ([]worker, error) {
	var resp struct {
		Rows []worker `json:"rows"`
	}
	if err := c.get(ctx, c.tenantPath("/worker"), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// Task and workflow run status values (V1TaskStatus).
const (
	statusQueued    = "QUEUED"
	statusRunning   = "RUNNING"
	statusCompleted = "COMPLETED"
	statusFailed    = "FAILED"
	statusCancelled = "CANCELLED"
)

// taskRun is one row of GET /stable/tenants/{tenant}/workflow-runs: a task
// when only_tasks is set, else a workflow run (DAG or single task).
type taskRun struct {
	Status                string  `json:"status"`
	DisplayName           string  `json:"displayName"`
	CreatedAt             apiTime `json:"createdAt"`
	StartedAt             apiTime `json:"startedAt"`
	FinishedAt            apiTime `json:"finishedAt"`
	TaskExternalID        string  `json:"taskExternalId"`
	WorkflowRunExternalID string  `json:"workflowRunExternalId"`
	WorkflowID            string  `json:"workflowId"`
	ErrorMessage          string  `json:"errorMessage"`
}

// runID is the run's stable identity: the workflow run id, else the task id.
func (r taskRun) runID() string {
	if r.WorkflowRunExternalID != "" {
		return r.WorkflowRunExternalID
	}
	return r.TaskExternalID
}

// age returns how long the run has been going at now, measured from
// startedAt or, before it started, createdAt. ok is false when neither is
// known.
func (r taskRun) age(now time.Time) (time.Duration, bool) {
	start := firstOf(r.StartedAt, r.CreatedAt)
	if start.IsZero() {
		return 0, false
	}
	return now.Sub(start), true
}

// duration is finishedAt minus startedAt when both are known.
func (r taskRun) duration() (time.Duration, bool) {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0, false
	}
	return r.FinishedAt.Sub(r.StartedAt.Time), true
}

// finished reports whether the run reached a terminal status.
func (r taskRun) finished() bool {
	switch r.Status {
	case statusCompleted, statusFailed, statusCancelled:
		return true
	}
	return false
}

// runQuery is the filter of one workflow-runs read.
type runQuery struct {
	since       time.Time
	statuses    []string
	onlyTasks   bool
	workflowIDs []string
	limit       int
}

// runs lists workflow runs (or tasks) matching q, newest page first as the
// server orders them; callers sort when order matters.
func (c *client) runs(ctx context.Context, q runQuery) ([]taskRun, error) {
	v := url.Values{}
	v.Set("since", rfc3339(q.since))
	v.Set("only_tasks", strconv.FormatBool(q.onlyTasks))
	for _, s := range q.statuses {
		v.Add("statuses", s)
	}
	for _, id := range q.workflowIDs {
		v.Add("workflow_ids", id)
	}
	if q.limit > 0 {
		v.Set("limit", strconv.Itoa(q.limit))
	}
	var resp struct {
		Rows []taskRun `json:"rows"`
	}
	if err := c.get(ctx, c.stablePath("/workflow-runs"), v, &resp); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// taskMetrics reads the count of tasks per status since the given time,
// optionally for one workflow. It counts on the server and can take long on
// a busy tenant, so it runs on the slow client.
func (c *client) taskMetrics(ctx context.Context, since time.Time, workflowID string) (map[string]float64, error) {
	v := url.Values{}
	v.Set("since", rfc3339(since))
	if workflowID != "" {
		v.Set("workflow_ids", workflowID)
	}
	var rows []struct {
		Status string  `json:"status"`
		Count  float64 `json:"count"`
	}
	if err := c.getWith(ctx, c.slow, c.stablePath("/task-metrics"), v, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Status] += r.Count
	}
	return out, nil
}

// workflowRow is one row of GET /tenants/{tenant}/workflows.
type workflowRow struct {
	Name     string `json:"name"`
	Metadata struct {
		ID string `json:"id"`
	} `json:"metadata"`
}

// errWorkflowNotFound is returned by workflowID for an unknown name, with
// the workflows whose name is that name behind a namespace prefix.
type errWorkflowNotFound struct {
	name  string
	close []string
}

func (e errWorkflowNotFound) Error() string {
	msg := fmt.Sprintf("workflow %q not found", e.name)
	if len(e.close) == 0 {
		return msg
	}
	quoted := make([]string, len(e.close))
	for i, n := range e.close {
		quoted[i] = strconv.Quote(n)
	}
	return msg + "; did you mean " + strings.Join(quoted, " or ") + "?"
}

// maxCloseNames caps the names a not-found error suggests.
const maxCloseNames = 3

// closeNames returns the workflow names that are name behind a namespace
// prefix (<namespace>_<name>), sorted. A client that sets a namespace
// registers its workflows that way; the binding may have named the bare
// workflow. They are suggested, never taken in its place.
func closeNames(names map[string]string, name string) []string {
	var out []string
	for n := range names {
		if strings.HasSuffix(n, "_"+name) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if len(out) > maxCloseNames {
		out = out[:maxCloseNames]
	}
	return out
}

// workflowID resolves a workflow name to its id. Ids are cached per client;
// a miss refreshes the whole list.
func (c *client) workflowID(ctx context.Context, name string) (string, error) {
	c.mu.Lock()
	id, ok := c.workflows[name]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	rows, err := listAll[workflowRow](ctx, c, c.tenantPath("/workflows"), nil, workflowPages)
	if err != nil {
		return "", err
	}
	fresh := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.Name != "" && r.Metadata.ID != "" {
			fresh[r.Name] = r.Metadata.ID
		}
	}
	c.mu.Lock()
	c.workflows = fresh
	id, ok = fresh[name]
	c.mu.Unlock()
	if !ok {
		return "", errWorkflowNotFound{name: name, close: closeNames(fresh, name)}
	}
	return id, nil
}

// forgetWorkflow drops a cached id so the next lookup refreshes.
func (c *client) forgetWorkflow(name string) {
	c.mu.Lock()
	delete(c.workflows, name)
	c.mu.Unlock()
}

// cronRow is one row of GET /tenants/{tenant}/workflows/crons.
type cronRow struct {
	Cron         string `json:"cron"`
	Name         string `json:"name"`
	WorkflowName string `json:"workflowName"`
	Enabled      bool   `json:"enabled"`
}

// crons lists the tenant's cron triggers.
func (c *client) crons(ctx context.Context) ([]cronRow, error) {
	return listAll[cronRow](ctx, c, c.tenantPath("/workflows/crons"), nil, defaultPages)
}

// version reads the server version from /api/v1/meta.
func (c *client) version(ctx context.Context) (string, error) {
	var meta struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/api/v1/meta", nil, &meta); err != nil {
		return "", err
	}
	return meta.Version, nil
}
