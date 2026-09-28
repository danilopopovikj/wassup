package hatchet

import (
	"context"
	"fmt"
	"net/url"
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

// queueMetrics is GET /tenants/{tenant}/queue-metrics. On servers that only
// have step-run-queue-metrics, Total and Workflow are nil and legacy is set.
type queueMetrics struct {
	Queues   map[string]float64 `json:"queues"`
	Total    *counts            `json:"total"`
	Workflow map[string]counts  `json:"workflow"`
	legacy   bool
}

// shape is what a queue looks like: queued and, when the server knows them,
// pending and running.
type shape struct {
	queued, pending, running float64
	// known is true when pending and running were reported.
	known bool
}

// depth is the queue depth: queued plus pending.
func (s shape) depth() float64 { return s.queued + s.pending }

// total is the tenant-wide shape.
func (q queueMetrics) total() shape {
	if q.Total != nil {
		return shape{queued: q.Total.NumQueued, pending: q.Total.NumPending, running: q.Total.NumRunning, known: true}
	}
	var s shape
	for _, n := range q.Queues {
		s.queued += n
	}
	return s
}

// queue is the shape of one named queue; listed is false when the server
// did not mention it (an unknown or empty queue).
func (q queueMetrics) queue(name string) (s shape, listed bool) {
	n, ok := q.Queues[name]
	return shape{queued: n}, ok
}

// workflow is the shape of one named workflow. Legacy servers do not report
// per-workflow numbers, which is an error rather than a zero.
func (q queueMetrics) workflow(name string) (s shape, listed bool, err error) {
	if q.Workflow == nil {
		return shape{}, false, fmt.Errorf("this Hatchet server has no per-workflow queue metrics (step-run-queue-metrics only)")
	}
	c, ok := q.Workflow[name]
	return shape{queued: c.NumQueued, pending: c.NumPending, running: c.NumRunning, known: true}, ok, nil
}

// queueMetrics reads queue-metrics, falling back to step-run-queue-metrics on
// servers that predate it.
func (c *client) queueMetrics(ctx context.Context) (queueMetrics, error) {
	var qm queueMetrics
	err := c.get(ctx, c.tenantPath("/queue-metrics"), nil, &qm)
	if err == nil {
		return qm, nil
	}
	if !notFound(err) {
		return qm, err
	}
	var legacy struct {
		Queues map[string]float64 `json:"queues"`
	}
	if err := c.get(ctx, c.tenantPath("/step-run-queue-metrics"), nil, &legacy); err != nil {
		return qm, err
	}
	return queueMetrics{Queues: legacy.Queues, legacy: true}, nil
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
// optionally for one workflow.
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
	if err := c.get(ctx, c.stablePath("/task-metrics"), v, &rows); err != nil {
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

// errWorkflowNotFound is returned by workflowID for an unknown name.
type errWorkflowNotFound string

func (e errWorkflowNotFound) Error() string { return fmt.Sprintf("workflow %q not found", string(e)) }

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
		return "", errWorkflowNotFound(name)
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
