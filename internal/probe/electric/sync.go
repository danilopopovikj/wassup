// Package electric implements electric.sync, the probe of an Electric SQL
// sync service. It reads the service's health endpoint and, when a table is
// configured, performs the same shape handshake a client would: an initial
// shape request followed by one short live poll. Everything is read only.
package electric

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
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// KindSync is the probe kind.
const KindSync = "electric.sync"

const (
	defaultTick     = 5 * time.Second
	minInterval     = 5 * time.Second
	defaultTimeout  = 5 * time.Second
	liveTimeout     = 3 * time.Second
	maxSchemaNames  = 20
	maxBodyBytes    = 1 << 20
	userAgent       = "wassup/1 (+https://github.com/danilopopovikj/wassup)"
	statusActive    = "active"
	statusWaiting   = "waiting"
	statusStarting  = "starting"
	detailWaiting   = "waiting for Postgres"
	detailStarting  = "starting"
	keyNotReady     = "notready"
	keyRefused      = "refused"
	electricHandle  = "electric-handle"
	electricOffset  = "electric-offset"
	electricSchema  = "electric-schema"
	electricUpToDte = "electric-up-to-date"
)

func init() {
	probe.Register(probe.Access{
		Kind:   KindSync,
		Source: "the Electric SQL HTTP API: GET /v1/health and, when table is set, a /v1/shape handshake plus one short live poll",
		Delivers: "latency_ms of the health request, ready 1/0; with table: shape_ms, up_to_date 1/0, columns, busy on 429; " +
			"NotReady while Electric waits for Postgres or the shape is unavailable, ConnectionRefused when the service cannot be reached",
		SpecFields:  []string{"url", "secret_env", "table", "interval", "timeout"},
		Needs:       "HTTP access to Electric; the ELECTRIC_SECRET in the environment variable named by secret_env when the service requires one",
		Implemented: true,
	}, func() probe.Probe { return &Sync{} })
}

// config is the parsed spec.
type config struct {
	target   string
	url      string
	secret   string
	table    string
	tick     time.Duration
	interval time.Duration
	timeout  time.Duration
}

// Sync is the electric.sync probe.
type Sync struct {
	h probe.Health

	// now, client and live are overridable for tests. client carries the spec
	// timeout; live carries the short live-poll timeout.
	now    func() time.Time
	client *http.Client
	live   *http.Client

	// since remembers when each condition was first seen so Since is stable.
	since map[string]time.Time
}

// Kind implements probe.Probe.
func (p *Sync) Kind() string { return KindSync }

// Validate implements probe.Probe.
func (p *Sync) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "url"); err != nil {
		return err
	}
	if err := checkURL(probe.Str(spec, "url", "")); err != nil {
		return err
	}
	for _, k := range []string{"interval", "timeout"} {
		if v, ok := spec[k].(string); ok && v != "" {
			if _, err := time.ParseDuration(v); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	for _, k := range []string{"secret_env", "table"} {
		if v, ok := spec[k]; ok {
			if _, isStr := v.(string); !isStr {
				return fmt.Errorf("%s must be a string, got %T", k, v)
			}
		}
	}
	return nil
}

// checkURL requires an absolute http(s) URL.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url %q must be an absolute http(s) URL", raw)
	}
	return nil
}

// Health implements probe.Probe.
func (p *Sync) Health() probe.ProbeHealth { return p.h.Get() }

// clock returns the current time from the test hook or the wall clock.
func (p *Sync) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// parse turns a spec into a config, reading the secret from the environment.
func parse(spec map[string]any) (config, error) {
	c := config{
		target:  probe.Str(spec, "_target", ""),
		url:     strings.TrimRight(probe.Str(spec, "url", ""), "/"),
		table:   probe.Str(spec, "table", ""),
		tick:    defaultTick,
		timeout: probe.Dur(spec, "timeout", defaultTimeout),
	}
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		c.tick = d
	}
	c.interval = max(probe.Dur(spec, "interval", c.tick), minInterval)
	if env := probe.Str(spec, "secret_env", ""); env != "" {
		v, ok := os.LookupEnv(env)
		if !ok || v == "" {
			return c, fmt.Errorf("environment variable %s (secret_env) is not set", env)
		}
		c.secret = v
	}
	return c, nil
}

// Start implements probe.Probe.
func (p *Sync) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
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
	if p.live == nil {
		p.live = newClient(liveTimeout)
	}
	p.h.Set(probe.HealthOK, "polling "+c.url)
	go loop(ctx, c.tick, c.interval, out, func(ctx context.Context) probe.Observation {
		return p.poll(ctx, c)
	})
	return nil
}

// newClient builds an HTTP client with a per-request timeout and no
// keep-alive, so a stuck long poll never pins a connection.
func newClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	return &http.Client{Timeout: timeout, Transport: tr}
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

// response is one HTTP exchange, flattened for the probe's needs.
type response struct {
	status  int
	header  http.Header
	body    []byte
	latency time.Duration
	err     error
	timeout bool
	refused bool // transport failure: nothing answered
}

