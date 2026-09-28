package k8s

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

const kindScrape = "k8s.scrape"

const (
	// scrapeInterval is how often a metrics page is read when the binding
	// does not say. A page is a few hundred kilobytes and is read through
	// the API server, so it is read less often than the diagram ticks.
	scrapeInterval = 15 * time.Second
	// scrapeWindow is what a rate is taken over. A minute keeps a service
	// with a request every few seconds from reading idle between two of them.
	scrapeWindow = time.Minute
	// scrapeTimeout bounds one round of reads.
	scrapeTimeout = 10 * time.Second
	// pageKeep is how long the page of a pod that is no longer read stays in
	// the shared store.
	pageKeep = 10 * time.Minute
)

func init() {
	probe.Register(probe.Access{
		Kind:   kindScrape,
		Source: "a counter on the Prometheus metrics page of every ready pod of a workload, read with a GET through the API server (pod informer, pods/proxy)",
		Delivers: "rate (per second, the counter's growth over the window summed over the pods) and error_rate (the percentage of it that the series matching errors make up); " +
			"detail: the pods read, the series matched, the window",
		SpecFields: []string{"namespace (required)", "selector (required)", "port (required)", "metric (required)", "match", "errors", "path", "scheme", "interval", "window", "kubeconfig", "context"},
		Needs: "list/watch on pods and get on pods/proxy in the namespace, which reaches every port of its pods with a GET; " +
			"the pods serve their metrics without credentials on that port",
		Implemented: true,
		Facets:      []string{facet.NameTraffic, facet.NameIngress},
		// Not pods/proxy: it is granted in the namespace a binding names and
		// in no other, which `wassup access` does from the bindings.
		RBAC: []probe.Rule{probe.Reads("", "pods")},
	}, func() probe.Probe { return &scrapeProbe{base: base{kind: kindScrape}} })
}

// ScrapeNamespace returns the namespace a k8s.scrape binding reads pods in,
// or "" for a binding of another probe. `wassup access` grants pods/proxy
// there.
func ScrapeNamespace(kind string, spec map[string]any) string {
	if kind != kindScrape {
		return ""
	}
	return probe.Str(spec, "namespace", "")
}

// ScrapeRules is what reading a metrics page needs in a namespace.
func ScrapeRules() []probe.Rule {
	return []probe.Rule{{Group: "", Resources: []string{"pods/proxy"}, Verbs: []string{"get"}}}
}

// scrapeConfig is a validated k8s.scrape spec.
type scrapeConfig struct {
	namespace string
	selector  labels.Selector
	port      string
	scheme    string
	path      string
	metric    string
	match     map[string]*regexp.Regexp
	errors    map[string]*regexp.Regexp
	interval  time.Duration
	window    time.Duration
	tick      time.Duration
}

// scrapeConfigOf validates a spec.
func scrapeConfigOf(spec map[string]any) (scrapeConfig, error) {
	var cfg scrapeConfig
	if err := probe.RequireString(spec, "namespace", "selector", "metric"); err != nil {
		return cfg, err
	}
	sel, err := labels.Parse(probe.Str(spec, "selector", ""))
	if err != nil {
		return cfg, fmt.Errorf("selector: %w", err)
	}
	cfg.namespace, cfg.selector, cfg.metric = probe.Str(spec, "namespace", ""), sel, probe.Str(spec, "metric", "")
	if strings.ContainsAny(cfg.metric, "{ \t") {
		return cfg, fmt.Errorf("metric is the name of a counter alone; its labels go into match")
	}
	if n, ok := probe.Num(spec, "port"); ok {
		cfg.port = strconv.Itoa(int(n))
	} else {
		cfg.port = probe.Str(spec, "port", "")
	}
	switch {
	case cfg.port == "":
		return cfg, fmt.Errorf("%q is required: the port the pods serve their metrics on", "port")
	case strings.ContainsAny(cfg.port, "/: \t"):
		return cfg, fmt.Errorf("port %q is a number or the name of a port", cfg.port)
	case isNumber(cfg.port):
		if _, err := portNumber(cfg.port); err != nil {
			return cfg, err
		}
	}
	switch cfg.scheme = probe.Str(spec, "scheme", "http"); cfg.scheme {
	case "http", "https":
	default:
		return cfg, fmt.Errorf("scheme %q is http or https", cfg.scheme)
	}
	if cfg.path = probe.Str(spec, "path", "/metrics"); !strings.HasPrefix(cfg.path, "/") {
		return cfg, fmt.Errorf("path %q must start with /", cfg.path)
	}
	if cfg.match, err = probe.Matchers(spec, "match"); err != nil {
		return cfg, err
	}
	if cfg.errors, err = probe.Matchers(spec, "errors"); err != nil {
		return cfg, err
	}
	if cfg.interval, err = probe.StrictDur(spec, "interval", scrapeInterval); err != nil {
		return cfg, err
	}
	if cfg.window, err = probe.StrictDur(spec, "window", scrapeWindow); err != nil {
		return cfg, err
	}
	if cfg.window < 2*cfg.interval {
		cfg.window = 2 * cfg.interval // a rate takes two readings
	}
	cfg.tick = tickOf(spec)
	return cfg, nil
}

