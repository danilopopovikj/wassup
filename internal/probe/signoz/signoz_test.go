package signoz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// fakeSigNoz answers the three requests the probe sends and records them.
type fakeSigNoz struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	signIns  int
	queries  []queryRequest
	others   []string
	token    string              // the token a sign-in hands out
	accepted string              // the token a query has to carry; "" accepts token
	rows     []series            // what SigNoz holds; an answer groups it by the labels of the query
	day      []series            // what it holds of the last day, when that is more; nil is rows
	byMetric map[string][]series // what it holds per metric, when set
	status   int                 // the status of a query, when not 200
}

// series is one series SigNoz holds, counting at an even rate over every
// step of a query. A rate that is nil is one SigNoz does not know. until,
// when set, is when it stopped counting: no step after it holds a count.
type series struct {
	labels map[string]string
	rate   any
	until  time.Time
}

// of makes a series from pairs of label and value and a rate.
func of(rate any, pairs ...string) series {
	s := series{labels: map[string]string{}, rate: rate}
	for i := 0; i+1 < len(pairs); i += 2 {
		s.labels[pairs[i]] = pairs[i+1]
	}
	return s
}

// grouped answers the way SigNoz does: one series per combination of the
// labels asked for, the counts of its series added up step by step.
func grouped(all []series, by []string, q queryRequest) []any {
	stepMs := int64(q.CompositeQuery.Queries[0].Spec.StepInterval) * 1000
	type group struct {
		labels []any
		values map[int64]float64
	}
	var order []string
	groups := map[string]*group{}
	for _, s := range all {
		rate, ok := s.rate.(float64)
		if !ok {
			continue // SigNoz knows no value of it
		}
		var labels []any
		var key []string
		for _, l := range by {
			if v, ok := s.labels[l]; ok {
				labels = append(labels, map[string]any{"key": map[string]any{"name": l}, "value": v})
				key = append(key, l+"="+v)
			}
		}
		k := strings.Join(key, ",")
		g := groups[k]
		if g == nil {
			g = &group{labels: labels, values: map[int64]float64{}}
			groups[k] = g
			order = append(order, k)
		}
		for ts := q.Start - q.Start%stepMs; ts < q.End; ts += stepMs {
			if !s.until.IsZero() && ts >= s.until.UnixMilli() {
				break
			}
			from, to := max(ts, q.Start), min(ts+stepMs, q.End)
			g.values[ts] += rate * float64(to-from) / 1000
		}
	}
	out := []any{}
	for _, k := range order {
		g := groups[k]
		var ts []int64
		for t := range g.values {
			ts = append(ts, t)
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
		values := []any{}
		for _, t := range ts {
			if g.values[t] != 0 {
				values = append(values, map[string]any{"timestamp": t, "value": g.values[t]})
			}
		}
		if g.labels == nil {
			g.labels = []any{}
		}
		out = append(out, map[string]any{"labels": g.labels, "values": values})
	}
	return out
}

func newFake(t *testing.T) *fakeSigNoz {
	f := &fakeSigNoz{t: t, token: "session-1"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Setenv("SIGNOZ_USER", "reader@bookstore.example")
	t.Setenv("SIGNOZ_PASSWORD", "open-sesame")
	return f
}

func (f *fakeSigNoz) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == contextPath:
		if got := r.URL.Query().Get("email"); got != "reader@bookstore.example" {
			f.t.Errorf("the sign-in asks for the user %q", got)
		}
		io.WriteString(w, `{"status":"success","data":{"exists":true,"orgs":[{"id":"org-1","name":"bookstore"}]}}`)
	case r.Method == http.MethodPost && r.URL.Path == loginPath:
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["password"] != "open-sesame" || body["orgId"] != "org-1" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"status":"error","error":{"code":"unauthenticated","message":"wrong password"}}`)
			return
		}
		f.signIns++
		io.WriteString(w, `{"status":"success","data":{"accessToken":"`+f.token+`","refreshToken":"r"}}`)
	case r.Method == http.MethodPost && r.URL.Path == queryPath:
		want := f.accepted
		if want == "" {
			want = f.token
		}
		if r.Header.Get("Authorization") != "Bearer "+want && r.Header.Get(keyHeader) != want {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"status":"error","error":{"code":"unauthenticated","message":"token expired"}}`)
			return
		}
		var q queryRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			f.t.Errorf("the query is not JSON: %v", err)
		}
		f.queries = append(f.queries, q)
		if f.status != 0 {
			w.WriteHeader(f.status)
			io.WriteString(w, `{"status":"error","error":{"code":"internal","message":"the database is busy"}}`)
			return
		}
		var by []string
		for _, g := range q.CompositeQuery.Queries[0].Spec.GroupBy {
			by = append(by, g.Name)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"type": "time_series", "data": map[string]any{"results": []any{map[string]any{
				"queryName": "A", "aggregations": []any{map[string]any{"index": 0, "series": grouped(f.held(q), by, q)}},
			}}},
		}})
	default:
		f.others = append(f.others, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// held is what SigNoz holds of the time a query asks for.
func (f *fakeSigNoz) held(q queryRequest) []series {
	if f.byMetric != nil {
		return f.byMetric[q.CompositeQuery.Queries[0].Spec.Aggregations[0].MetricName]
	}
	if f.day != nil && time.Duration(q.End-q.Start)*time.Millisecond > time.Hour {
		return f.day
	}
	return f.rows
}

func (f *fakeSigNoz) counts() (signIns, queries int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signIns, len(f.queries)
}

// spec is a binding on the calls of the api to the payment provider.
func (f *fakeSigNoz) spec(target string, more map[string]any) map[string]any {
	s := map[string]any{
		"url": f.srv.URL, "metric": "signoz_external_call_latency_count",
		"_target": target, "_tick": 20 * time.Millisecond,
	}
	for k, v := range more {
		s[k] = v
	}
	return s
}

// newStore is a store of its own, so that a test shares nothing with another.
func newStore() *tableStore {
	return &tableStore{slots: map[string]*slot{}, now: time.Now, settle: 20 * time.Millisecond, ask: ask}
}

// first starts a probe and returns its first observation.
func first(t *testing.T, p *edgeProbe, spec map[string]any) probe.Observation {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 16)
	if err := p.Validate(spec); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := p.Start(ctx, spec, out); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case o := <-out:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("no observation")
	}
	return probe.Observation{}
}

