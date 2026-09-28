// Package hatchet implements the Hatchet (hatchet.run) probes of wassup:
// hatchet.queue for a queue component, hatchet.workers for the worker
// deployment, hatchet.workflow for a job component and hatchet.health for the
// engine itself.
//
// The probes read the Hatchet REST API with net/http only. Each one polls at
// a configurable interval (default 15s, never below 5s) and re-emits the last
// observation every tick in between, so the engine can tell a slow interval
// from a dead probe. The API token comes from the environment variable named
// by token_env (default HATCHET_CLIENT_TOKEN); the tenant id comes from the
// spec or, when absent, from the token's JWT payload (claim tenant_id, then
// sub), decoded without verification.
package hatchet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

const (
	// defaultTokenEnv names the environment variable holding the API token
	// when the spec does not set token_env.
	defaultTokenEnv = "HATCHET_CLIENT_TOKEN"
	// defaultInterval is the poll interval when the spec does not set one.
	defaultInterval = 15 * time.Second
	// minInterval is the lowest poll interval a spec may ask for.
	minInterval = 5 * time.Second
	// defaultTimeout bounds one API call when the spec does not set timeout.
	defaultTimeout = 10 * time.Second
	// defaultTick is used when the runtime did not inject spec["_tick"].
	defaultTick = 5 * time.Second
	// defaultPages caps how many pages a paginated list follows.
	defaultPages = 5
	// workflowPages caps the pages of the workflow list, which can be long.
	workflowPages = 10
	// pageSize is the page size requested from paginated endpoints.
	pageSize = 100
	// maxBody bounds one response body.
	maxBody = 16 << 20
	// errBodyLen bounds how much of an error body is kept in messages.
	errBodyLen = 200
	// userAgent identifies wassup to the Hatchet API.
	userAgent = "wassup-hatchet"
)

// commonFields are the spec fields every probe of this package accepts.
var commonFields = []string{"url", "token_env", "tenant", "interval", "timeout"}

// withFields returns the common spec fields followed by extra.
func withFields(extra ...string) []string {
	return append(append([]string(nil), commonFields...), extra...)
}

// config is the parsed common part of a spec.
type config struct {
	base     string
	tokenEnv string
	tenant   string
	interval time.Duration
	timeout  time.Duration
	tick     time.Duration
	target   string
}

// requirements says what configure must be able to resolve.
type requirements struct {
	token  bool // the token must be present in the environment
	tenant bool // a tenant id must come from the spec or the token
}

// validateCommon checks the shared spec fields offline.
func validateCommon(spec map[string]any) error {
	if err := probe.RequireString(spec, "url"); err != nil {
		return err
	}
	if _, err := parseBase(probe.Str(spec, "url", "")); err != nil {
		return err
	}
	if v, ok := spec["tenant"]; ok {
		if _, isStr := v.(string); !isStr {
			return fmt.Errorf("tenant must be a string, got %T", v)
		}
	}
	return validateDurations(spec, "interval", "timeout")
}

// validateDurations checks that every present key parses as a duration.
func validateDurations(spec map[string]any, keys ...string) error {
	for _, k := range keys {
		v, ok := spec[k]
		if !ok || v == nil {
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			return fmt.Errorf("%s must be a duration string like \"30s\", got %T", k, v)
		}
		if _, err := time.ParseDuration(s); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// parseBase validates the API base URL and normalizes it: no trailing slash
// and no trailing /api, since every path this package requests starts with
// /api.
func parseBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("url %q must be an absolute http(s) URL", raw)
	}
	base := strings.TrimRight(raw, "/")
	base = strings.TrimSuffix(base, "/api")
	return base, nil
}

