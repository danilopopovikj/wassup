package hatchet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	if lr, _ := o.Detail["last_run"].(time.Time); lr.Sub(now.Add(-50*time.Minute)).Abs() > time.Second {
		t.Fatalf("last_run = %v, want the finish of the latest finished run", o.Detail["last_run"])
	}
	if r := o.Metrics["rate"]; r != 43.0/(12*3600) {
		t.Fatalf("rate = %v, want the finished runs per second over 12h", r)
	}
	if o.Detail["window"] != "12h0m0s" {
		t.Fatalf("window = %v", o.Detail["window"])
	}
	if _, ok := o.Detail["window_note"]; ok {
		t.Fatal("window_note for the window that was asked for")
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
	if !strings.Contains(o.Err, `workflow "missing" not found`) || strings.Contains(o.Err, "did you mean") || o.Metrics != nil {
		t.Fatalf("observation = %+v", o)
	}
	if h := p.Health(); h.State != probe.HealthDegraded {
		t.Fatalf("health = %+v", h)
	}
}

// A workflow registered behind a namespace prefix is suggested, never
// taken in the place of the name the binding gave.
func TestWorkflowNamespacedNameIsSuggested(t *testing.T) {
	srv := serve(t, routes{
		tenantPath("/workflows"): rawJSON(`{"rows":[{"name":"bookstore_nightly-report","metadata":{"id":"wf-1"}},{"name":"other","metadata":{"id":"wf-2"}}]}`),
	})
	p := &WorkflowProbe{}
	st, err := p.setup(specFor(t, srv, map[string]any{"workflow": "nightly-report"}))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if want := `workflow "nightly-report" not found; did you mean "bookstore_nightly-report"?`; o.Err != want {
		t.Fatalf("err = %q, want %q", o.Err, want)
	}
}

// countingAPI is a fake API for hatchet.workflow whose task-metrics takes
// slow to count a window wider than fast, and counts its calls.
type countingAPI struct {
	mu    sync.Mutex
	calls []time.Duration // the window of every task-metrics call
}

func (a *countingAPI) serve(t *testing.T, fast, slow time.Duration) *httptest.Server {
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	return serve(t, routes{
		tenantPath("/workflows"): rawJSON(`{"rows":[{"name":"every-minute","metadata":{"id":"wf-1"}}]}`),
		stablePath("/task-metrics"): func(w http.ResponseWriter, r *http.Request) {
			since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
			window := time.Since(since).Round(time.Hour)
			a.mu.Lock()
			a.calls = append(a.calls, window)
			a.mu.Unlock()
			if window > fast {
				time.Sleep(slow)
			}
			rawJSON(`[{"status":"COMPLETED","count":58},{"status":"FAILED","count":1},{"status":"CANCELLED","count":1}]`)(w, r)
		},
		stablePath("/workflow-runs"): jsonOK(map[string]any{"rows": []map[string]any{
			{"status": "COMPLETED", "workflowRunExternalId": "r1", "createdAt": ts(2 * time.Minute), "finishedAt": ts(time.Minute)},
		}}),
		tenantPath("/workflows/crons"): rawJSON(`{"rows":[]}`),
	})
}

func (a *countingAPI) windows() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Duration(nil), a.calls...)
}

// The counts are read at most once a minute; the rounds in between report
// the latest read.
func TestWorkflowCountsReadOncePerMinute(t *testing.T) {
	api := &countingAPI{}
	srv := api.serve(t, 48*time.Hour, 0)
	p := &WorkflowProbe{}
	st, err := p.setup(specFor(t, srv, map[string]any{"workflow": "every-minute"}))
	if err != nil {
		t.Fatal(err)
	}
	defer st.c.close()
	for i := 0; i < 3; i++ {
		o := p.poll(context.Background(), st)
		if o.Err != "" || o.Metrics["succeeded"] != 58 || o.Detail["window"] != "24h0m0s" {
			t.Fatalf("round %d: err %q, metrics %v, detail %v", i, o.Err, o.Metrics, o.Detail)
		}
	}
	if n := len(api.windows()); n != 1 {
		t.Fatalf("task-metrics read %d times in three rounds, want once", n)
	}
}