func TestTheGuardLetsTheSignInAndTheQueryThroughAndNothingElse(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
	}))
	defer srv.Close()
	c := newHTTPClient()

	for _, path := range []string{loginPath, queryPath, "/signoz" + queryPath} {
		resp, err := c.Post(srv.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
	}
	resp, err := c.Get(srv.URL + contextPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	sent := len(seen)

	refused := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/dashboards"},
		{http.MethodPost, "/api/v1/user"},
		{http.MethodPost, "/api/v2/sessions/rotate"},
		{http.MethodPost, contextPath},
		{http.MethodPut, queryPath},
		{http.MethodPatch, loginPath},
		{http.MethodDelete, "/api/v2/sessions"},
	}
	for _, r := range refused {
		req, _ := http.NewRequest(r.method, srv.URL+r.path, strings.NewReader("{}"))
		if _, err := c.Do(req); !errors.Is(err, probe.ErrReadOnly) {
			t.Errorf("%s %s: err = %v, want ErrReadOnly", r.method, r.path, err)
		}
	}
	if len(seen) != sent {
		t.Errorf("the server saw %v: a refused request was sent", seen[sent:])
	}
}

func TestARateIsTheSumOfTheSeriesThatMatch(t *testing.T) {
	f := newFake(t)
	f.rows = []series{
		of(1.5, "address", "api.payments.example:443", "service.name", "api", "status.code", "STATUS_CODE_UNSET"),
		of(0.5, "address", "api.payments.example:443", "service.name", "api", "status.code", "STATUS_CODE_ERROR"),
		of(7.0, "address", "api.payments.example:443", "service.name", "worker", "status.code", "STATUS_CODE_UNSET"),
		of(3.0, "address", "api.mail.example:443", "service.name", "api", "status.code", "STATUS_CODE_UNSET"),
		of(nil, "address", "api.mail.example:443", "service.name", "api", "status.code", "STATUS_CODE_ERROR"),
	}
	p := &edgeProbe{tables: newStore()}
	o := first(t, p, f.spec("api->payments", map[string]any{
		"match":  map[string]any{"service.name": "api", "address": `api\.payments\.example.*`},
		"errors": map[string]any{"status.code": "STATUS_CODE_ERROR"},
	}))
	if o.Err != "" {
		t.Fatalf("err: %s", o.Err)
	}
	if o.Target != "api->payments" || o.Probe != kindEdge {
		t.Errorf("target %q probe %q", o.Target, o.Probe)
	}
	if got := o.Metrics["rate"]; got != 2 {
		t.Errorf("rate = %v, want 2 (1.5 + 0.5)", got)
	}
	if got := o.Metrics["error_rate"]; got != 25 {
		t.Errorf("error_rate = %v, want 25", got)
	}
	if o.Detail["series"] != 2 {
		t.Errorf("series = %v, want 2", o.Detail["series"])
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Errorf("health = %+v", h)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) != 1 {
		t.Fatalf("%d queries, want 1", len(f.queries))
	}
	q := f.queries[0]
	spec := q.CompositeQuery.Queries[0].Spec
	if q.RequestType != "time_series" || spec.Signal != "metrics" || spec.Aggregations[0].TimeAggregation != "increase" ||
		spec.Aggregations[0].SpaceAggregation != "sum" || spec.Aggregations[0].MetricName != "signoz_external_call_latency_count" {
		t.Errorf("the query: %+v", q)
	}
	if got := q.End - q.Start; got != (5 * time.Minute).Milliseconds() {
		t.Errorf("the window is %d ms, want five minutes", got)
	}
	var by []string
	for _, g := range spec.GroupBy {
		by = append(by, g.Name)
	}
	if strings.Join(by, ",") != "address,service.name,status.code" {
		t.Errorf("grouped by %v", by)
	}
}

