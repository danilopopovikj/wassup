// Package amqp implements amqp.queue, the probe of one RabbitMQ queue. It
// reads the RabbitMQ management HTTP API (/api/queues/<vhost>/<queue>), not
// the AMQP wire protocol, so it needs a management user with the monitoring
// tag and nothing else. Everything is read only.
package amqp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// KindQueue is the probe kind.
const KindQueue = "amqp.queue"

const (
	defaultTick     = 5 * time.Second
	defaultTimeout  = 5 * time.Second
	defaultVhost    = "/"
	defaultUser     = "guest"
	defaultPassEnv  = "RABBITMQ_PASSWORD"
	maxBodyBytes    = 4 << 20
	userAgent       = "wassup/1 (+https://github.com/danilopopovikj/wassup)"
	growthWindowDur = time.Minute
)

func init() {
	probe.Register(probe.Access{
		Kind:   KindQueue,
		Source: "the RabbitMQ management API (GET /api/queues/<vhost>/<queue>)",
		Delivers: "depth (messages ready), unacked, consumers, rate (deliver/get per second, ack rate as fallback), publish_rate, " +
			"growth_per_min over the last minute, oldest_age_s when the queue exposes head_message_timestamp; detail: state, memory, node, vhost",
		SpecFields:  []string{"management_url", "queue", "vhost", "user", "password_env", "interval", "timeout"},
		Needs:       "a management user with the monitoring tag; the password in the environment variable named by password_env (default RABBITMQ_PASSWORD)",
		Implemented: true,
		Tier:        probe.TierToken,
		Facets:      []string{facet.NameQueue},
	}, func() probe.Probe { return &Queue{} })
}

// config is the parsed spec.
type config struct {
	target   string
	base     string // management URL without trailing slash
	queue    string
	vhost    string
	user     string
	password string
	auth     bool
	tick     time.Duration
	interval time.Duration
	timeout  time.Duration
}

// requestURL is the queue's management API URL. The vhost and the queue name
// are path-escaped, so the default vhost "/" becomes %2F.
func (c config) requestURL() string {
	return c.base + "/api/queues/" + url.PathEscape(c.vhost) + "/" + url.PathEscape(c.queue)
}

// Queue is the amqp.queue probe.
type Queue struct {
	h probe.Health

	// now and client are overridable for tests.
	now    func() time.Time
	client *http.Client

	ring depthRing
}

// Kind implements probe.Probe.
func (p *Queue) Kind() string { return KindQueue }

// managementURL reads management_url, accepting url as a fallback spelling.
func managementURL(spec map[string]any) string {
	return probe.Str(spec, "management_url", probe.Str(spec, "url", ""))
}

// Validate implements probe.Probe.
func (p *Queue) Validate(spec map[string]any) error {
	raw := managementURL(spec)
	if raw == "" {
		return errors.New(`"management_url" is required`)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("management_url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("management_url %q must be an absolute http(s) URL", raw)
	}
	if err := probe.RequireString(spec, "queue"); err != nil {
		return err
	}
	for _, k := range []string{"vhost", "user", "password_env"} {
		if v, ok := spec[k]; ok {
			if _, isStr := v.(string); !isStr {
				return fmt.Errorf("%s must be a string, got %T", k, v)
			}
		}
	}
	for _, k := range []string{"interval", "timeout"} {
		if v, ok := spec[k].(string); ok && v != "" {
			if _, err := time.ParseDuration(v); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	return nil
}

// Health implements probe.Probe.
func (p *Queue) Health() probe.ProbeHealth { return p.h.Get() }

// clock returns the current time from the test hook or the wall clock.
func (p *Queue) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// strOr reads a string field, honouring an explicitly empty value.
func strOr(spec map[string]any, key, def string) string {
	if v, ok := spec[key].(string); ok {
		return v
	}
	return def
}

// parse turns a spec into a config, reading the password from the
// environment. An empty password_env or an unset variable is only acceptable
// when user is empty too (an unauthenticated management API); otherwise the
// spec names a user we cannot authenticate and the probe fails.
func parse(spec map[string]any) (config, error) {
	c := config{
		target:  probe.Str(spec, "_target", ""),
		base:    strings.TrimRight(managementURL(spec), "/"),
		queue:   probe.Str(spec, "queue", ""),
		vhost:   strOr(spec, "vhost", defaultVhost),
		user:    strOr(spec, "user", defaultUser),
		tick:    defaultTick,
		timeout: probe.Dur(spec, "timeout", defaultTimeout),
	}
	if c.vhost == "" {
		c.vhost = defaultVhost
	}
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		c.tick = d
	}
	c.interval = probe.Dur(spec, "interval", c.tick)
	env := strOr(spec, "password_env", defaultPassEnv)
	pw := ""
	if env != "" {
		pw = os.Getenv(env)
	}
	switch {
	case pw != "":
		c.password, c.auth = pw, true
	case c.user == "":
		c.auth = false
	case env == "":
		return c, fmt.Errorf("password_env is empty but user %q is set", c.user)
	default:
		return c, fmt.Errorf("environment variable %s (password_env) is not set for user %q", env, c.user)
	}
	return c, nil
}

// Start implements probe.Probe.
func (p *Queue) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := p.Validate(spec); err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c, err := parse(spec)
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	if p.client == nil {
		p.client = newClient(c.timeout)
	}
	p.h.Set(probe.HealthOK, "polling "+c.requestURL())
	go loop(ctx, c.tick, c.interval, out, func(ctx context.Context) probe.Observation {
		return p.poll(ctx, c)
	})
	return nil
}

// newClient builds an HTTP client with a per-request timeout.
func newClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	return &http.Client{Timeout: timeout, Transport: probe.ReadOnly(tr)}
}

