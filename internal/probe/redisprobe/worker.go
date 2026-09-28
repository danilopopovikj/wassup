package redisprobe

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

const (
	// workerMinInterval is the shortest poll interval against Flower.
	workerMinInterval = 5 * time.Second
	// workerMaxLongTasks bounds the TaskRunning conditions per observation.
	workerMaxLongTasks = 5
	// workerP95Min is the number of finished tasks p95_s needs.
	workerP95Min = 10
	// workerNotReadyKey is the firstSeen key of the NotReady condition.
	workerNotReadyKey = "no-workers"
)

// workerAccess documents celery.worker.
var workerAccess = probe.Access{
	Kind:   "celery.worker",
	Source: "Flower's REST API (/api/workers with and without status=true, /api/tasks?state=SUCCESS)",
	Delivers: "workers_online, workers_total, active, pool_max, pool_used, running_s, p95_s (when at least 10 finished tasks are known), queues; " +
		"TaskRunning for tasks older than long_task, NotReady when no worker is online; detail: flower_url, via, workers, queues, long_tasks",
	SpecFields: []string{"flower_url", "name", "long_task", "interval", "timeout", "via", "kubeconfig", "context"},
	Needs: "HTTP access to Flower; no credentials. " +
		"With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to Flower, " +
		"which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); " +
		"bindings with the same via share one. flower_url may then be left out and reads http://<name>.<namespace>.svc:<port>; " +
		"one that is set keeps its scheme and its path, and its host is the Host header and the name of the certificate, not the address that is dialled",
	Implemented: true,
	Facets:      []string{facet.NameBackgroundWorker},
}

func init() {
	probe.Register(workerAccess, func() probe.Probe { return &WorkerProbe{} })
}

// flowerWorkerStats is the stats object Flower reports per worker.
type flowerWorkerStats struct {
	Pool struct {
		MaxConcurrency int   `json:"max-concurrency"`
		Processes      []int `json:"processes"`
	} `json:"pool"`
	Total map[string]int64 `json:"total"`
}

// flowerWorkerFull is one entry of /api/workers including stats.
type flowerWorkerFull struct {
	ActiveQueues []struct {
		Name string `json:"name"`
	} `json:"active_queues"`
	Active []FlowerTask       `json:"active"`
	Stats  *flowerWorkerStats `json:"stats"`
}

// WorkerProbe is celery.worker: the Celery workers behind a workload as Flower
// sees them. Spec: flower_url (required, unless via names a tunnel), name
// (optional prefix filter on the worker hostname), long_task (default 10m),
// interval (default tick, min 5s), timeout (default 5s), via (to Flower).
type WorkerProbe struct {
	h probe.Health
	probe.Lifetime

	// now and client are overridable for tests.
	now    func() time.Time
	client *http.Client

	// tunnel is set when the binding names a tunnel wassup opens itself;
	// conns is then the transport of client, whose connections go through
	// it. flower_url only says what to ask for.
	tunnel *probe.Via
	conns  *http.Transport
}

// workerConfig is the parsed spec.
type workerConfig struct {
	target   string
	flower   string
	name     string
	longTask time.Duration
	tick     time.Duration
	interval time.Duration
	timeout  time.Duration
	// via is the `via` of the binding, a tunnel or a label, for the detail.
	via string
}

// Kind implements probe.Probe.
func (p *WorkerProbe) Kind() string { return workerAccess.Kind }