func TestWithoutErrorsNoErrorRateIsReported(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(4.0, "service.name", "api")}
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("api->db", map[string]any{"match": map[string]any{"service.name": "api"}}))
	if _, ok := o.Metrics["error_rate"]; ok {
		t.Errorf("error_rate = %v: nothing said which series are errors", o.Metrics["error_rate"])
	}
	if o.Metrics["rate"] != 4 {
		t.Errorf("rate = %v", o.Metrics["rate"])
	}
}

// What matches nothing was counted by nobody. That is not a rate of zero:
// the binding reports no rate and says what SigNoz holds instead.
func TestNothingMatchesReportsNoRateAndSaysWhatThereIs(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(3.0, "address", "api.mail.example:443"), of(9.0, "address", "db.bookstore:5432"), of(1.0, "address", "db.bookstore:5432")}
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("api->payments", map[string]any{"match": map[string]any{"address": "api.payments.example:443"}}))
	if o.Err != "" {
		t.Fatalf("err: %s", o.Err)
	}
	if r, ok := o.Metrics["rate"]; ok {
		t.Errorf("rate = %v: nothing was read that could be a rate", r)
	}
	if note, _ := o.Detail["match_note"].(string); !strings.Contains(note, "no series") {
		t.Errorf("match_note = %q", note)
	}
	// what SigNoz holds instead, the busiest first
	seen, _ := o.Detail["label_values"].(map[string][]string)
	if got := strings.Join(seen["address"], " "); got != "db.bookstore:5432 api.mail.example:443" {
		t.Errorf("label_values = %v", o.Detail["label_values"])
	}
}

func TestAMetricSigNozDoesNotHoldIsNoData(t *testing.T) {
	f := newFake(t)
	f.rows = nil
	p := &edgeProbe{tables: newStore()}
	o := first(t, p, f.spec("api->payments", nil))
	if !strings.Contains(o.Err, "holds no value of signoz_external_call_latency_count") {
		t.Errorf("err = %q", o.Err)
	}
	if len(o.Metrics) != 0 {
		t.Errorf("metrics = %v: a read that found nothing reports nothing", o.Metrics)
	}
	if h := p.Health(); h.State != probe.HealthDegraded {
		t.Errorf("health = %+v", h)
	}
}

