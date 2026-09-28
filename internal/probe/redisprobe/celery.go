package redisprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// Envelope is the part of a Celery message (kombu's JSON envelope on the
// Redis transport) a queue probe cares about. Only the headers and
// properties are read; the body stays opaque.
type Envelope struct {
	Task        string
	ID          string
	Origin      string
	RoutingKey  string
	ETA         time.Time // zero when the task has no eta
	PublishedAt time.Time // zero unless a published_at header or property is present
}

// ParseEnvelope parses one raw list element from a Celery queue. Celery does
// not stamp a publish time by default, so PublishedAt is only set when a
// "published_at" header or property exists (some producers add it); ETA is
// read from headers.eta.
func ParseEnvelope(raw []byte) (Envelope, error) {
	var msg struct {
		Headers struct {
			Task        string `json:"task"`
			ID          string `json:"id"`
			Origin      string `json:"origin"`
			ETA         any    `json:"eta"`
			PublishedAt any    `json:"published_at"`
		} `json:"headers"`
		Properties struct {
			PublishedAt  any `json:"published_at"`
			DeliveryInfo struct {
				RoutingKey string `json:"routing_key"`
			} `json:"delivery_info"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Envelope{}, fmt.Errorf("celery envelope: %w", err)
	}
	e := Envelope{
		Task:       msg.Headers.Task,
		ID:         msg.Headers.ID,
		Origin:     msg.Headers.Origin,
		RoutingKey: msg.Properties.DeliveryInfo.RoutingKey,
	}
	if e.Task == "" && e.ID == "" {
		return e, errors.New("celery envelope: no task headers")
	}
	e.ETA = parseTime(msg.Headers.ETA)
	e.PublishedAt = parseTime(msg.Headers.PublishedAt)
	if e.PublishedAt.IsZero() {
		e.PublishedAt = parseTime(msg.Properties.PublishedAt)
	}
	return e, nil
}

// Age returns how long the message has been waiting: since published_at when
// known, else since its eta (a task past its eta has waited that long). It
// reports false when the envelope carries neither.
func (e Envelope) Age(now time.Time) (time.Duration, bool) {
	switch {
	case !e.PublishedAt.IsZero():
		return max(now.Sub(e.PublishedAt), 0), true
	case !e.ETA.IsZero():
		return max(now.Sub(e.ETA), 0), true
	}
	return 0, false
}

// parseTime accepts an RFC 3339 string (Celery's isoformat) or a unix
// timestamp in seconds.
func parseTime(v any) time.Time {
	switch t := v.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05"} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts
			}
		}
	case float64:
		if t > 0 {
			sec := int64(t)
			return time.Unix(sec, int64((t-float64(sec))*1e9))
		}
	}
	return time.Time{}
}

// depthRing keeps the last minute of depth readings for growth_per_min.
type depthRing struct {
	at    []time.Time
	depth []float64
}

// push records a reading and drops those older than a minute before it.
func (r *depthRing) push(at time.Time, depth float64) {
	r.at = append(r.at, at)
	r.depth = append(r.depth, depth)
	cut := 0
	for cut < len(r.at)-1 && at.Sub(r.at[cut]) > time.Minute {
		cut++
	}
	r.at, r.depth = r.at[cut:], r.depth[cut:]
}

// growth returns the depth change per minute over the ring, false when the
// ring holds fewer than two readings.
func (r *depthRing) growth() (float64, bool) {
	n := len(r.at)
	if n < 2 {
		return 0, false
	}
	span := r.at[n-1].Sub(r.at[0]).Minutes()
	if span <= 0 {
		return 0, false
	}
	return (r.depth[n-1] - r.depth[0]) / span, true
}

// FlowerWorker is one entry of Flower's /api/workers.
type FlowerWorker struct {
	ActiveQueues []struct {
		Name string `json:"name"`
	} `json:"active_queues"`
	Active []FlowerTask `json:"active"`
}

// FlowerTask is one running task as Flower reports it.
type FlowerTask struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Hostname     string  `json:"hostname"`
	TimeStart    float64 `json:"time_start"`
	DeliveryInfo struct {
		RoutingKey string `json:"routing_key"`
	} `json:"delivery_info"`
}

// FlowerFinished is one task from /api/tasks with its runtime.
type FlowerFinished struct {
	RoutingKey string   `json:"routing_key"`
	Runtime    *float64 `json:"runtime"`
	State      string   `json:"state"`
}

// FlowerSummary is what the probe derives from Flower for one queue.
type FlowerSummary struct {
	Consumers int
	Running   []FlowerTask
	P95       float64
	HasP95    bool
}

// SummarizeFlower counts the workers consuming queue and lists the tasks
// running on it (tasks without a routing key are attributed to every queue,
// as Flower omits the key on some transports).
func SummarizeFlower(workers map[string]FlowerWorker, queue string) FlowerSummary {
	var s FlowerSummary
	for _, w := range workers {
		for _, q := range w.ActiveQueues {
			if q.Name == queue {
				s.Consumers++
				break
			}
		}
		for _, t := range w.Active {
			if t.DeliveryInfo.RoutingKey == "" || t.DeliveryInfo.RoutingKey == queue {
				s.Running = append(s.Running, t)
			}
		}
	}
	return s
}

// P95Runtime returns the 95th percentile runtime of finished tasks on queue.
func P95Runtime(tasks []FlowerFinished, queue string) (float64, bool) {
	var rt []float64
	for _, t := range tasks {
		if t.Runtime == nil || (t.RoutingKey != "" && t.RoutingKey != queue) {
			continue
		}
		rt = append(rt, *t.Runtime)
	}
	if len(rt) == 0 {
		return 0, false
	}
	sort.Float64s(rt)
	idx := int(float64(len(rt)-1) * 0.95)
	return rt[idx], true
}

// flowerClient reads Flower's REST API.
type flowerClient struct {
	base string
	http *http.Client
}

func (f *flowerClient) get(ctx context.Context, path string, query url.Values, into any) error {
	u := strings.TrimRight(f.base, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("flower %s: HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, into)
}

// summary fetches workers and, best effort, recent finished tasks.
func (f *flowerClient) summary(ctx context.Context, queue string) (FlowerSummary, error) {
	var workers map[string]FlowerWorker
	if err := f.get(ctx, "/api/workers", url.Values{"refresh": {"1"}}, &workers); err != nil {
		return FlowerSummary{}, err
	}
	s := SummarizeFlower(workers, queue)
	var finished map[string]FlowerFinished
	if err := f.get(ctx, "/api/tasks", url.Values{"state": {"SUCCESS"}, "limit": {"200"}}, &finished); err == nil {
		list := make([]FlowerFinished, 0, len(finished))
		for _, t := range finished {
			list = append(list, t)
		}
		s.P95, s.HasP95 = P95Runtime(list, queue)
	}
	return s, nil
}

// celeryAccess documents celery.queue.
var celeryAccess = probe.Access{
	Kind:   "celery.queue",
	Source: "the Celery queue list on a Redis broker (LLEN, LINDEX) and, when flower_url is set, Flower's REST API",
	Delivers: "depth, oldest_age_s (when the oldest envelope carries published_at or eta), growth_per_min; " +
		"with Flower: consumers, running_s, p95_s and TaskRunning for tasks older than long_task",
	SpecFields:  []string{"broker", "queue", "password_env", "flower_url", "oldest", "long_task"},
	Needs:       "network access to the broker (password via password_env) and, optionally, to Flower",
	Implemented: true,
	Facets:      []string{facet.NameQueue},
}

func init() {
	probe.Register(celeryAccess, func() probe.Probe { return &CeleryProbe{} })
}

// CeleryProbe is celery.queue. Spec: broker (redis URL, required), queue
// (required), password_env, flower_url, oldest (default true), long_task
// (default 10m).
type CeleryProbe struct {
	h probe.Health
}

// Kind implements probe.Probe.
func (p *CeleryProbe) Kind() string { return celeryAccess.Kind }

// Validate implements probe.Probe.
func (p *CeleryProbe) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "broker", "queue"); err != nil {
		return err
	}
	if err := validateOptions(spec, "broker"); err != nil {
		return err
	}
	if fu := probe.Str(spec, "flower_url", ""); fu != "" {
		u, err := url.Parse(fu)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("flower_url must be an http(s) URL")
		}
	}
	if v, ok := spec["long_task"].(string); ok {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("long_task: %w", err)
		}
	}
	return nil
}

// Health implements probe.Probe.
func (p *CeleryProbe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *CeleryProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	opt, err := options(spec, "broker")
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	queue := probe.Str(spec, "queue", "")
	oldest := true
	if v, ok := spec["oldest"].(bool); ok {
		oldest = v
	}
	longTask := probe.Dur(spec, "long_task", 10*time.Minute)
	var flower *flowerClient
	if fu := probe.Str(spec, "flower_url", ""); fu != "" {
		flower = &flowerClient{base: fu, http: &http.Client{Timeout: roundTimeout}}
	}
	client := redis.NewClient(opt)
	tgt := target(spec)
	every := tick(spec)
	go func() {
		defer client.Close()
		ring := &depthRing{}
		seen := firstSeen{}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			o := p.round(ctx, client, flower, queue, oldest, longTask, ring, seen, tgt)
			if ctx.Err() != nil {
				return
			}
			if !probe.Send(ctx, out, o) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// round does one tick of work and returns the observation.
func (p *CeleryProbe) round(ctx context.Context, client *redis.Client, flower *flowerClient, queue string, oldest bool, longTask time.Duration, ring *depthRing, seen firstSeen, tgt string) probe.Observation {
	o := probe.Observation{Target: tgt, Probe: p.Kind(), At: time.Now(), Metrics: map[string]float64{}, Detail: map[string]any{"queue": queue}}
	rctx, cancel := context.WithTimeout(ctx, roundTimeout)
	defer cancel()

	depth, err := client.LLen(rctx, queue).Result()
	if err != nil {
		o.Err = "LLEN " + queue + ": " + err.Error()
		o.Metrics, o.Detail = nil, nil
		p.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	q := facet.QueueFacet{Depth: facet.NI(int(depth)), LongTask: longTask}
	ring.push(o.At, float64(depth))
	if g, ok := ring.growth(); ok {
		q.GrowthPerMin = facet.N(g)
	}
	if oldest && depth > 0 {
		raw, err := client.LIndex(rctx, queue, -1).Result()
		if err == nil {
			if env, perr := ParseEnvelope([]byte(raw)); perr == nil {
				o.Detail["oldest_task"] = env.Task
				if age, ok := env.Age(o.At); ok {
					q.Oldest, q.HasOldest = age, true
				}
			}
		}
	}

	health, msg := probe.HealthOK, ""
	if flower != nil {
		s, err := flower.summary(rctx, queue)
		if err != nil {
			health, msg = probe.HealthDegraded, "flower: "+err.Error()
			o.Detail["flower_error"] = err.Error()
		} else {
			q.Consumers = facet.NI(s.Consumers)
			if s.HasP95 {
				q.TypicalDuration = time.Duration(s.P95 * float64(time.Second))
			}
			for _, task := range s.Running {
				if task.TimeStart <= 0 {
					continue
				}
				q.Running_ = append(q.Running_, facet.Task{ID: task.ID, Name: task.Name, Started: parseTime(task.TimeStart)})
			}
			if len(s.Running) > 0 {
				o.Detail["running"] = len(s.Running)
			}
		}
	}
	facet.EmitQueue(&o, q, o.At)
	live := map[string]bool{}
	for _, cond := range o.Conditions {
		if cond.Kind == model.CondTaskRunning {
			live[cond.Ref] = true
			seen.mark(cond.Ref, cond.Since)
		}
	}
	seen.keep(live)
	p.h.Set(health, msg)
	return o
}