// get performs one GET with the given client.
func (p *Sync) get(ctx context.Context, client *http.Client, rawURL string) response {
	var r response
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		r.err, r.refused = err, true
		return r
	}
	req.Header.Set("User-Agent", userAgent)
	start := time.Now()
	resp, err := client.Do(req)
	r.latency = time.Since(start)
	if err != nil {
		r.err = err
		r.timeout = isTimeout(err)
		r.refused = !r.timeout
		return r
	}
	defer resp.Body.Close()
	r.status = resp.StatusCode
	r.header = resp.Header
	r.body, _ = io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	return r
}

// isTimeout reports whether err is a deadline or a network timeout.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ms converts a duration to fractional milliseconds.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// mark returns the time key was first seen, recording now when new.
func (p *Sync) mark(key string, now time.Time) time.Time {
	if p.since == nil {
		p.since = map[string]time.Time{}
	}
	if t, ok := p.since[key]; ok {
		return t
	}
	p.since[key] = now
	return now
}

// clear forgets a condition's first-seen time.
func (p *Sync) clear(key string) { delete(p.since, key) }

// poll does one round: health, then (with a table) shape and live poll.
func (p *Sync) poll(ctx context.Context, c config) probe.Observation {
	now := p.clock()
	o := probe.Observation{
		Target:  c.target,
		Probe:   KindSync,
		At:      now,
		Metrics: map[string]float64{},
		Detail:  map[string]any{"url": c.url},
	}
	if c.table != "" {
		o.Detail["table"] = c.table
	}

	hr := p.get(ctx, p.client, c.url+"/v1/health")
	if hr.err != nil {
		o.Metrics = nil
		if hr.timeout {
			o.Err = fmt.Sprintf("GET /v1/health timed out after %s", c.timeout.Round(time.Millisecond))
			p.clear(keyRefused)
		} else {
			o.Err = "GET /v1/health: " + hr.err.Error()
			o.Conditions = append(o.Conditions, model.Condition{
				Kind:   model.CondConnectionRefused,
				Ref:    c.url,
				Since:  p.mark(keyRefused, now),
				Detail: hr.err.Error(),
			})
		}
		o.Detail["last_error"] = o.Err
		p.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	p.clear(keyRefused)
	o.Metrics["latency_ms"] = ms(hr.latency)
	o.Detail["health_status"] = hr.status

	if hr.status == http.StatusUnauthorized || hr.status == http.StatusForbidden {
		o.Metrics = nil
		o.Err = fmt.Sprintf("GET /v1/health: HTTP %d, secret rejected", hr.status)
		o.Detail["last_error"] = o.Err
		p.h.Set(probe.HealthFailed, o.Err)
		return o
	}

	status := healthStatus(hr)
	o.Detail["status"] = status
	health, msg := probe.HealthOK, ""
	switch {
	case hr.status == http.StatusOK && status == statusActive:
		o.Metrics["ready"] = 1
		p.clear(keyNotReady)
	case hr.status == http.StatusOK || hr.status == http.StatusAccepted:
		o.Metrics["ready"] = 0
		detail := status
		switch status {
		case statusWaiting:
			detail = detailWaiting
		case statusStarting:
			detail = detailStarting
		case "":
			detail = fmt.Sprintf("HTTP %d", hr.status)
		}
		o.Conditions = append(o.Conditions, model.Condition{
			Kind: model.CondNotReady, Ref: c.url, Since: p.mark(keyNotReady, now), Detail: detail,
		})
		health, msg = probe.HealthDegraded, "electric is "+detail
	default:
		o.Metrics["ready"] = 0
		detail := fmt.Sprintf("HTTP %d %s", hr.status, http.StatusText(hr.status))
		o.Conditions = append(o.Conditions, model.Condition{
			Kind: model.CondNotReady, Ref: c.url, Since: p.mark(keyNotReady, now), Detail: detail,
		})
		health, msg = probe.HealthDegraded, "health endpoint returned "+detail
	}

	if c.table != "" {
		if h, m := p.shape(ctx, c, &o, now); h != probe.HealthOK && (health == probe.HealthOK || h == probe.HealthFailed) {
			health, msg = h, m
		}
	}
	p.h.Set(health, msg)
	return o
}

// healthStatus extracts "status" from a health body.
func healthStatus(r response) string {
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		return ""
	}
	return body.Status
}

// shapeURL builds a /v1/shape request URL.
func shapeURL(c config, params url.Values) string {
	params.Set("table", c.table)
	if c.secret != "" {
		params.Set("secret", c.secret)
	}
	return c.url + "/v1/shape?" + params.Encode()
}