func TestTheBindingsOfOneMetricShareOneSignInAndOneQuery(t *testing.T) {
	f := newFake(t)
	f.rows = []series{
		of(2.0, "address", "api.payments.example:443", "service.name", "api"),
		of(3.0, "address", "api.mail.example:443", "service.name", "api"),
		of(1.0, "address", "api.mail.example:443", "service.name", "worker"),
	}
	store := newStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 256)
	specs := []map[string]any{
		f.spec("api->payments", map[string]any{"match": map[string]any{"address": "api.payments.example:443"}}),
		f.spec("api->mail", map[string]any{"match": map[string]any{"address": "api.mail.example:443", "service.name": "api"}}),
		f.spec("worker->mail", map[string]any{"match": map[string]any{"address": "api.mail.example:443", "service.name": "worker"}}),
	}
	// every binding says what it matches on before one of them asks
	for _, s := range specs {
		p := &edgeProbe{tables: store}
		if err := p.Start(ctx, s, out); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]float64{}
	deadline := time.After(5 * time.Second)
	for n := 0; n < 30; n++ { // ten rounds of three
		select {
		case o := <-out:
			if o.Err != "" {
				t.Fatalf("%s: %s", o.Target, o.Err)
			}
			got[o.Target] = o.Metrics["rate"]
		case <-deadline:
			t.Fatal("the probes stopped reporting")
		}
	}
	want := map[string]float64{"api->payments": 2, "api->mail": 3, "worker->mail": 1}
	for id, r := range want {
		if got[id] != r {
			t.Errorf("%s = %v, want %v", id, got[id], r)
		}
	}
	signIns, queries := f.counts()
	if signIns != 1 {
		t.Errorf("%d sign-ins, want 1", signIns)
	}
	if queries != 1 {
		t.Errorf("%d queries for three bindings and thirty observations, want 1", queries)
	}
	if len(f.others) != 0 {
		t.Errorf("other requests: %v", f.others)
	}
}

func TestATableIsAskedForAgainAfterTheInterval(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(4.0, "service.name", "api")}
	now := time.Now()
	store := newStore()
	store.now = func() time.Time { return now }
	cfg, err := edgeConfigOf(f.spec("api->db", map[string]any{"match": map[string]any{"service.name": "api"}, "interval": "1m"}))
	if err != nil {
		t.Fatal(err)
	}
	sess := sessionFor(cfg.base, cfg.creds)
	read := func() {
		t.Helper()
		if _, err := store.read(context.Background(), sess, cfg); err != nil {
			t.Fatal(err)
		}
	}
	read()
	now = now.Add(59 * time.Second)
	read()
	if _, q := f.counts(); q != 1 {
		t.Fatalf("%d queries within the interval, want 1", q)
	}
	now = now.Add(2 * time.Second)
	read()
	if _, q := f.counts(); q != 2 {
		t.Fatalf("%d queries after the interval, want 2", q)
	}
	// a binding that matches on a label the table is not grouped by
	wider := cfg
	wider.labels = []string{"address", "service.name"}
	if _, err := store.read(context.Background(), sess, wider); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.queries[len(f.queries)-1].CompositeQuery.Queries[0].Spec.GroupBy
	var by []string
	for _, g := range last {
		by = append(by, g.Name)
	}
	sort.Strings(by)
	if len(f.queries) != 3 || strings.Join(by, ",") != "address,service.name" {
		t.Errorf("%d queries, the last grouped by %v: a new label takes a new query grouped by all of them", len(f.queries), by)
	}
}

func TestAnErrorIsKeptAsLongAsAnAnswer(t *testing.T) {
	f := newFake(t)
	f.status = http.StatusInternalServerError
	store := newStore()
	cfg, err := edgeConfigOf(f.spec("api->db", nil))
	if err != nil {
		t.Fatal(err)
	}
	sess := sessionFor(cfg.base, cfg.creds)
	for i := 0; i < 5; i++ {
		_, err := store.read(context.Background(), sess, cfg)
		if err == nil || !strings.Contains(err.Error(), "HTTP 500: the database is busy") {
			t.Fatalf("err = %v", err)
		}
		if misconfigured(err) {
			t.Errorf("a SigNoz in trouble is not a binding that is wrong")
		}
	}
	if _, q := f.counts(); q != 1 {
		t.Errorf("%d queries to a SigNoz that answers with an error, want 1 per interval", q)
	}
}

func TestASessionThatRanOutIsAskedForOnceMore(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(4.0, "service.name", "api")}
	store := newStore()
	cfg, err := edgeConfigOf(f.spec("api->db", map[string]any{"match": map[string]any{"service.name": "api"}, "interval": "1ms"}))
	if err != nil {
		t.Fatal(err)
	}
	sess := sessionFor(cfg.base, cfg.creds)
	if _, err := store.read(context.Background(), sess, cfg); err != nil {
		t.Fatal(err)
	}
	// SigNoz forgets the session and hands out another one from now on.
	f.mu.Lock()
	f.token = "session-2"
	f.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	if _, err := store.read(context.Background(), sess, cfg); err != nil {
		t.Fatalf("after the session ran out: %v", err)
	}
	if signIns, _ := f.counts(); signIns != 2 {
		t.Errorf("%d sign-ins, want 2", signIns)
	}

	// A token SigNoz never takes is asked for once, not for ever.
	f.mu.Lock()
	f.accepted = "none"
	f.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	_, err = store.read(context.Background(), sess, cfg)
	if err == nil || !misconfigured(err) {
		t.Errorf("err = %v, want the refusal", err)
	}
	if signIns, _ := f.counts(); signIns != 3 {
		t.Errorf("%d sign-ins, want 3: one more try and no loop", signIns)
	}
}