// Validate implements probe.Probe.
func (p *WorkerProbe) Validate(spec map[string]any) error {
	if err := probe.ValidateVia(probe.Str(spec, "via", "")); err != nil {
		return err
	}
	if _, err := flowerURL(spec); err != nil {
		return err
	}
	if v, ok := spec["name"]; ok {
		if _, isStr := v.(string); !isStr {
			return fmt.Errorf("name must be a string, got %T", v)
		}
	}
	for _, k := range []string{"long_task", "interval", "timeout"} {
		if v, ok := spec[k].(string); ok && v != "" {
			if _, err := time.ParseDuration(v); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	return nil
}

// Health implements probe.Probe.
func (p *WorkerProbe) Health() probe.ProbeHealth { return p.h.Get() }

// flowerURL returns the URL of Flower in a spec.
func flowerURL(spec map[string]any) (string, error) {
	return probe.NewVia(spec).URLOf(spec, "flower_url")
}

// clock returns the current time from the test hook or the wall clock.
func (p *WorkerProbe) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// parseWorkerSpec turns a spec into a workerConfig.
func parseWorkerSpec(spec map[string]any) workerConfig {
	c := workerConfig{
		target:   target(spec),
		name:     probe.Str(spec, "name", ""),
		longTask: probe.Dur(spec, "long_task", 10*time.Minute),
		tick:     tick(spec),
		timeout:  probe.Dur(spec, "timeout", roundTimeout),
		via:      probe.Str(spec, "via", ""),
	}
	c.flower, _ = flowerURL(spec)
	c.interval = max(probe.Dur(spec, "interval", c.tick), workerMinInterval, c.tick)
	return c
}

// Start implements probe.Probe.
func (p *WorkerProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := p.Validate(spec); err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c := parseWorkerSpec(spec)
	p.tunnel = probe.NewVia(spec)
	if p.client == nil && p.tunnel != nil {
		p.conns = p.tunnel.Through(http.DefaultTransport.(*http.Transport).Clone())
		p.client = &http.Client{Timeout: c.timeout, Transport: probe.ReadOnly(p.conns)}
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: c.timeout, Transport: probe.ReadOnly(nil)}
	}
	flower := &flowerClient{base: c.flower, http: p.client, unanswered: p.unanswered}
	p.h.Set(probe.HealthOK, "polling "+c.flower)
	p.Go(func() {
		defer p.release()
		seen := firstSeen{}
		last := p.turn(ctx, flower, c, seen)
		if ctx.Err() != nil || !probe.Send(ctx, out, last) {
			return
		}
		pollT := time.NewTicker(c.interval)
		defer pollT.Stop()
		var tickC <-chan time.Time
		if c.interval > c.tick {
			tickT := time.NewTicker(c.tick)
			defer tickT.Stop()
			tickC = tickT.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-pollT.C:
				last = p.turn(ctx, flower, c, seen)
				if ctx.Err() != nil {
					return
				}
				if !probe.Send(ctx, out, last) {
					return
				}
			case <-tickC:
				o := last
				o.At = time.Now()
				if !probe.Send(ctx, out, o) {
					return
				}
			}
		}
	})
	return nil
}

// turn runs one round. Through a tunnel it does not end with the probe's
// context: a request that is cut halfway reaches the server as a reset, and
// the port-forward goes down with it (probe.RoundContext). A round that the
// stop cut short says nothing about Flower: the health stays what it was.
func (p *WorkerProbe) turn(ctx context.Context, flower *flowerClient, c workerConfig, seen firstSeen) probe.Observation {
	before := p.h.Get()
	rctx, cancel := ctx, context.CancelFunc(func() {})
	if p.tunnel != nil {
		rctx, cancel = probe.RoundContext(ctx, c.timeout)
	}
	defer cancel()
	o := p.round(rctx, flower, c, seen)
	if ctx.Err() != nil {
		p.h.Set(before.State, before.Message)
	}
	return o
}

// unanswered is called after a request that got no answer. Through a tunnel
// it may be the tunnel that broke: the connections are closed and the
// tunnel is dropped, in that order, so the next round opens a new one.
func (p *WorkerProbe) unanswered() {
	if p.tunnel == nil {
		return
	}
	if p.conns != nil {
		p.conns.CloseIdleConnections()
	}
	p.tunnel.Drop()
}

// release lets go of what the probe holds when it stops: the connections
// first, the tunnel after them.
func (p *WorkerProbe) release() {
	if p.conns != nil {
		p.conns.CloseIdleConnections()
	}
	p.tunnel.Close()
}

// workerMatches applies the optional name prefix to a Flower worker name
// ("celery@worker-1"): the whole name or the part after the "@" may match.
func workerMatches(name, prefix string) bool {
	if prefix == "" {
		return true
	}
	if strings.HasPrefix(name, prefix) {
		return true
	}
	if i := strings.IndexByte(name, '@'); i >= 0 {
		return strings.HasPrefix(name[i+1:], prefix)
	}
	return false
}

// longTaskInfo is one task past long_task, for Detail.
type longTaskInfo struct {
	id, name, worker string
	started          time.Time
}