// pagePath is the API server path that passes a GET on to a pod.
func (cfg scrapeConfig) pagePath(pod string) string {
	name := pod + ":" + cfg.port
	if cfg.scheme == "https" {
		name = "https:" + name
	}
	return "/api/v1/namespaces/" + cfg.namespace + "/pods/" + name + "/proxy" + cfg.path
}

// page is one reading of a metrics page.
type page struct {
	mu   sync.Mutex
	at   time.Time
	body []byte
	err  error
}

// pageStore shares the readings of metrics pages between the probes of one
// process: six bindings on the counters of one router are one request per
// pod and round, not six.
type pageStore struct {
	mu    sync.Mutex
	pages map[string]*page
	now   func() time.Time
}

var pages = &pageStore{pages: map[string]*page{}, now: time.Now}

// read returns the page at path, read no longer than fresh ago, and when
// it was read.
func (s *pageStore) read(ctx context.Context, c *Clients, path string, fresh time.Duration) ([]byte, time.Time, error) {
	key := fmt.Sprintf("%p %s", c, path)
	s.mu.Lock()
	now := s.now()
	for k, p := range s.pages {
		if k != key && p.mu.TryLock() {
			if !p.at.IsZero() && now.Sub(p.at) > pageKeep {
				delete(s.pages, k)
			}
			p.mu.Unlock()
		}
	}
	p := s.pages[key]
	if p == nil {
		p = &page{}
		s.pages[key] = p
	}
	s.mu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && s.now().Sub(p.at) < fresh {
		return p.body, p.at, p.err
	}
	body, err := c.proxy(ctx, path)
	if err != nil && ctx.Err() != nil {
		// The binding that read was stopped, or ran out of time. That says
		// nothing about the pod, so the next one reads again.
		return nil, s.now(), err
	}
	p.body, p.err, p.at = body, err, s.now()
	return p.body, p.at, p.err
}

// reading is the counter of one pod at one moment: all the series that
// match, and those among them that count as errors.
type reading struct {
	at            time.Time
	total, errors float64
}

// scrapeProbe is k8s.scrape.
type scrapeProbe struct {
	base
	// readings holds, per pod, the readings within the window, oldest first.
	readings map[string][]reading
	// store is the store of pages the probe reads from: the shared one,
	// unless a test gave it its own.
	store *pageStore
}

// Kind implements probe.Probe.
func (p *scrapeProbe) Kind() string { return kindScrape }

// Validate implements probe.Probe.
func (p *scrapeProbe) Validate(spec map[string]any) error {
	_, err := scrapeConfigOf(spec)
	return err
}

// Start implements probe.Probe.
func (p *scrapeProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	cfg, err := scrapeConfigOf(spec)
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c, err := p.connect(spec)
	if err != nil {
		return err
	}
	if p.store == nil {
		p.store = pages
	}
	target := targetOf(spec, cfg.metric)
	inf := c.informers()
	pods := inf.factory.Core().V1().Pods().Lister()
	kick, _ := kicker()
	go func() {
		if err := p.await(ctx, c, out, target, inf.pods); err != nil {
			return
		}
		runLoop(ctx, tickOf(spec), kick, func() {
			rctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
			defer cancel()
			o, err := p.observe(rctx, c, pods, cfg, target)
			if err != nil {
				p.fail(ctx, out, target, err)
				return
			}
			p.emit(ctx, out, o)
		})
	}()
	return nil
}