func TestAnAPIKeyTakesTheSignInOut(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(4.0, "service.name", "api")}
	f.accepted = "key-of-the-reader"
	t.Setenv("BOOKSTORE_SIGNOZ_KEY", "key-of-the-reader")
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("api->db", map[string]any{
		"token_env": "BOOKSTORE_SIGNOZ_KEY", "match": map[string]any{"service.name": "api"},
	}))
	if o.Err != "" || o.Metrics["rate"] != 4 {
		t.Errorf("err %q rate %v", o.Err, o.Metrics["rate"])
	}
	if signIns, _ := f.counts(); signIns != 0 {
		t.Errorf("%d sign-ins with a key", signIns)
	}
}

func TestCredentialsThatAreMissingOrWrongSayWhichAndNotWhat(t *testing.T) {
	f := newFake(t)
	t.Setenv("SIGNOZ_PASSWORD", "")
	p := &edgeProbe{tables: newStore()}
	o := first(t, p, f.spec("api->db", nil))
	if !strings.Contains(o.Err, "SIGNOZ_PASSWORD (password_env) is not set") {
		t.Errorf("err = %q", o.Err)
	}
	if h := p.Health(); h.State != probe.HealthFailed {
		t.Errorf("health = %+v, want failed: asking again does not help", h)
	}

	t.Setenv("SIGNOZ_PASSWORD", "not-the-password")
	p = &edgeProbe{tables: newStore()}
	o = first(t, p, f.spec("api->mail", nil))
	if !strings.Contains(o.Err, "HTTP 401") {
		t.Errorf("err = %q", o.Err)
	}
	for _, secret := range []string{"not-the-password", "reader@bookstore.example"} {
		if strings.Contains(o.Err, secret) || strings.Contains(p.Health().Message, secret) {
			t.Errorf("the error names %q: %s", secret, o.Err)
		}
	}
	if h := p.Health(); h.State != probe.HealthFailed {
		t.Errorf("health = %+v, want failed", h)
	}
}

