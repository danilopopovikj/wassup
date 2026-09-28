// Package s3probe implements s3.bucket, the probe of one bucket in an
// S3-compatible object store: AWS S3, Hetzner Object Storage, MinIO,
// DigitalOcean Spaces, Cloudflare R2. It signs its own requests (SigV4) and
// only ever reads: a HeadBucket for reachability and a paged ListObjectsV2
// for what the bucket holds.
package s3probe

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// Kind is the probe kind.
const Kind = "s3.bucket"

const (
	defaultTick       = 5 * time.Second
	defaultInterval   = time.Minute // a listing is not free; once a minute is plenty for a bucket
	minInterval       = 15 * time.Second
	defaultTimeout    = 10 * time.Second
	defaultRegion     = "us-east-1"
	defaultAccessEnv  = "AWS_ACCESS_KEY_ID"
	defaultSecretEnv  = "AWS_SECRET_ACCESS_KEY"
	defaultMaxObjects = 20000
	pageSize          = 1000
	maxBodyBytes      = 8 << 20
	userAgent         = "wassup/1 (+https://github.com/danilopopovikj/wassup)"
)

func init() {
	probe.Register(probe.Access{
		Kind:   Kind,
		Source: "an S3-compatible object store (AWS S3, Hetzner Object Storage, MinIO, Spaces, R2): HeadBucket, then a paged ListObjectsV2 under the optional prefix",
		Delivers: "used_bytes and objects summed over the listing, latency_ms of HeadBucket, total_bytes and disk_pct when quota_bytes is set; " +
			"NotReady when the bucket does not exist, ConnectionRefused when the endpoint does not answer; detail: last_write, region, endpoint. " +
			"A bucket with more than max_objects keys reports only lower bounds in the detail",
		SpecFields:  []string{"bucket", "endpoint", "region", "prefix", "path_style", "access_key_env", "secret_key_env", "session_token_env", "quota_bytes", "max_objects", "interval", "timeout"},
		Needs:       "read-only credentials (s3:ListBucket) in the environment variables named by access_key_env and secret_key_env (default AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)",
		Implemented: true,
		Facets:      []string{facet.NameStorage},
	}, func() probe.Probe { return &Bucket{} })
}

// config is the parsed spec.
type config struct {
	target     string
	bucket     string
	base       string // the bucket's URL, ending in "/"
	region     string
	prefix     string
	creds      credentials
	quota      facet.Num
	maxObjects int
	tick       time.Duration
	interval   time.Duration
	timeout    time.Duration
}

// Bucket is the s3.bucket probe.
type Bucket struct {
	h probe.Health

	// now and client are overridable for tests.
	now    func() time.Time
	client *http.Client

	// since remembers when each condition was first seen so Since is stable.
	since map[string]time.Time
}

// Kind implements probe.Probe.
func (p *Bucket) Kind() string { return Kind }

// Validate implements probe.Probe.
func (p *Bucket) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "bucket"); err != nil {
		return err
	}
	if ep := probe.Str(spec, "endpoint", ""); ep != "" {
		u, err := url.Parse(ep)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("endpoint %q must be an absolute http(s) URL", ep)
		}
	}
	for _, k := range []string{"interval", "timeout"} {
		if v, ok := spec[k].(string); ok && v != "" {
			if _, err := time.ParseDuration(v); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	for _, k := range []string{"region", "prefix", "access_key_env", "secret_key_env", "session_token_env"} {
		if v, ok := spec[k]; ok {
			if _, isStr := v.(string); !isStr {
				return fmt.Errorf("%s must be a string, got %T", k, v)
			}
		}
	}
	if v, ok := spec["path_style"]; ok {
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("path_style must be true or false, got %T", v)
		}
	}
	for _, k := range []string{"quota_bytes", "max_objects"} {
		if v, ok := spec[k]; ok {
			n, isNum := probe.Num(spec, k)
			if !isNum || n <= 0 {
				return fmt.Errorf("%s must be a positive number, got %v", k, v)
			}
		}
	}
	return nil
}

// Health implements probe.Probe.
func (p *Bucket) Health() probe.ProbeHealth { return p.h.Get() }