// round does one poll of Flower and returns the observation.
func (p *WorkerProbe) round(ctx context.Context, flower *flowerClient, c workerConfig, seen firstSeen) probe.Observation {
	now := p.clock()
	o := probe.Observation{Target: c.target, Probe: p.Kind(), At: now, Detail: map[string]any{"flower_url": c.flower}}
	if c.via != "" {
		o.Detail["via"] = c.via
	}
	if c.name != "" {
		o.Detail["name"] = c.name
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var workers map[string]*flowerWorkerFull
	if err := flower.get(rctx, "/api/workers", url.Values{"refresh": {"false"}}, &workers); err != nil {
		o.Err = "flower: " + err.Error()
		o.Detail["last_error"] = o.Err
		p.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	var status map[string]bool
	if err := flower.get(rctx, "/api/workers", url.Values{"status": {"true"}}, &status); err != nil {
		o.Err = "flower: " + err.Error()
		o.Detail["last_error"] = o.Err
		p.h.Set(probe.HealthDegraded, o.Err)
		return o
	}

	// Every worker name Flower knows, filtered by the prefix and sorted so
	// Detail is stable.
	nameSet := map[string]bool{}
	for n := range workers {
		if workerMatches(n, c.name) {
			nameSet[n] = true
		}
	}
	for n := range status {
		if workerMatches(n, c.name) {
			nameSet[n] = true
		}
	}
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	sort.Strings(names)

	var online, active, poolMax int
	var longest time.Duration
	queues := map[string]bool{}
	var long, allActive []longTaskInfo
	workerDetail := make([]map[string]any, 0, len(names))
	for _, n := range names {
		w := workers[n]
		isOnline := status[n]
		d := map[string]any{"name": n, "online": isOnline}
		if w != nil {
			d["active"] = len(w.Active)
			if w.Stats != nil {
				d["concurrency"] = w.Stats.Pool.MaxConcurrency
			}
		}
		workerDetail = append(workerDetail, d)
		if !isOnline || w == nil {
			continue
		}
		online++
		active += len(w.Active)
		if w.Stats != nil {
			poolMax += w.Stats.Pool.MaxConcurrency
		}
		for _, q := range w.ActiveQueues {
			if q.Name != "" {
				queues[q.Name] = true
			}
		}
		for _, t := range w.Active {
			if t.TimeStart <= 0 {
				continue
			}
			started := parseTime(t.TimeStart)
			running := max(now.Sub(started), 0)
			longest = max(longest, running)
			allActive = append(allActive, longTaskInfo{id: t.ID, name: t.Name, worker: n, started: started})
			if running >= c.longTask {
				long = append(long, longTaskInfo{id: t.ID, name: t.Name, worker: n, started: started})
			}
		}
	}

	o.Metrics = map[string]float64{"queues": float64(len(queues))}
	o.Detail["workers"] = workerDetail
	queueNames := make([]string, 0, len(queues))
	for q := range queues {
		queueNames = append(queueNames, q)
	}
	sort.Strings(queueNames)
	o.Detail["queues"] = queueNames

	pool := facet.BackgroundWorkerFacet{
		Online: facet.NI(online), Total: facet.NI(len(names)), Active: facet.NI(active), LongTask: c.longTask,
		NotReadyDetail: "no Celery workers online",
		Since:          func(key string, at time.Time) time.Time { return seen.mark(key, at) },
	}
	if poolMax > 0 {
		pool.SlotsUsed, pool.SlotsMax = facet.NI(active), facet.NI(poolMax)
	}
	for _, t := range allActive {
		pool.Running = append(pool.Running, facet.Task{ID: t.id, Name: t.name, Worker: t.worker, Started: t.started})
	}
	// Longest-running first in the detail, every long task listed.
	sort.Slice(long, func(i, j int) bool { return long[i].started.Before(long[j].started) })
	longDetail := make([]map[string]any, 0, len(long))
	for _, t := range long {
		longDetail = append(longDetail, map[string]any{
			"id": t.id, "name": t.name, "worker": t.worker, "running_s": now.Sub(t.started).Seconds(),
		})
	}
	if len(longDetail) > 0 {
		o.Detail["long_tasks"] = longDetail
	}

	// p95 of recent successful runtimes, best effort.
	var finished map[string]FlowerFinished
	if err := flower.get(rctx, "/api/tasks", url.Values{"state": {"SUCCESS"}, "limit": {"200"}}, &finished); err == nil {
		if p95, ok := p95Runtimes(finished); ok {
			pool.TypicalDuration = time.Duration(p95 * float64(time.Second))
		}
	} else {
		o.Detail["tasks_error"] = err.Error()
	}

	facet.EmitBackgroundWorker(&o, pool, now)
	live := map[string]bool{}
	for _, cond := range o.Conditions {
		switch cond.Kind {
		case model.CondTaskRunning:
			live[cond.Ref] = true
			seen.mark(cond.Ref, cond.Since)
		case model.CondNotReady:
			live[facet.KeyNoWorkers] = true
		}
	}
	seen.keep(live)

	p.h.Set(probe.HealthOK, "")
	return o
}

// p95Runtimes returns the 95th percentile of the runtimes of finished tasks
// when at least workerP95Min of them carry one.
func p95Runtimes(finished map[string]FlowerFinished) (float64, bool) {
	rt := make([]float64, 0, len(finished))
	for _, t := range finished {
		if t.Runtime != nil {
			rt = append(rt, *t.Runtime)
		}
	}
	if len(rt) < workerP95Min {
		return 0, false
	}
	sort.Float64s(rt)
	return rt[int(float64(len(rt)-1)*0.95)], true
}