func TestValidate(t *testing.T) {
	ok := map[string]any{"url": "https://signoz.bookstore.example", "metric": "signoz_calls_total"}
	with := func(k string, v any) map[string]any {
		s := map[string]any{}
		for kk, vv := range ok {
			s[kk] = vv
		}
		if v == nil {
			delete(s, k)
		} else {
			s[k] = v
		}
		return s
	}
	cases := []struct {
		name string
		spec map[string]any
		want string // part of the error; "" is valid
	}{
		{"the least a binding says", ok, ""},
		{"everything", map[string]any{"url": "http://signoz.signoz:8080/", "metric": "signoz_calls_total",
			"match": map[string]any{"service.name": "api"}, "errors": map[string]any{"status.code": "STATUS_CODE_ERROR"},
			"window": "10m", "interval": "2m", "user_env": "U", "password_env": "P"}, ""},
		{"no url", with("url", nil), `"url" is required`},
		{"no metric", with("metric", nil), `"metric" is required`},
		{"a url that is a host", with("url", "signoz.bookstore.example"), "http(s) address"},
		{"a password in the url", with("url", "https://reader:secret@signoz.bookstore.example"), "must not carry a user"},
		{"labels in the metric", with("metric", `signoz_calls_total{service="api"}`), "labels go into match"},
		{"an expression that does not compile", with("match", map[string]any{"address": "("}), "the expression for label"},
		{"a match that is a string", with("match", "api"), "must map a label"},
		{"a window that is a number", with("window", 5), "must be a duration"},
		{"an interval that is no duration", with("interval", "often"), "is not a duration"},
		{"a day that is a week", with("known", "168h"), ""},
		{"a day that is left out", with("known", "0s"), ""},
		{"a day that is no duration", with("known", "yesterday"), "is not a duration"},
	}
	p := &edgeProbe{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := p.Validate(c.spec)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("err = %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("err = %v, want %q", err, c.want)
			case err != nil && strings.Contains(err.Error(), "secret"):
				t.Errorf("the error repeats the password: %v", err)
			}
		})
	}
	cfg, err := edgeConfigOf(with("window", "30s"))
	if err != nil || cfg.window != minWindow {
		t.Errorf("window = %v err %v, want the shortest a rate can be taken over", cfg.window, err)
	}
	if cfg.base != "https://signoz.bookstore.example" || cfg.interval != edgeInterval || cfg.known != edgeKnown {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestTheAnswerIsReadSeriesBySeries(t *testing.T) {
	var a queryAnswer
	raw := `{"type":"time_series","data":{"results":[{"queryName":"A","aggregations":[{"index":0,"series":[
		{"labels":[{"key":{"name":"service.name"},"value":"api"},{"key":{"name":"address"},"value":"db:5432"}],
		 "values":[{"timestamp":1000000,"value":30},{"timestamp":1060000,"value":0},{"timestamp":1120000,"value":45}]},
		{"labels":[{"key":{"name":"service.name"},"value":"api"},{"key":{"name":"address"},"value":"cache:6379"}],"values":[]},
		{"labels":[{"key":{"name":"service.name"},"value":"worker"}],
		 "values":[{"timestamp":1000000,"value":6}]}]}]}]}}`
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	end := time.UnixMilli(1180000)
	tab, err := tableOf(a, []string{"address", "service.name"}, 3*time.Minute, end, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(tab.rows) != 2 {
		t.Fatalf("rows = %+v, want the two SigNoz holds values of", tab.rows)
	}
	r := tab.rows[0]
	if r.count != 75 || r.value != 75.0/180 || r.labels["service.name"] != "api" || r.labels["address"] != "db:5432" {
		t.Errorf("row 0 = %+v, want 75 over three minutes", r)
	}
	if !r.last.Equal(end) {
		t.Errorf("last = %v, want the end of the step that counted last, %v", r.last, end)
	}
	if r := tab.rows[1]; r.count != 6 || r.labels["address"] != "" || !r.last.Equal(time.UnixMilli(1060000)) {
		t.Errorf("row 1 = %+v: a series without the label has an empty one", r)
	}
}

// A service that is called in a few minutes of an hour is called at the
// rate its calls make over the hour, not at the rate of the minutes it was
// called in: SigNoz's own average of a rate reads the second, many times
// over.
func TestASparseSeriesReadsItsCountOverTheWindow(t *testing.T) {
	var a queryAnswer
	raw := `{"data":{"results":[{"aggregations":[{"series":[{"labels":[],
		"values":[{"timestamp":0,"value":6},{"timestamp":600000,"value":6},{"timestamp":1200000,"value":6}]}]}]}]}}`
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	tab, err := tableOf(a, nil, time.Hour, time.UnixMilli(3600000), 60)
	if err != nil {
		t.Fatal(err)
	}
	if got := tab.rows[0].value; got != 18.0/3600 {
		t.Errorf("rate = %v, want 18 calls over an hour (0.005/s), not the 0.1/s of the minutes they came in", got)
	}
}

// A quiet binding says when it was last counted, from the day, or from the
// week when the day holds nothing of it.
func TestAQuietBindingSaysWhenItWasLastCounted(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(4.0, "address", "db.bookstore:5432")}
	stopped := time.Now().Add(-3 * time.Hour).Truncate(time.Hour)
	f.day = []series{of(4.0, "address", "db.bookstore:5432"), {labels: map[string]string{"address": "api.mail.example:443"}, rate: 0.01, until: stopped}}
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("api->mail", map[string]any{"match": map[string]any{"address": `api\.mail\..*`}}))
	if o.Err != "" {
		t.Fatalf("err: %s", o.Err)
	}
	if o.Metrics["rate"] != 0 {
		t.Errorf("rate = %v, want 0: counted within the day, not within the window", o.Metrics["rate"])
	}
	last, err := time.Parse(time.RFC3339, fmt.Sprint(o.Detail["last_seen"]))
	if err != nil || !last.Equal(stopped) {
		t.Errorf("last_seen = %v, want %v, the end of the last step that counted", o.Detail["last_seen"], stopped)
	}
	if o.Detail["count"] != 0.0 {
		t.Errorf("count = %v, want 0 in the window", o.Detail["count"])
	}
}