// configure reads the common spec fields, the token and the tenant, and
// builds the API client. The error is a misconfiguration: the caller reports
// failed health.
func configure(spec map[string]any, req requirements) (config, *client, error) {
	if err := validateCommon(spec); err != nil {
		return config{}, nil, err
	}
	cfg := config{
		tokenEnv: probe.Str(spec, "token_env", defaultTokenEnv),
		tenant:   probe.Str(spec, "tenant", ""),
		interval: probe.Dur(spec, "interval", defaultInterval),
		timeout:  probe.Dur(spec, "timeout", defaultTimeout),
		tick:     tickOf(spec),
		target:   probe.Str(spec, "_target", ""),
	}
	base, err := parseBase(probe.Str(spec, "url", ""))
	if err != nil {
		return config{}, nil, err
	}
	cfg.base = base
	if cfg.interval < minInterval {
		cfg.interval = minInterval
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultTimeout
	}
	token, ok := os.LookupEnv(cfg.tokenEnv)
	if (!ok || token == "") && req.token {
		return config{}, nil, fmt.Errorf("environment variable %s (token_env) is not set", cfg.tokenEnv)
	}
	if cfg.tenant == "" && req.tenant {
		tenant, err := tenantFromToken(token)
		if err != nil {
			return config{}, nil, fmt.Errorf("tenant is not set and could not be read from the token in %s: %w", cfg.tokenEnv, err)
		}
		cfg.tenant = tenant
	}
	c := &client{
		base:   cfg.base,
		token:  token,
		tenant: cfg.tenant,
		http:   &http.Client{Timeout: cfg.timeout},
	}
	return cfg, c, nil
}

// tenantFromToken extracts the tenant id from a Hatchet API token: the JWT
// payload's tenant_id claim, or sub. The signature is not verified; the id
// is only used to build request paths the server authorizes anyway.
func tenantFromToken(token string) (string, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return "", errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("token payload: %w", err)
	}
	var claims struct {
		TenantID string `json:"tenant_id"`
		Sub      string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("token payload: %w", err)
	}
	switch {
	case claims.TenantID != "":
		return claims.TenantID, nil
	case claims.Sub != "":
		return claims.Sub, nil
	}
	return "", errors.New("token has neither a tenant_id nor a sub claim")
}

// tickOf returns the runtime tick injected into the spec, or the default.
func tickOf(spec map[string]any) time.Duration {
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		return d
	}
	return defaultTick
}

// client is a minimal read-only Hatchet API client.
type client struct {
	base   string
	token  string
	tenant string
	http   *http.Client

	mu        sync.Mutex
	workflows map[string]string // workflow name -> id
}

// tenantPath builds /api/v1/tenants/{tenant}<suffix>.
func (c *client) tenantPath(suffix string) string {
	return "/api/v1/tenants/" + url.PathEscape(c.tenant) + suffix
}

// stablePath builds /api/v1/stable/tenants/{tenant}<suffix>.
func (c *client) stablePath(suffix string) string {
	return "/api/v1/stable/tenants/" + url.PathEscape(c.tenant) + suffix
}