// shape performs the initial shape request and one short live poll, filling
// metrics, conditions and detail on o. It returns the health it implies.
func (p *Sync) shape(ctx context.Context, c config, o *probe.Observation, now time.Time) (probe.HealthState, string) {
	sr := p.get(ctx, p.client, shapeURL(c, url.Values{"offset": {"-1"}}))
	if sr.err != nil {
		msg := "GET /v1/shape: " + sr.err.Error()
		if sr.timeout {
			msg = fmt.Sprintf("GET /v1/shape timed out after %s", c.timeout.Round(time.Millisecond))
		}
		o.Detail["shape_error"] = msg
		return probe.HealthDegraded, msg
	}
	o.Metrics["shape_ms"] = ms(sr.latency)
	o.Detail["shape_status"] = sr.status
	if h := sr.header.Get(electricHandle); h != "" {
		o.Detail["handle"] = h
	}
	if off := sr.header.Get(electricOffset); off != "" {
		o.Detail["offset"] = off
	}
	if names, n := schemaColumns(sr.header.Get(electricSchema)); n > 0 {
		o.Metrics["columns"] = float64(n)
		o.Detail["schema"] = names
	}

	switch {
	case sr.status == http.StatusOK:
		// fall through to the live poll below
	case sr.status == http.StatusUnauthorized || sr.status == http.StatusForbidden:
		msg := fmt.Sprintf("GET /v1/shape: HTTP %d, secret rejected", sr.status)
		o.Detail["shape_error"] = msg
		return probe.HealthFailed, msg
	case sr.status == http.StatusTooManyRequests:
		o.Metrics["busy"] = 1
		o.Detail["shape_error"] = "HTTP 429: Electric is busy, shape request throttled"
		return probe.HealthDegraded, "electric is busy (429)"
	case sr.status == http.StatusConflict:
		// The handle we did not have has expired; Electric hands out a fresh
		// one. Nothing is wrong, the next poll starts from it.
		o.Detail["shape_error"] = "HTTP 409: shape handle expired, fresh handle received"
		return probe.HealthOK, ""
	case sr.status == http.StatusNotFound || sr.status >= 500:
		detail := fmt.Sprintf("HTTP %d %s", sr.status, http.StatusText(sr.status))
		if m := bodyMessage(sr.body); m != "" {
			detail += ": " + m
		}
		o.Detail["shape_error"] = detail
		o.Conditions = append(o.Conditions, model.Condition{
			Kind: model.CondNotReady, Ref: c.table, Since: p.mark(keyNotReady, now), Detail: detail,
		})
		return probe.HealthDegraded, "shape " + c.table + ": " + detail
	default:
		detail := fmt.Sprintf("HTTP %d %s", sr.status, http.StatusText(sr.status))
		if m := bodyMessage(sr.body); m != "" {
			detail += ": " + m
		}
		o.Detail["shape_error"] = detail
		return probe.HealthDegraded, "shape " + c.table + ": " + detail
	}

	handle, offset := sr.header.Get(electricHandle), sr.header.Get(electricOffset)
	if handle == "" || offset == "" {
		o.Detail["live"] = "skipped: shape response carried no handle or offset"
		return probe.HealthOK, ""
	}
	lr := p.get(ctx, p.live, shapeURL(c, url.Values{"offset": {offset}, "handle": {handle}, "live": {"true"}}))
	switch {
	case lr.timeout:
		// A live poll holds until something changes; nothing did within our
		// window, which is the up-to-date case.
		o.Metrics["up_to_date"] = 1
		o.Detail["live"] = fmt.Sprintf("no change within %s", liveTimeout)
	case lr.err != nil:
		o.Detail["live"] = "error: " + lr.err.Error()
	case lr.status == http.StatusNoContent || lr.header.Get(electricUpToDte) != "":
		o.Metrics["up_to_date"] = 1
		o.Detail["live"] = fmt.Sprintf("HTTP %d up to date", lr.status)
		if off := lr.header.Get(electricOffset); off != "" {
			o.Detail["offset"] = off
		}
	case lr.status == http.StatusOK:
		o.Metrics["up_to_date"] = 0
		o.Detail["live"] = "HTTP 200 new data"
		if off := lr.header.Get(electricOffset); off != "" {
			o.Detail["offset"] = off
		}
	case lr.status == http.StatusTooManyRequests:
		o.Metrics["busy"] = 1
		o.Detail["live"] = "HTTP 429: Electric is busy"
	default:
		o.Detail["live"] = fmt.Sprintf("HTTP %d %s", lr.status, http.StatusText(lr.status))
	}
	return probe.HealthOK, ""
}

// schemaColumns parses the electric-schema header (a JSON object keyed by
// column name) and returns up to maxSchemaNames names plus the total count.
func schemaColumns(raw string) ([]string, int) {
	if raw == "" {
		return nil, 0
	}
	var cols map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &cols); err != nil || len(cols) == 0 {
		return nil, 0
	}
	names := make([]string, 0, len(cols))
	for k := range cols {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) > maxSchemaNames {
		names = names[:maxSchemaNames]
	}
	return names, len(cols)
}

// bodyMessage returns the "message" of a JSON error body, if any.
func bodyMessage(body []byte) string {
	var b struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return ""
	}
	return b.Message
}