func TestTheUnitOfACountIsSaid(t *testing.T) {
	for _, c := range []struct {
		metric, unit, want string
	}{
		{"signoz_external_call_latency_count", "", "req/s"},
		{"signoz_db_latency_count", "", "queries/s"},
		{"signoz_calls_total", "spans", "spans/s"},
		{"signoz_calls_total", "spans/s", "spans/s"},
	} {
		spec := map[string]any{"url": "https://signoz.bookstore.example", "metric": c.metric}
		if c.unit != "" {
			spec["unit"] = c.unit
		}
		cfg, err := edgeConfigOf(spec)
		if err != nil || cfg.unit != c.want {
			t.Errorf("%s unit %q: %q %v, want %q", c.metric, c.unit, cfg.unit, err, c.want)
		}
	}
	if _, err := edgeConfigOf(map[string]any{"url": "https://s.example", "metric": "m", "unit": "Req per sec"}); err == nil {
		t.Error("a unit that is not a word was taken")
	}
}

// A binding that is stopped while it asks leaves nothing behind: the next
// one asks again instead of reading the other's "context canceled".
func TestABindingThatWasStoppedLeavesNoErrorForTheOthers(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(4.0, "service.name", "api")}
	store := newStore()
	cfg, err := edgeConfigOf(f.spec("api->db", map[string]any{"match": map[string]any{"service.name": "api"}}))
	if err != nil {
		t.Fatal(err)
	}
	sess := sessionFor(cfg.base, cfg.creds)
	// the first read of a metric waits a moment for the other bindings, so a
	// read that is its own first has a table to stand on
	if _, err := store.read(context.Background(), sess, cfg); err != nil {
		t.Fatal(err)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	later := cfg
	later.interval = time.Nanosecond // asks again
	if _, err := store.read(stopped, sess, later); err == nil {
		t.Fatal("a read on a context that is done succeeded")
	}
	tab, err := store.read(context.Background(), sess, cfg)
	if err != nil {
		t.Fatalf("the binding after the one that was stopped: %v", err)
	}
	if len(tab.rows) != 1 {
		t.Errorf("rows = %+v", tab.rows)
	}
}

// On a database the probe reports its transactions a second and nothing
// an edge would: the share of errors is no measure of a database.
func TestOnADatabaseOnlyTheRateIsReported(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(56.0, "k8s.pod.name", "bookstore-db-1"), of(3.9, "k8s.pod.name", "bookstore-db-2")}
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("db-primary", map[string]any{
		"metric": "cnpg_pg_stat_database_xact_commit",
		"match":  map[string]any{"k8s.pod.name": "bookstore-db-1"},
		"errors": map[string]any{"k8s.pod.name": "bookstore-db-1"},
	}))
	if o.Err != "" {
		t.Fatalf("err: %s", o.Err)
	}
	if len(o.Metrics) != 1 || o.Metrics["rate"] != 56 {
		t.Errorf("metrics = %v, want the rate alone", o.Metrics)
	}
	if len(o.Conditions) != 0 {
		t.Errorf("conditions = %+v", o.Conditions)
	}
}

// A service that is called now and then has no series in a window in which
// nobody called it. That it was counted within the last day says the
// counter is there and did not move: a rate of zero, which was measured.
func TestWhatWasCountedTodayAndNotNowReadsARateOfZero(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(3.0, "address", "api.mail.example:443")}
	f.day = []series{of(0.2, "address", "api.mail.example:443"), of(0.001, "address", "api.payments.example:443", "http.status_code", "200")}
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("api->payments", map[string]any{
		"match": map[string]any{"address": "api.payments.example:443"}, "errors": map[string]any{"http.status_code": "5.."}}))
	if o.Err != "" {
		t.Fatalf("err: %s", o.Err)
	}
	if r, ok := o.Metrics["rate"]; !ok || r != 0 {
		t.Errorf("rate = %v, %v: the counter is there and did not move", r, ok)
	}
	if e, ok := o.Metrics["error_rate"]; !ok || e != 0 {
		t.Errorf("error_rate = %v, %v", e, ok)
	}
	if note, _ := o.Detail["quiet_note"].(string); !strings.Contains(note, "24 h") || !strings.Contains(note, "5 min") {
		t.Errorf("quiet_note = %q", note)
	}
	if _, ok := o.Detail["match_note"]; ok {
		t.Errorf("the match found its series: %v", o.Detail["match_note"])
	}
	// the last day is asked for in steps of five minutes: close enough to
	// say when it was last counted, few enough to keep the answer small
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) != 2 {
		t.Fatalf("queries = %d, want the window and the day", len(f.queries))
	}
	day := f.queries[1]
	if got := time.Duration(day.End-day.Start) * time.Millisecond; got != 24*time.Hour {
		t.Errorf("the second query covers %s", got)
	}
	if got := day.CompositeQuery.Queries[0].Spec.StepInterval; got != 300 {
		t.Errorf("step = %d s", got)
	}
}

