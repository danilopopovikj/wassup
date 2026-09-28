package hatchet

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestWorkflowMetricsAndJobFailed(t *testing.T) {
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	longErr := strings.Repeat("x", 200)
	var metricsQuery, runsQuery string
	srv := serve(t, routes{
		tenantPath("/workflows"): rawJSON(`{"rows":[{"name":"nightly-report","metadata":{"id":"wf-9"}}],"pagination":{"current_page":1,"num_pages":1}}`),
		stablePath("/task-metrics"): func(w http.ResponseWriter, r *http.Request) {
			metricsQuery = r.URL.RawQuery
			rawJSON(`[{"status":"COMPLETED","count":40},{"status":"FAILED","count":2},{"status":"RUNNING","count":1},{"status":"QUEUED","count":3},{"status":"CANCELLED","count":1}]`)(w, r)
		},
		stablePath("/workflow-runs"): func(w http.ResponseWriter, r *http.Request) {
			runsQuery = r.URL.RawQuery
			jsonOK(map[string]any{"rows": []map[string]any{
				{"status": "COMPLETED", "workflowRunExternalId": "r1", "createdAt": ts(3 * time.Hour), "startedAt": ts(3 * time.Hour), "finishedAt": ts(170 * time.Minute)},
				{"status": "RUNNING", "workflowRunExternalId": "r3", "displayName": "nightly-report", "createdAt": ts(5 * time.Minute), "startedAt": ts(4 * time.Minute)},
				{"status": "FAILED", "workflowRunExternalId": "r2", "createdAt": ts(time.Hour), "startedAt": ts(time.Hour), "finishedAt": ts(50 * time.Minute), "errorMessage": longErr},
			}})(w, r)
		},
		tenantPath("/workflows/crons"): rawJSON(`{"rows":[{"cron":"0 2 * * *","name":"nightly","workflowName":"nightly-report","enabled":true},{"cron":"* * * * *","name":"other","workflowName":"other","enabled":true}],"pagination":{}}`),
	})
	p := &WorkflowProbe{}
	st, err := p.setup(specFor(t, srv, map[string]any{"workflow": "nightly-report", "window": "12h"}))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	want := map[string]float64{"succeeded": 40, "failed": 2, "active": 1, "queued": 3, "cancelled": 1}
	for k, v := range want {
		if o.Metrics[k] != v {
			t.Errorf("%s = %v, want %v", k, o.Metrics[k], v)
		}
	}
	if rs := o.Metrics["running_s"]; rs < 239 || rs > 245 {
		t.Errorf("running_s = %v", rs)
	}
	if !strings.Contains(metricsQuery, "workflow_ids=wf-9") || !strings.Contains(metricsQuery, "since=") {
		t.Errorf("task-metrics query = %q", metricsQuery)
	}
	for _, part := range []string{"only_tasks=false", "workflow_ids=wf-9", "limit=20"} {
		if !strings.Contains(runsQuery, part) {
			t.Errorf("runs query %q lacks %s", runsQuery, part)
		}
	}
	jf, ok := model.HasCondition(o.Conditions, model.CondJobFailed)
	if !ok || jf.Ref != "run/r2" || jf.Since.IsZero() {
		t.Fatalf("JobFailed = %+v (%v)", jf, ok)
	}
	if len([]rune(jf.Detail)) != errorMessageLen || !strings.HasSuffix(jf.Detail, "…") {
		t.Fatalf("JobFailed detail = %q (%d)", jf.Detail, len(jf.Detail))
	}
	jr, ok := model.HasCondition(o.Conditions, model.CondJobRunning)
	if !ok || jr.Ref != "run/r3" || jr.Since.IsZero() {
		t.Fatalf("JobRunning = %+v (%v)", jr, ok)
	}
	if len(o.Events) != 1 || o.Events[0].Kind != "job" || o.Events[0].Ref != "r2" || o.Events[0].Summary != "nightly-report run failed" || o.Events[0].Target != "hatchet" {
		t.Fatalf("events = %+v", o.Events)
	}
	if o.Detail["schedule"] != "cron 0 2 * * *" || o.Detail["cron"] != "0 2 * * *" || o.Detail["workflow_id"] != "wf-9" {
		t.Fatalf("detail = %v", o.Detail)
	}
	if o.Detail["latest_status"] != "FAILED" {
		t.Fatalf("latest_status = %v", o.Detail["latest_status"])
	}
	if _, ok := o.Detail["last_success"]; !ok {
		t.Fatal("last_success missing")
	}
	if _, ok := o.Detail["last_failure"]; !ok {
		t.Fatal("last_failure missing")
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}

	// The failure event is emitted once per run id.
	o = p.poll(context.Background(), st)
	if len(o.Events) != 0 {
		t.Fatalf("event re-emitted: %+v", o.Events)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondJobFailed); !ok {
		t.Fatal("JobFailed dropped on the second poll")
	}
}

func TestWorkflowLatestSuccessClearsFailure(t *testing.T) {
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	srv := serve(t, routes{
		tenantPath("/workflows"):    rawJSON(`{"rows":[{"name":"sync","metadata":{"id":"wf-1"}}]}`),
		stablePath("/task-metrics"): rawJSON(`[{"status":"COMPLETED","count":5},{"status":"FAILED","count":1}]`),
		stablePath("/workflow-runs"): jsonOK(map[string]any{"rows": []map[string]any{
			{"status": "FAILED", "workflowRunExternalId": "old", "createdAt": ts(2 * time.Hour), "finishedAt": ts(2 * time.Hour), "errorMessage": "disk full"},
			{"status": "COMPLETED", "workflowRunExternalId": "new", "createdAt": ts(time.Hour), "finishedAt": ts(59 * time.Minute)},
		}}),
		tenantPath("/workflows/crons"): rawJSON(`{"rows":[]}`),
	})
	p := &WorkflowProbe{}
	st, err := p.setup(specFor(t, srv, map[string]any{"workflow": "sync"}))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondJobFailed); ok {
		t.Fatal("JobFailed although the latest run succeeded")
	}
	if len(o.Events) != 1 || o.Events[0].Ref != "old" {
		t.Fatalf("events = %+v", o.Events)
	}
	if o.Metrics["failed"] != 1 || o.Metrics["succeeded"] != 5 || o.Metrics["active"] != 0 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if _, ok := o.Detail["schedule"]; ok {
		t.Fatal("schedule fabricated without a cron")
	}
}

func TestWorkflowUnknownName(t *testing.T) {
	srv := serve(t, routes{
		tenantPath("/workflows"): rawJSON(`{"rows":[{"name":"other","metadata":{"id":"wf-1"}}]}`),
	})
	p := &WorkflowProbe{}
	st, err := p.setup(specFor(t, srv, map[string]any{"workflow": "missing"}))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if !strings.Contains(o.Err, `workflow "missing" not found`) || o.Metrics != nil {
		t.Fatalf("observation = %+v", o)
	}
	if h := p.Health(); h.State != probe.HealthDegraded {
		t.Fatalf("health = %+v", h)
	}
}