// A server that cannot count the day in time is asked for a narrower
// window; the counts say which window they cover, and the next read starts
// from the window that answered.
func TestWorkflowCountsNarrowTheWindow(t *testing.T) {
	api := &countingAPI{}
	srv := api.serve(t, 2*time.Hour, 300*time.Millisecond)
	p := &WorkflowProbe{}
	st, err := p.setup(specFor(t, srv, map[string]any{"workflow": "every-minute"}))
	if err != nil {
		t.Fatal(err)
	}
	defer st.c.close()
	st.c.slow.Timeout = 50 * time.Millisecond
	o := p.poll(context.Background(), st)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if o.Detail["window"] != "1h0m0s" || o.Metrics["succeeded"] != 58 {
		t.Fatalf("detail %v, metrics %v", o.Detail, o.Metrics)
	}
	if r := o.Metrics["rate"]; r != 60.0/3600 {
		t.Fatalf("rate = %v, want 60 finished runs over the hour", r)
	}
	if note, _ := o.Detail["window_note"].(string); !strings.Contains(note, "the counts cover the last 1h0m0s") {
		t.Fatalf("window_note = %q", note)
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}
	if got := api.windows(); len(got) != 3 || got[0] != 24*time.Hour || got[1] != 6*time.Hour || got[2] != time.Hour {
		t.Fatalf("windows asked = %v", got)
	}

	// The next read starts from the hour that answered.
	st.counts.started = time.Now().Add(-2 * countsEvery)
	o = p.poll(context.Background(), st)
	if got := api.windows(); len(got) != 4 || got[3] != time.Hour || o.Detail["window"] != "1h0m0s" {
		t.Fatalf("windows asked = %v, detail %v", got, o.Detail)
	}
}

// Task counts that cannot be read leave the counts out and degrade the
// probe; what the runs list said is still reported.
func TestWorkflowCountsFailingKeepTheRuns(t *testing.T) {
	for name, tc := range map[string]struct {
		metrics http.HandlerFunc
		want    string
	}{
		"an error": {func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, "task counts: hatchet API"},
		"every window too slow": {func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			rawJSON(`[]`)(w, r)
		}, "counting over 24h0m0s, 6h0m0s, 1h0m0s took longer than 50ms each time"},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			srv := serve(t, routes{
				tenantPath("/workflows"):    rawJSON(`{"rows":[{"name":"sync","metadata":{"id":"wf-1"}}]}`),
				stablePath("/task-metrics"): tc.metrics,
				stablePath("/workflow-runs"): jsonOK(map[string]any{"rows": []map[string]any{
					{"status": "COMPLETED", "workflowRunExternalId": "r1", "createdAt": now.Add(-2 * time.Minute).Format(time.RFC3339Nano)},
				}}),
				tenantPath("/workflows/crons"): rawJSON(`{"rows":[]}`),
			})
			p := &WorkflowProbe{}
			st, err := p.setup(specFor(t, srv, map[string]any{"workflow": "sync"}))
			if err != nil {
				t.Fatal(err)
			}
			defer st.c.close()
			st.c.slow.Timeout = 50 * time.Millisecond
			o := p.poll(context.Background(), st)
			if o.Err != "" {
				t.Fatalf("err = %q", o.Err)
			}
			for _, k := range []string{"succeeded", "failed", "active", "queued", "cancelled", "rate"} {
				if _, ok := o.Metrics[k]; ok {
					t.Errorf("%s reported although the counts were not read", k)
				}
			}
			if _, ok := o.Detail["window"]; ok {
				t.Error("window claimed for counts that were not read")
			}
			if o.Detail["latest_status"] != "COMPLETED" {
				t.Errorf("latest_status = %v", o.Detail["latest_status"])
			}
			if lr, _ := o.Detail["last_run"].(time.Time); lr.IsZero() {
				t.Errorf("last_run = %v, want the created time of a run with no finish", o.Detail["last_run"])
			}
			if h := p.Health(); h.State != probe.HealthDegraded || !strings.Contains(h.Message, tc.want) {
				t.Errorf("health = %+v, want degraded with %q", h, tc.want)
			}
		})
	}
}
