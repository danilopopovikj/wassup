package k8s

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/danilopopovikj/wassup/internal/probe"
)

const routerPage = `# HELP router_requests_total How many requests were served.
# TYPE router_requests_total counter
router_requests_total{code="200",method="GET",service="shop-api-8000@kubernetes"} %d
router_requests_total{code="500",method="GET",service="shop-api-8000@kubernetes"} %d
router_requests_total{code="200",method="GET",service="shop-web-80@kubernetes"} 7 1758000000000
router_requests_tls_total{service="shop-api-8000@kubernetes"} 99999
router_open_connections{entrypoint="web"} 3
`

func TestSeriesOf(t *testing.T) {
	got, err := seriesOf([]byte(fmt.Sprintf(routerPage, 120, 3)), "router_requests_total")
	if err != nil || len(got) != 3 {
		t.Fatalf("series = %+v, %v", got, err)
	}
	if s := got[0]; s.value != 120 || s.labels["code"] != "200" || s.labels["service"] != "shop-api-8000@kubernetes" {
		t.Errorf("first series = %+v", s)
	}
	if s := got[2]; s.value != 7 {
		t.Errorf("a timestamp after the value is not the value: %+v", s)
	}

	page := "up 1\n" +
		"quoted{path=\"a \\\"b\\\" \\\\ c\\nd\",empty=\"\"} 2.5e+03\n" +
		"quoted{path=\"x\"} NaN\n" +
		"quoted{path=\"y\"} +Inf\n"
	got, err = seriesOf([]byte(page), "quoted")
	if err != nil || len(got) != 1 || got[0].value != 2500 || got[0].labels["path"] != "a \"b\" \\ c\nd" {
		t.Errorf("escapes, and values that are no number: %+v, %v", got, err)
	}
	if got, err = seriesOf([]byte(page), "up"); err != nil || len(got) != 1 || got[0].value != 1 || len(got[0].labels) != 0 {
		t.Errorf("a series without labels: %+v, %v", got, err)
	}
	for _, bad := range []string{`m{a="1"`, `m{a=1} 2`, `m{a="1} 2`, `m{a="1"} x`, `m{a="1"}`} {
		if got, err := seriesOf([]byte(bad), "m"); err == nil {
			t.Errorf("%q must not be read as %+v", bad, got)
		}
	}
}

func FuzzSeriesOf(f *testing.F) {
	f.Add([]byte(fmt.Sprintf(routerPage, 1, 2)), "router_requests_total")
	f.Add([]byte(`m{a="\"",b="\\"} 1 2`), "m")
	f.Add([]byte("m{"), "m")
	f.Fuzz(func(t *testing.T, page []byte, metric string) {
		got, err := seriesOf(page, metric)
		if err != nil && got != nil {
			t.Errorf("an error comes with no series: %+v, %v", got, err)
		}
	})
}

func TestScrapeValidate(t *testing.T) {
	good := map[string]any{"namespace": "edge", "selector": "app=router", "port": 9100, "metric": "router_requests_total",
		"match": map[string]any{"service": "shop-api-.*"}, "errors": map[string]any{"code": "5.."}, "interval": "30s"}
	p := &scrapeProbe{}
	if err := p.Validate(good); err != nil {
		t.Fatalf("good spec: %v", err)
	}
	with := func(k string, v any) map[string]any {
		out := map[string]any{}
		for key, val := range good {
			out[key] = val
		}
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	for name, spec := range map[string]map[string]any{
		"no namespace":      with("namespace", nil),
		"no selector":       with("selector", nil),
		"no port":           with("port", nil),
		"no metric":         with("metric", nil),
		"labels in metric":  with("metric", `router_requests_total{code="200"}`),
		"port out of range": with("port", 70000),
		"bad expression":    with("match", map[string]any{"service": "("}),
		"match is a string": with("match", "service=api"),
		"bad interval":      with("interval", "soon"),
		"bad scheme":        with("scheme", "ftp"),
		"relative path":     with("path", "metrics"),
		"bad selector":      with("selector", "app in"),
	} {
		if err := p.Validate(spec); err == nil {
			t.Errorf("%s: must not validate", name)
		}
	}
	cfg, err := scrapeConfigOf(with("scheme", "https"))
	if err != nil || cfg.pagePath("router-1") != "/api/v1/namespaces/edge/pods/https:router-1:9100/proxy/metrics" {
		t.Errorf("path = %q, %v", cfg.pagePath("router-1"), err)
	}
}

// router answers the GETs of a test: every read of a pod's page moves the
// clock ten seconds on and the pod's counters up. Nothing else moves the
// clock, so a test reads two pods: the read of one ages the page of the
// other.
type router struct {
	mu     sync.Mutex
	now    time.Time
	reads  map[string]int
	broken map[string]bool
	step   func(pod string, read int) (ok, failed int)
}

func (r *router) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *router) get(_ context.Context, path string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pod := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/namespaces/prod/pods/"), ":9100/proxy/metrics")
	if r.broken[pod] {
		return nil, errors.New("connection refused")
	}
	r.now = r.now.Add(10 * time.Second)
	r.reads[pod]++
	ok, failed := r.step(pod, r.reads[pod])
	return []byte(fmt.Sprintf(routerPage, ok, failed)), nil
}