// observe reads the page of every ready pod and turns the counter into a
// rate over the window.
func (p *scrapeProbe) observe(ctx context.Context, c *Clients, pods corelisters.PodLister, cfg scrapeConfig, target string) (probe.Observation, error) {
	list, err := pods.Pods(cfg.namespace).List(cfg.selector)
	if err != nil {
		return probe.Observation{}, err
	}
	var ready []*corev1.Pod
	for _, pd := range list {
		if pd.DeletionTimestamp == nil && pd.Status.Phase == corev1.PodRunning && podReady(pd) {
			ready = append(ready, pd)
		}
	}
	if len(ready) == 0 {
		return probe.Observation{}, fmt.Errorf("no ready pod in namespace %s matches %q", cfg.namespace, cfg.selector)
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })
	if p.readings == nil {
		p.readings = map[string][]reading{}
	}

	var read, unread []string
	var firstErr error
	matched := 0
	live := map[string]bool{}
	for _, pd := range ready {
		live[pd.Name] = true
		// The first two readings are a tick apart, so the rate is there on
		// the second round; after that the page is read every interval.
		fresh := cfg.interval
		if len(p.readings[pd.Name]) < 2 && cfg.tick < fresh {
			fresh = cfg.tick / 2
		}
		body, at, err := p.store.read(ctx, c, cfg.pagePath(pd.Name), fresh)
		if err == nil {
			var all []series
			if all, err = seriesOf(body, cfg.metric); err == nil && len(all) == 0 {
				err = fmt.Errorf("the page has no metric %s", cfg.metric)
			}
			if err == nil {
				r := reading{at: at}
				for _, s := range all {
					if !probe.Matches(s.labels, cfg.match) {
						continue
					}
					matched++
					r.total += s.value
					if len(cfg.errors) > 0 && probe.Matches(s.labels, cfg.errors) {
						r.errors += s.value
					}
				}
				p.keep(pd.Name, r, cfg.window)
			}
		}
		if err != nil {
			unread = append(unread, pd.Name)
			if firstErr == nil {
				firstErr = fmt.Errorf("pod %s/%s port %s: %w", cfg.namespace, pd.Name, cfg.port, err)
			}
			continue
		}
		read = append(read, pd.Name)
	}
	for pod := range p.readings {
		if !live[pod] {
			delete(p.readings, pod)
		}
	}
	if len(read) == 0 {
		return probe.Observation{}, firstErr
	}

	now := time.Now()
	o := probe.Observation{Target: target, At: now, Metrics: map[string]float64{}, Detail: map[string]any{
		"metric": cfg.metric, "pods": read, "series": matched, "window_s": int(cfg.window.Seconds()),
	}}
	if len(unread) > 0 {
		o.Detail["unread_pods"] = unread
		o.Detail["unread_note"] = "the rate leaves out the pods that could not be read: " + firstErr.Error()
	}
	if matched == 0 && len(cfg.match) > 0 {
		o.Detail["match_note"] = "no series of " + cfg.metric + " matches, so no rate is reported: nothing was counted yet, or match names a value that does not exist"
	}
	var t facet.TrafficFacet
	// What matches nothing was not counted by anybody: that is no rate of
	// zero, it is no rate.
	if rate, errs, ok := p.rate(read); ok && matched > 0 {
		t.Rate = facet.N(round3(rate))
		if len(cfg.errors) > 0 {
			t.ErrorRate = facet.N(0)
			if rate > 0 {
				t.ErrorRate = facet.N(round1(100 * errs / rate))
			}
		}
	}
	// IngressFacet and TrafficFacet write a rate the same way.
	facet.EmitTraffic(&o, t, now)
	return o, nil
}

// keep adds a reading to a pod's history. A page that was not read again
// adds nothing, and a counter that went down was reset (the pod's process
// started over), so what came before is no basis for a rate.
func (p *scrapeProbe) keep(pod string, r reading, window time.Duration) {
	h := p.readings[pod]
	if n := len(h); n > 0 {
		switch last := h[n-1]; {
		case !r.at.After(last.at):
			return
		case r.total < last.total || r.errors < last.errors:
			h = nil
		}
	}
	h = append(h, r)
	// the window, and always the two newest
	for len(h) > 2 && r.at.Sub(h[0].at) > window {
		h = h[1:]
	}
	p.readings[pod] = h
}

// rate sums, over the pods read, the growth of the counter per second
// between the oldest and the newest reading. It is not known before one
// pod has two readings.
func (p *scrapeProbe) rate(podNames []string) (rate, errs float64, ok bool) {
	for _, pod := range podNames {
		h := p.readings[pod]
		if len(h) < 2 {
			continue
		}
		first, last := h[0], h[len(h)-1]
		dt := last.at.Sub(first.at).Seconds()
		if dt <= 0 {
			continue
		}
		rate += (last.total - first.total) / dt
		errs += (last.errors - first.errors) / dt
		ok = true
	}
	return rate, errs, ok
}

// round3 rounds to three decimals: a request a minute is 0.017 per second.
func round3(v float64) float64 { return probe.Round(v, 3) }