// loop runs check once, then every interval, re-emitting the last
// observation every tick in between with a fresh At.
func loop(ctx context.Context, tick, interval time.Duration, out chan<- probe.Observation, check func(context.Context) probe.Observation) {
	if interval < tick {
		interval = tick
	}
	last := check(ctx)
	if !probe.Send(ctx, out, last) {
		return
	}
	checkT := time.NewTicker(interval)
	defer checkT.Stop()
	var tickC <-chan time.Time
	if interval > tick {
		tickT := time.NewTicker(tick)
		defer tickT.Stop()
		tickC = tickT.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-checkT.C:
			last = check(ctx)
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
}

// rateDetails is one of the *_details objects of message_stats.
type rateDetails struct {
	Rate *float64 `json:"rate"`
}

// queueInfo is the part of the management API's queue object the probe reads.
type queueInfo struct {
	Name                 string `json:"name"`
	Vhost                string `json:"vhost"`
	State                string `json:"state"`
	Node                 string `json:"node"`
	Durable              *bool  `json:"durable"`
	Memory               *int64 `json:"memory"`
	Messages             *int64 `json:"messages"`
	MessagesReady        *int64 `json:"messages_ready"`
	MessagesUnacked      *int64 `json:"messages_unacknowledged"`
	Consumers            *int64 `json:"consumers"`
	HeadMessageTimestamp *int64 `json:"head_message_timestamp"`
	MessageStats         *struct {
		Publish    *rateDetails `json:"publish_details"`
		Ack        *rateDetails `json:"ack_details"`
		DeliverGet *rateDetails `json:"deliver_get_details"`
	} `json:"message_stats"`
}

// poll performs one management API request and builds the observation.
func (p *Queue) poll(ctx context.Context, c config) probe.Observation {
	now := p.clock()
	o := probe.Observation{
		Target: c.target,
		Probe:  KindQueue,
		At:     now,
		Detail: map[string]any{"url": c.requestURL(), "vhost": c.vhost, "queue": c.queue},
	}
	fail := func(state probe.HealthState, msg string) probe.Observation {
		o.Err = msg
		o.Detail["last_error"] = msg
		p.h.Set(state, msg)
		return o
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.requestURL(), nil)
	if err != nil {
		return fail(probe.HealthFailed, err.Error())
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if c.auth {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if isTimeout(err) {
			return fail(probe.HealthDegraded, fmt.Sprintf("GET %s timed out after %s", c.requestURL(), c.timeout.Round(time.Millisecond)))
		}
		return fail(probe.HealthDegraded, "GET "+c.requestURL()+": "+err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	o.Detail["status"] = resp.StatusCode

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fail(probe.HealthFailed, fmt.Sprintf("management API rejected user %q (HTTP %d)", c.user, resp.StatusCode))
	case resp.StatusCode == http.StatusNotFound:
		return fail(probe.HealthFailed, fmt.Sprintf("queue %q does not exist in vhost %q (HTTP 404)", c.queue, c.vhost))
	case resp.StatusCode >= 500:
		return fail(probe.HealthDegraded, fmt.Sprintf("management API HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	case resp.StatusCode != http.StatusOK:
		return fail(probe.HealthDegraded, fmt.Sprintf("management API HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}

	var q queueInfo
	if err := json.Unmarshal(body, &q); err != nil {
		return fail(probe.HealthDegraded, "management API: decoding queue: "+err.Error())
	}

	o.Metrics = map[string]float64{}
	f := facet.QueueFacet{}
	if q.MessagesReady != nil {
		f.Depth = facet.NI(int(*q.MessagesReady))
		p.ring.push(now, f.Depth.V)
		if g, ok := p.ring.growth(); ok {
			f.GrowthPerMin = facet.N(g)
		}
	}
	if q.MessagesUnacked != nil {
		o.Metrics["unacked"] = float64(*q.MessagesUnacked)
	}
	if q.Consumers != nil {
		f.Consumers = facet.NI(int(*q.Consumers))
	}
	if q.MessageStats != nil {
		if r := rateOf(q.MessageStats.DeliverGet); r != nil {
			f.Rate = facet.N(*r)
		} else if r := rateOf(q.MessageStats.Ack); r != nil {
			f.Rate = facet.N(*r)
		}
		if r := rateOf(q.MessageStats.Publish); r != nil {
			f.PublishRate = facet.N(*r)
		}
	}
	if q.HeadMessageTimestamp != nil && *q.HeadMessageTimestamp > 0 {
		f.Oldest, f.HasOldest = max(now.Sub(time.Unix(*q.HeadMessageTimestamp, 0)), 0), true
	}
	facet.EmitQueue(&o, f, now)

	if q.State != "" {
		o.Detail["state"] = q.State
	}
	if q.Node != "" {
		o.Detail["node"] = q.Node
	}
	if q.Memory != nil {
		o.Detail["memory"] = *q.Memory
	}
	if q.Messages != nil {
		o.Detail["messages"] = *q.Messages
	}
	if q.Durable != nil {
		o.Detail["durable"] = *q.Durable
	}
	if q.Vhost != "" {
		o.Detail["vhost"] = q.Vhost
	}
	p.h.Set(probe.HealthOK, "")
	return o
}

// rateOf returns the rate of a *_details object, nil when absent.
func rateOf(d *rateDetails) *float64 {
	if d == nil {
		return nil
	}
	return d.Rate
}

// isTimeout reports whether err is a deadline or a network timeout.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
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
	for cut < len(r.at)-1 && at.Sub(r.at[cut]) > growthWindowDur {
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