// startScrape runs a k8s.scrape probe against pods named after the router
// and a store of pages on the router's clock.
func startScrape(t *testing.T, r *router, objs []runtime.Object, kv ...string) <-chan probe.Observation {
	t.Helper()
	// a probe an earlier call started may still read the router
	r.mu.Lock()
	r.now, r.reads = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC), map[string]int{}
	r.mu.Unlock()
	c := newTestClients(objs, nil, nil)
	c.proxyGet = r.get
	p := &scrapeProbe{base: base{kind: kindScrape, clients: c}, store: &pageStore{pages: map[string]*page{}, now: r.clock}}
	spec := testSpec("router->api", append([]string{"namespace", "prod", "selector", "app=api", "port", "9100", "metric", "router_requests_total"}, kv...)...)
	spec["match"] = map[string]any{"service": "shop-api-.*"}
	if m, ok := spec["match_service"].(string); ok {
		spec["match"] = map[string]any{"service": m}
		delete(spec, "match_service")
	}
	spec["errors"] = map[string]any{"code": "5.."}
	out, _ := startProbe(t, p, spec)
	return out
}

func hasRate(o probe.Observation) bool {
	_, ok := o.Metrics["rate"]
	return o.Err == "" && ok
}

func TestScrapeRateOverThePods(t *testing.T) {
	// every read is 10 s after the one before and finds 100 requests more,
	// 10 of them failed; a pod is read every 20 s, so it serves 5 a second
	r := &router{step: func(_ string, read int) (int, int) { return 1000 + 90*read, 10 * read }}
	notReady := apiPod("router-3", false, 0, nil, nil)
	out := startScrape(t, r, []runtime.Object{apiPod("router-1", true, 0, nil, nil), apiPod("router-2", true, 0, nil, nil), notReady})

	first := firstOK(t, out)
	if _, ok := first.Metrics["rate"]; ok {
		t.Errorf("one reading is no rate: %v", first.Metrics)
	}
	o := waitFor(t, out, hasRate)
	if o.Metrics["rate"] != 10 || o.Metrics["error_rate"] != 10 {
		t.Errorf("two pods at 5 a second, a tenth of it failed: %v", o.Metrics)
	}
	if pods, _ := o.Detail["pods"].([]string); len(pods) != 2 || pods[0] != "router-1" {
		t.Errorf("the pod that is not ready is not read: %v", o.Detail["pods"])
	}
	if o.Detail["series"] != 4 {
		t.Errorf("two series of the api on each of two pods, the web shop's left out: %v", o.Detail["series"])
	}
	// a page younger than the interval is not read again
	r.mu.Lock()
	reads := r.reads["router-1"]
	r.mu.Unlock()
	for i := 0; i < 3; i++ {
		if o = firstOK(t, out); o.Metrics["rate"] != 10 {
			t.Errorf("the rate holds between two readings: %v", o.Metrics)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reads["router-1"] != reads || r.reads["router-3"] != 0 {
		t.Errorf("reads = %v, want router-1 at %d and router-3 never", r.reads, reads)
	}
}

func TestScrapeCounterStartsOver(t *testing.T) {
	// the second read finds less than the first: the router started over
	r := &router{step: func(_ string, read int) (int, int) {
		if read == 1 {
			return 5000, 0
		}
		return 40 * read, 0
	}}
	out := startScrape(t, r, []runtime.Object{apiPod("router-1", true, 0, nil, nil), apiPod("router-2", true, 0, nil, nil)}, "interval", "1s", "window", "1h")
	o := waitFor(t, out, hasRate)
	// 40 more on every read, a read of a pod every 20 s, two pods
	if o.Metrics["rate"] != 4 {
		t.Errorf("the rate is taken from the readings after the counter started over: %v", o.Metrics)
	}
}

func TestScrapeLeavesOutWhatItCannotRead(t *testing.T) {
	r := &router{broken: map[string]bool{"router-2": true}, step: func(_ string, read int) (int, int) { return 100 * read, 0 }}
	out := startScrape(t, r, []runtime.Object{apiPod("router-1", true, 0, nil, nil), apiPod("router-2", true, 0, nil, nil), apiPod("router-3", true, 0, nil, nil)}, "interval", "1s")
	o := waitFor(t, out, hasRate)
	if o.Metrics["rate"] != 10 {
		t.Errorf("two pods read, 100 more every 20 s each: %v", o.Metrics)
	}
	if unread, _ := o.Detail["unread_pods"].([]string); len(unread) != 1 || unread[0] != "router-2" {
		t.Errorf("unread_pods = %v", o.Detail["unread_pods"])
	}
	if note, _ := o.Detail["unread_note"].(string); !strings.Contains(note, "connection refused") {
		t.Errorf("unread_note = %q", note)
	}

	// no pod can be read: nothing is known, which is not a rate of zero
	r = &router{broken: map[string]bool{"router-1": true}, step: func(string, int) (int, int) { return 0, 0 }}
	out = startScrape(t, r, []runtime.Object{apiPod("router-1", true, 0, nil, nil)})
	o = waitFor(t, out, func(o probe.Observation) bool { return o.Err != "" })
	if !strings.Contains(o.Err, "router-1") || !strings.Contains(o.Err, "connection refused") || len(o.Metrics) != 0 {
		t.Errorf("observation = %+v", o)
	}

	// no pod at all
	out = startScrape(t, r, nil)
	o = waitFor(t, out, func(o probe.Observation) bool { return o.Err != "" })
	if !strings.Contains(o.Err, "no ready pod") {
		t.Errorf("err = %q", o.Err)
	}
}

// What matches nothing was counted by nobody: the binding reports no rate,
// not a rate of zero, and says why.
func TestScrapeReportsNoRateWhenNothingMatches(t *testing.T) {
	r := &router{step: func(_ string, read int) (int, int) { return 100 * read, 0 }}
	out := startScrape(t, r, []runtime.Object{apiPod("router-1", true, 0, nil, nil), apiPod("router-2", true, 0, nil, nil)},
		"interval", "1s", "match_service", "shop-apii-.*")
	seen := 0
	o := waitFor(t, out, func(o probe.Observation) bool {
		seen++
		return o.Err == "" && seen >= 4 // long enough for a rate, had there been one
	})
	if _, ok := o.Metrics["rate"]; ok {
		t.Errorf("rate = %v: no series matched", o.Metrics["rate"])
	}
	if _, ok := o.Metrics["error_rate"]; ok {
		t.Errorf("error_rate = %v: no series matched", o.Metrics["error_rate"])
	}
	if note, _ := o.Detail["match_note"].(string); !strings.Contains(note, "no rate is reported") {
		t.Errorf("match_note = %q", note)
	}
}

// A binding that is stopped while it reads leaves nothing behind: the next
// one reads the page again instead of the other's "context canceled".
func TestScrapeAReadThatWasStoppedIsNotKept(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	store := &pageStore{pages: map[string]*page{}, now: func() time.Time { return now }}
	c := newTestClients(nil, nil, nil)
	reads := 0
	c.proxyGet = func(ctx context.Context, _ string) ([]byte, error) {
		reads++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []byte("router_requests_total 1\n"), nil
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.read(stopped, c, "/metrics", time.Minute); err == nil {
		t.Fatal("a read on a context that is done succeeded")
	}
	body, _, err := store.read(context.Background(), c, "/metrics", time.Minute)
	if err != nil || len(body) == 0 {
		t.Fatalf("the binding after the one that was stopped: %q %v", body, err)
	}
	if _, _, err := store.read(context.Background(), c, "/metrics", time.Minute); err != nil || reads != 2 {
		t.Errorf("reads = %d err = %v: a page that was read is kept for the interval", reads, err)
	}
}

// A match that finds nothing lists what the pages hold, busiest first, so
// the binding can be corrected.
func TestScrapeListsTheValuesWhenNothingMatches(t *testing.T) {
	r := &router{step: func(_ string, read int) (int, int) { return 100 * read, 0 }}
	out := startScrape(t, r, []runtime.Object{apiPod("router-1", true, 0, nil, nil)}, "interval", "1s", "match_service", "shop-apii-.*")
	o := firstOK(t, out)
	values, _ := o.Detail["label_values"].(map[string][]string)
	if got := values["service"]; len(got) != 2 || got[0] != "shop-api-8000@kubernetes" || got[1] != "shop-web-80@kubernetes" {
		t.Errorf("label_values = %v", o.Detail["label_values"])
	}
	if note, _ := o.Detail["match_note"].(string); !strings.Contains(note, "label_values") {
		t.Errorf("match_note = %q", note)
	}
}

// Each machine gets the rate of the pods on it, and a machine with a pod
// that could not be read is left out, never read as zero.
func TestScrapeRateByNode(t *testing.T) {
	r := &router{broken: map[string]bool{"router-4": true}, step: func(pod string, read int) (int, int) {
		if pod == "router-3" {
			return 1000 + 180*read, 20 * read // twice as busy
		}
		return 1000 + 90*read, 10 * read
	}}
	onNode := func(name, node string) runtime.Object {
		pd := apiPod(name, true, 0, nil, nil)
		pd.Spec.NodeName = node
		return pd
	}
	out := startScrape(t, r, []runtime.Object{onNode("router-1", "node-1"), onNode("router-2", "node-1"), onNode("router-3", "node-2"), onNode("router-4", "node-3")}, "interval", "1s")
	o := waitFor(t, out, func(o probe.Observation) bool { return hasRate(o) && o.Detail["rate_by_node"] != nil })
	rates, _ := o.Detail["rate_by_node"].(map[string]float64)
	errs, _ := o.Detail["error_rate_by_node"].(map[string]float64)
	if len(rates) != 2 || rates["node-1"] <= 0 || rates["node-2"] <= 0 {
		t.Fatalf("rate_by_node = %v", rates)
	}
	if _, ok := rates["node-3"]; ok {
		t.Errorf("node-3's only pod was not read: %v", rates)
	}
	if sum := rates["node-1"] + rates["node-2"]; math.Abs(sum-o.Metrics["rate"]) > 0.002 {
		t.Errorf("the machines add up to %v, the rate is %v", sum, o.Metrics["rate"])
	}
	if len(errs) != 2 || errs["node-1"] != 10 || errs["node-2"] != 10 {
		t.Errorf("error_rate_by_node = %v", errs)
	}
}

// A counter that did not move between two readings a few seconds apart says
// nothing about the window: no rate until it has been watched that long,
// then a rate of zero.
func TestScrapeZeroNeedsTheWholeWindow(t *testing.T) {
	r := &router{step: func(string, int) (int, int) { return 500, 0 }}
	out := startScrape(t, r, []runtime.Object{apiPod("router-1", true, 0, nil, nil), apiPod("router-2", true, 0, nil, nil)}, "interval", "1s", "window", "40s")
	o := waitFor(t, out, func(o probe.Observation) bool { return o.Err == "" && o.Detail["quiet_note"] != nil })
	if _, ok := o.Metrics["rate"]; ok {
		t.Errorf("a quiet span shorter than the window is no rate: %v", o.Metrics)
	}
	if _, ok := o.Metrics["error_rate"]; ok {
		t.Errorf("nor an error rate: %v", o.Metrics)
	}
	o = waitFor(t, out, hasRate)
	if o.Metrics["rate"] != 0 || o.Metrics["error_rate"] != 0 {
		t.Errorf("watched for the whole window, nothing counted: %v", o.Metrics)
	}
	if span, _ := o.Detail["span_s"].(int); span < 30 {
		t.Errorf("span_s = %v", o.Detail["span_s"])
	}
}

// A pod with one reading so far is left out of the rate and named, and its
// machine has no rate yet.
func TestScrapeNamesThePodsWithoutARate(t *testing.T) {
	p := &scrapeProbe{pods: map[string]*history{}}
	at := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	p.keep("a", reading{at: at, total: 10}, time.Minute)
	p.keep("a", reading{at: at.Add(10 * time.Second), total: 60}, time.Minute)
	p.keep("b", reading{at: at.Add(10 * time.Second), total: 60}, time.Minute)
	if _, ok := p.podRate("b", time.Minute); ok {
		t.Error("one reading is no rate")
	}
	g, ok := p.growth([]string{"a", "b"}, time.Minute)
	if !ok || g.rate != 5 || g.span != 10*time.Second || g.watched {
		t.Errorf("growth = %+v %v", g, ok)
	}
	// the counter started over: no rate until it is read again
	p.keep("a", reading{at: at.Add(20 * time.Second), total: 3}, time.Minute)
	if _, ok := p.podRate("a", time.Minute); ok {
		t.Error("a counter that started over has no rate")
	}
}