// apiError is a non-2xx answer from the API.
type apiError struct {
	Status int
	Path   string
	Body   string
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("hatchet API %s: HTTP %d", e.Path, e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// statusOf returns the HTTP status behind err, or 0.
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// rejected reports whether the token was refused (401 or 403): the binding
// cannot work until the token changes.
func rejected(err error) bool {
	s := statusOf(err)
	return s == http.StatusUnauthorized || s == http.StatusForbidden
}

// notFound reports whether err is a 404.
func notFound(err error) bool { return statusOf(err) == http.StatusNotFound }

// healthFor classifies a read error: a rejected token fails the probe,
// anything else (connection errors, 5xx, decode errors) degrades it.
func healthFor(err error) (probe.HealthState, string) {
	if rejected(err) {
		return probe.HealthFailed, "token rejected: " + err.Error()
	}
	return probe.HealthDegraded, err.Error()
}

// do performs one GET and returns the status, body and latency. Only
// transport failures are errors here; callers decide about the status.
func (c *client) do(ctx context.Context, path string, query url.Values) (int, []byte, time.Duration, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, 0, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	start := time.Now()
	resp, err := c.http.Do(req)
	latency := time.Since(start)
	if err != nil {
		return 0, nil, latency, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, nil, latency, err
	}
	return resp.StatusCode, body, latency, nil
}

// get performs one GET and decodes the JSON body into out.
func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	status, body, _, err := c.do(ctx, path, query)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return &apiError{Status: status, Path: path, Body: errorText(body)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("hatchet API %s: decode: %w", path, err)
	}
	return nil
}

// errorText pulls a short message out of an error body: Hatchet's
// {"errors": [{"description": ...}]} when present, else the trimmed body.
func errorText(body []byte) string {
	var e struct {
		Errors []struct {
			Description string `json:"description"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Errors) > 0 && e.Errors[0].Description != "" {
		return truncate(e.Errors[0].Description, errBodyLen)
	}
	return truncate(strings.TrimSpace(string(body)), errBodyLen)
}

// truncate shortens s to at most n runes, marking the cut with an ellipsis.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// page is the envelope of Hatchet's paginated lists.
type page[T any] struct {
	Rows       []T `json:"rows"`
	Pagination struct {
		CurrentPage *int64 `json:"current_page"`
		NextPage    *int64 `json:"next_page"`
		NumPages    *int64 `json:"num_pages"`
	} `json:"pagination"`
}

// listAll GETs a paginated list with offset/limit paging and follows
// pagination.next_page for at most maxPages pages.
func listAll[T any](ctx context.Context, c *client, path string, query url.Values, maxPages int) ([]T, error) {
	var out []T
	offset := 0
	for n := 0; n < maxPages; n++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("limit", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(offset))
		var p page[T]
		if err := c.get(ctx, path, q, &p); err != nil {
			return out, err
		}
		out = append(out, p.Rows...)
		next, cur := p.Pagination.NextPage, p.Pagination.CurrentPage
		if len(p.Rows) == 0 || next == nil || (cur != nil && *next <= *cur) {
			return out, nil
		}
		offset += len(p.Rows)
	}
	return out, nil
}

// apiTime is an RFC3339 timestamp that tolerates null and unknown formats
// (which read as zero) so one odd field never fails a whole decode.
type apiTime struct{ time.Time }

// UnmarshalJSON implements json.Unmarshaler.
func (t *apiTime) UnmarshalJSON(b []byte) error {
	t.Time = time.Time{}
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00"} {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v
			return nil
		}
	}
	return nil
}

// firstOf returns the first non-zero time.
func firstOf(ts ...apiTime) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t.Time
		}
	}
	return time.Time{}
}

// rfc3339 formats a time for a query parameter.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// run polls at interval and re-emits the last observation every tick in
// between (with a fresh At), until ctx ends.
func run(ctx context.Context, out chan<- probe.Observation, tick, interval time.Duration, poll func(ctx context.Context) probe.Observation) {
	if interval < tick {
		interval = tick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	var last probe.Observation
	var lastPoll time.Time
	for {
		now := time.Now()
		if lastPoll.IsZero() || now.Sub(lastPoll) >= interval {
			last = poll(ctx)
			lastPoll = now
			if ctx.Err() != nil {
				return
			}
		}
		o := last
		o.At = now
		if !probe.Send(ctx, out, o) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// firstSeen remembers when a condition key was first seen, so Since is
// stable across polls.
type firstSeen map[string]time.Time

// mark returns the first-seen time of key, recording now for a new key.
func (s firstSeen) mark(key string, now time.Time) time.Time {
	if t, ok := s[key]; ok {
		return t
	}
	s[key] = now
	return now
}

// keep forgets every key not in live.
func (s firstSeen) keep(live map[string]bool) {
	for k := range s {
		if !live[k] {
			delete(s, k)
		}
	}
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

// percentile returns the nearest-rank p-th percentile of vs (p in 0..100).
func percentile(vs []float64, p float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vs...)
	sort.Float64s(sorted)
	rank := int(p/100*float64(len(sorted)) + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// errObservation builds the observation for a failed read and records the
// matching health.
func errObservation(h *probe.Health, kind, target string, now time.Time, err error) probe.Observation {
	state, msg := healthFor(err)
	h.Set(state, msg)
	return probe.Observation{Target: target, Probe: kind, At: now, Err: err.Error()}
}