// clock returns the current time from the test hook or the wall clock.
func (p *Bucket) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// parse turns a spec into a config, reading the credentials from the
// environment. Without an endpoint the bucket is on AWS, addressed by
// virtual host; with one it is addressed by path unless path_style is
// false, which is what MinIO, Hetzner and most self-hosted stores expect.
func parse(spec map[string]any) (config, error) {
	c := config{
		target:     probe.Str(spec, "_target", ""),
		bucket:     probe.Str(spec, "bucket", ""),
		region:     probe.Str(spec, "region", defaultRegion),
		prefix:     probe.Str(spec, "prefix", ""),
		maxObjects: defaultMaxObjects,
		tick:       defaultTick,
		timeout:    probe.Dur(spec, "timeout", defaultTimeout),
	}
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		c.tick = d
	}
	c.interval = max(probe.Dur(spec, "interval", defaultInterval), minInterval, c.tick)
	if n, ok := probe.Num(spec, "max_objects"); ok && n > 0 {
		c.maxObjects = int(n)
	}
	if n, ok := probe.Num(spec, "quota_bytes"); ok && n > 0 {
		c.quota = facet.N(n)
	}

	endpoint := strings.TrimRight(probe.Str(spec, "endpoint", ""), "/")
	switch {
	case endpoint == "":
		c.base = "https://" + c.bucket + ".s3." + c.region + ".amazonaws.com/"
	default:
		pathStyle := true
		if v, ok := spec["path_style"].(bool); ok {
			pathStyle = v
		}
		if pathStyle {
			c.base = endpoint + "/" + c.bucket + "/"
		} else {
			u, _ := url.Parse(endpoint)
			u.Host = c.bucket + "." + u.Host
			c.base = u.String() + "/"
		}
	}

	var err error
	if c.creds.accessKey, err = envValue(spec, "access_key_env", defaultAccessEnv); err != nil {
		return c, err
	}
	if c.creds.secretKey, err = envValue(spec, "secret_key_env", defaultSecretEnv); err != nil {
		return c, err
	}
	if env := probe.Str(spec, "session_token_env", ""); env != "" {
		if c.creds.sessionToken, err = envValue(spec, "session_token_env", ""); err != nil {
			return c, err
		}
	}
	return c, nil
}

// envValue reads the environment variable named by spec[key] (or def) and
// fails when it is missing, so a bad deployment is a Start error and not a
// bucket that reads as unreachable.
func envValue(spec map[string]any, key, def string) (string, error) {
	env := probe.Str(spec, key, def)
	v, ok := os.LookupEnv(env)
	if !ok || v == "" {
		return "", fmt.Errorf("environment variable %s (%s) is not set", env, key)
	}
	return v, nil
}

// Start implements probe.Probe.
func (p *Bucket) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
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
		p.client = &http.Client{Timeout: c.timeout}
	}
	p.h.Set(probe.HealthOK, "polling "+c.base)
	go loop(ctx, c.tick, c.interval, out, func(ctx context.Context) probe.Observation {
		return p.poll(ctx, c)
	})
	return nil
}

// loop runs check once, then every interval, re-emitting the last
// observation every tick in between with a fresh At.
func loop(ctx context.Context, tick, interval time.Duration, out chan<- probe.Observation, check func(context.Context) probe.Observation) {
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
}

