// Package hcloudprobe implements the Hetzner Cloud probes of wassup:
// hcloud.lb for a load balancer and its per-target edges, hcloud.firewall
// for a firewall and the edges it may block.
//
// The probes talk to the Cloud API (https://api.hetzner.cloud/v1) with
// net/http and decode the same JSON schema hcloud-go/v2 uses. hcloud-go
// itself is not linked: its hcloud package pulls in prometheus/client_golang,
// which is not available in this build. The API is rate limited (3600
// requests per hour per token), so both probes poll at a configurable
// interval and re-emit the last observation every tick in between.
package hcloudprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// DefaultEndpoint is the Cloud API base URL.
const DefaultEndpoint = "https://api.hetzner.cloud/v1"

// defaultTokenEnv names the environment variable holding the API token when
// the spec does not set token_env.
const defaultTokenEnv = "HCLOUD_TOKEN"

// requestTimeout bounds one API call.
const requestTimeout = 10 * time.Second

// defaultTick is used when the runtime did not inject one.
const defaultTick = 5 * time.Second

// client is a minimal read-only Cloud API client.
type client struct {
	endpoint string
	token    string
	http     *http.Client
}

// newClient reads the token from the environment variable named by
// spec["token_env"] (default HCLOUD_TOKEN). The endpoint can be overridden
// with spec["endpoint"], mainly for tests.
func newClient(spec map[string]any) (*client, error) {
	env := probe.Str(spec, "token_env", defaultTokenEnv)
	token, ok := os.LookupEnv(env)
	if !ok || token == "" {
		return nil, fmt.Errorf("environment variable %s (token_env) is not set", env)
	}
	return &client{
		endpoint: strings.TrimRight(probe.Str(spec, "endpoint", DefaultEndpoint), "/"),
		token:    token,
		http:     &http.Client{Timeout: requestTimeout},
	}, nil
}

// apiError is a non-2xx answer from the API.
type apiError struct {
	Status int
	Code   string
	Msg    string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("hcloud API: HTTP %d %s: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("hcloud API: HTTP %d", e.Status)
}

// misconfigured reports whether an error means the binding can never work
// (bad token, forbidden) rather than a transient problem (rate limit, 5xx).
func misconfigured(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden
	}
	return false
}

// get performs one GET and decodes the JSON body into out.
func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.endpoint + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wassup-hcloudprobe")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &apiError{Status: resp.StatusCode}
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil {
			ae.Code, ae.Msg = e.Error.Code, e.Error.Message
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("hcloud API %s: decode: %w", path, err)
	}
	return nil
}

// resourceRef is a spec "name" or "id" pointing at one API resource.
type resourceRef struct {
	ID   int64
	Name string
}

// refFromSpec reads name or id; id may be a number or a numeric string.
func refFromSpec(spec map[string]any) (resourceRef, error) {
	var r resourceRef
	if n, ok := probe.Num(spec, "id"); ok && n > 0 {
		r.ID = int64(n)
	} else if s := probe.Str(spec, "id", ""); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			return r, fmt.Errorf("id must be a positive integer, got %q", s)
		}
		r.ID = id
	}
	r.Name = probe.Str(spec, "name", "")
	if r.ID == 0 && r.Name == "" {
		return r, errors.New(`"name" or "id" is required`)
	}
	return r, nil
}

// lookup resolves a ref to an id through GET /<collection>?name= when only
// the name is known. The resolved id is cached in the ref.
func (c *client) lookup(ctx context.Context, collection string, r *resourceRef) error {
	if r.ID != 0 {
		return nil
	}
	var resp map[string]json.RawMessage
	if err := c.get(ctx, "/"+collection, url.Values{"name": {r.Name}}, &resp); err != nil {
		return err
	}
	var items []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if raw, ok := resp[collection]; ok {
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("hcloud API /%s: decode: %w", collection, err)
		}
	}
	for _, it := range items {
		if it.Name == r.Name {
			r.ID = it.ID
			return nil
		}
	}
	return fmt.Errorf("hcloud: no %s named %q", strings.TrimSuffix(collection, "s"), r.Name)
}

// tick returns the injected tick or the default.
func tick(spec map[string]any) time.Duration {
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		return d
	}
	return defaultTick
}

// target returns the bound element id.
func target(spec map[string]any) string { return probe.Str(spec, "_target", "") }

// validateCommon checks the fields every hcloud probe shares.
func validateCommon(spec map[string]any) error {
	if _, err := refFromSpec(spec); err != nil {
		return err
	}
	if v, ok := spec["interval"].(string); ok {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("interval: %w", err)
		}
	}
	if ep := probe.Str(spec, "endpoint", ""); ep != "" {
		u, err := url.Parse(ep)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return errors.New("endpoint must be an http(s) URL")
		}
	}
	return nil
}

// run polls at interval and re-emits the last observations every tick in
// between, until ctx ends.
func run(ctx context.Context, out chan<- probe.Observation, every, interval time.Duration, poll func(ctx context.Context) []probe.Observation) {
	if interval < every {
		interval = every
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var last []probe.Observation
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
		for _, o := range last {
			o.At = now
			if !probe.Send(ctx, out, o) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// since remembers when a condition key was first seen.
type since map[string]time.Time

// mark returns the first-seen time of key, recording now for a new key.
func (s since) mark(key string, now time.Time) time.Time {
	if t, ok := s[key]; ok {
		return t
	}
	s[key] = now
	return now
}

// keep forgets every key not in live.
func (s since) keep(live map[string]bool) {
	for k := range s {
		if !live[k] {
			delete(s, k)
		}
	}
}