// What matches is asked for once: the day is only read when the window
// holds nothing of the binding.
func TestTheDayIsNotReadWhileTheWindowHoldsTheSeries(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(3.0, "address", "api.payments.example:443")}
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("api->payments", map[string]any{"match": map[string]any{"address": "api.payments.example:443"}}))
	if o.Metrics["rate"] != 3 {
		t.Errorf("rate = %v", o.Metrics["rate"])
	}
	if _, queries := f.counts(); queries != 1 {
		t.Errorf("queries = %d", queries)
	}
}

// A match that finds nothing in the day either names a value that does not
// exist. The values listed are those of the day, which holds more of them.
func TestAMatchThatNeverFoundAnythingListsTheValuesOfTheDay(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(3.0, "address", "api.mail.example:443")}
	f.day = []series{of(3.0, "address", "api.mail.example:443"), of(0.1, "address", "api.books.example:443")}
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("api->payments", map[string]any{"match": map[string]any{"address": "api.payments.example:443"}}))
	if r, ok := o.Metrics["rate"]; ok {
		t.Errorf("rate = %v", r)
	}
	seen, _ := o.Detail["label_values"].(map[string][]string)
	if got := strings.Join(seen["address"], " "); got != "api.mail.example:443 api.books.example:443" {
		t.Errorf("label_values = %v", o.Detail["label_values"])
	}
}

// known: 0s leaves the day alone.
func TestKnownCanBeTurnedOff(t *testing.T) {
	f := newFake(t)
	f.rows = []series{of(3.0, "address", "api.mail.example:443")}
	f.day = []series{of(0.001, "address", "api.payments.example:443")}
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("api->payments", map[string]any{
		"match": map[string]any{"address": "api.payments.example:443"}, "known": "0s"}))
	if r, ok := o.Metrics["rate"]; ok {
		t.Errorf("rate = %v", r)
	}
	if _, queries := f.counts(); queries != 1 {
		t.Errorf("queries = %d", queries)
	}
}

// The transactions of a database are its commits and its rollbacks: a
// binding adds the second counter with plus.
func TestPlusAddsTheSeriesOfMoreCounters(t *testing.T) {
	f := newFake(t)
	commits := []series{of(3.0, "k8s.pod.name", "bookstore-db-1")}
	rollbacks := []series{of(11.0, "k8s.pod.name", "bookstore-db-1")}
	f.byMetric = map[string][]series{"cnpg_pg_stat_database_xact_commit": commits, "cnpg_pg_stat_database_xact_rollback": rollbacks}
	o := first(t, &edgeProbe{tables: newStore()}, f.spec("db-primary", map[string]any{
		"metric": "cnpg_pg_stat_database_xact_commit", "plus": []any{"cnpg_pg_stat_database_xact_rollback"},
		"match": map[string]any{"k8s.pod.name": "bookstore-db-1"},
	}))
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if o.Metrics["rate"] != 14 {
		t.Errorf("rate = %v, want 3 commits and 11 rollbacks a second", o.Metrics["rate"])
	}
}

// When SigNoz's collector restarts, a counter it scrapes reads its whole
// count since the pod started as the increase of one step. That step is
// left out, and the window with it: the rate is what the other steps say.
func TestAStepWhereACounterStartedOverIsLeftOut(t *testing.T) {
	var a queryAnswer
	raw := `{"data":{"results":[{"aggregations":[{"series":[{"labels":[],"values":[
		{"timestamp":0,"value":800},{"timestamp":60000,"value":640},{"timestamp":120000,"value":45405716},
		{"timestamp":180000,"value":840},{"timestamp":240000,"value":720}]}]}]}]}}`
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	tab, err := tableOf(a, nil, 5*time.Minute, time.UnixMilli(300000), 60)
	if err != nil {
		t.Fatal(err)
	}
	r := tab.rows[0]
	if r.dropped != 1 || r.count != 3000 || r.value != 3000.0/240 {
		t.Errorf("row = %+v, want 3000 over the four minutes that were read", r)
	}
	// a burst in a series that is usually quiet is not a restart
	if got := startedOver([]float64{1, 2, 1, 900}); got[3] {
		t.Error("a burst of 900 calls was taken for a counter that started over")
	}
}