// do performs one signed, bodiless request.
func (p *Bucket) do(ctx context.Context, c config, method, rawURL string) response {
	var r response
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		r.err = err
		return r
	}
	req.Header.Set("User-Agent", userAgent)
	sign(req, c.creds, c.region, p.clock())
	start := time.Now()
	resp, err := p.client.Do(req)
	r.latency = time.Since(start)
	if err != nil {
		r.err = err
		r.timeout = isTimeout(err)
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

// s3Error is the XML body S3 returns with a non-2xx status.
type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// errorDetail turns an error response into one line.
func errorDetail(r response) string {
	var e s3Error
	if len(r.body) > 0 && xml.Unmarshal(r.body, &e) == nil && e.Code != "" {
		if e.Message != "" {
			return e.Code + ": " + e.Message
		}
		return e.Code
	}
	return fmt.Sprintf("HTTP %d %s", r.status, http.StatusText(r.status))
}

// listing is a ListObjectsV2 page.
type listing struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

// poll does one round: HeadBucket, then the listing. The storage facet writes
// the canonical form; the probe's first-seen tracker keeps NotReady and
// ConnectionRefused Since stable across rounds.
func (p *Bucket) poll(ctx context.Context, c config) probe.Observation {
	now := p.clock()
	o := probe.Observation{
		Target:  c.target,
		Probe:   Kind,
		At:      now,
		Metrics: map[string]float64{},
		Detail:  map[string]any{"bucket": c.bucket, "endpoint": c.base, "region": c.region},
	}
	if c.prefix != "" {
		o.Detail["prefix"] = c.prefix
	}
	s := facet.StorageFacet{TotalBytes: c.quota, Since: p.mark}

	head := p.do(ctx, c, http.MethodHead, c.base)
	if head.err != nil {
		if head.timeout {
			o.Err = fmt.Sprintf("HEAD %s timed out after %s", c.base, c.timeout.Round(time.Millisecond))
		} else {
			o.Err = "HEAD " + c.base + ": " + head.err.Error()
			s.Unreachable, s.UnreachDetail = true, head.err.Error()
			facet.EmitStorage(&o, s, now)
		}
		// The read failed: no numbers, only the condition and the error.
		o.Metrics = nil
		o.Detail["last_error"] = o.Err
		p.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	p.clear(facet.KeyTimeout)
	s.Latency = facet.N(float64(head.latency) / float64(time.Millisecond))

	switch head.status {
	case http.StatusOK:
	case http.StatusNotFound:
		s.NotReadyDetail = "bucket " + c.bucket + " does not exist"
		facet.EmitStorage(&o, s, now)
		p.h.Set(probe.HealthDegraded, s.NotReadyDetail)
		return o
	case http.StatusForbidden, http.StatusUnauthorized:
		o.Metrics = nil
		o.Err = fmt.Sprintf("HEAD %s: HTTP %d, access denied (check the credentials and s3:ListBucket)", c.base, head.status)
		o.Detail["last_error"] = o.Err
		p.h.Set(probe.HealthFailed, o.Err)
		return o
	case http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		o.Metrics = nil
		o.Err = fmt.Sprintf("HEAD %s: HTTP %d, the bucket lives elsewhere", c.base, head.status)
		if r := head.header.Get("x-amz-bucket-region"); r != "" {
			o.Err += " (region " + r + ")"
		}
		o.Detail["last_error"] = o.Err
		p.h.Set(probe.HealthFailed, o.Err)
		return o
	default:
		o.Metrics = nil
		o.Err = "HEAD " + c.base + ": " + errorDetail(head)
		o.Detail["last_error"] = o.Err
		p.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	p.clear(facet.KeyNotReady)

	health, msg := probe.HealthOK, ""
	objects, used, last, complete, err := p.list(ctx, c)
	switch {
	case err != nil:
		o.Detail["list_error"] = err.Error()
		health, msg = probe.HealthDegraded, "list: "+err.Error()
	case complete:
		s.Objects, s.UsedBytes, s.LastWrite = facet.NI(objects), facet.N(float64(used)), last
	default:
		// The bucket is bigger than the listing budget: a partial sum is
		// not the size, so it stays out of the metrics.
		o.Detail["objects_at_least"] = objects
		o.Detail["used_bytes_at_least"] = used
		o.Detail["listing_capped_at"] = c.maxObjects
		health, msg = probe.HealthDegraded, fmt.Sprintf("more than %d objects; set prefix or raise max_objects to size this bucket", c.maxObjects)
	}
	facet.EmitStorage(&o, s, now)
	p.h.Set(health, msg)
	return o
}

// list pages through ListObjectsV2 under the prefix until the bucket is
// exhausted or maxObjects keys have been seen. complete is false when it
// stopped early.
func (p *Bucket) list(ctx context.Context, c config) (objects int, used int64, last time.Time, complete bool, err error) {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "max-keys": {strconv.Itoa(pageSize)}}
		if c.prefix != "" {
			q.Set("prefix", c.prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		r := p.do(ctx, c, http.MethodGet, c.base+"?"+q.Encode())
		if r.err != nil {
			return objects, used, last, false, r.err
		}
		if r.status != http.StatusOK {
			return objects, used, last, false, errors.New(errorDetail(r))
		}
		var page listing
		if err := xml.Unmarshal(r.body, &page); err != nil {
			return objects, used, last, false, fmt.Errorf("parse listing: %w", err)
		}
		for _, obj := range page.Contents {
			objects++
			used += obj.Size
			if t, err := time.Parse(time.RFC3339Nano, obj.LastModified); err == nil && t.After(last) {
				last = t
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return objects, used, last, true, nil
		}
		if objects >= c.maxObjects {
			return objects, used, last, false, nil
		}
		token = page.NextContinuationToken
	}
}

// mark returns the time key was first seen, recording now when new.
func (p *Bucket) mark(key string, now time.Time) time.Time {
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
func (p *Bucket) clear(key string) { delete(p.since, key) }
