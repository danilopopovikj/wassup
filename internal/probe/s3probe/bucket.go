// Package s3probe implements s3.bucket, the probe of one bucket in an
// S3-compatible object store: AWS S3, Hetzner Object Storage, MinIO,
// DigitalOcean Spaces, Cloudflare R2. It signs its own requests (SigV4) and
// only ever reads: a HeadBucket for reachability and, when the binding asks
// for it, a paged ListObjectsV2 for what the bucket holds. The listing is
// not the default because it is not free: one request per 1000 objects,
// every round, on a store that may bill requests.
package s3probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"syscall"
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
		Kind: Kind,
		Source: "an S3-compatible object store (AWS S3, Hetzner Object Storage, MinIO, Spaces, R2): HeadBucket, one request a round; " +
			"with list_objects: true also a paged ListObjectsV2 under the optional prefix, one request per 1000 objects a round",
		Delivers: "latency_ms of HeadBucket; NotReady when the bucket does not exist, ConnectionRefused when the endpoint does not answer; " +
			"detail: region, endpoint. With list_objects: true also used_bytes and objects summed over the listing, last_write, " +
			"and disk_pct when quota_bytes is set; a bucket with more than max_objects keys (default 20000) is not sized and reports only lower bounds in the detail. " +
			"Without the listing the size and the object count are not read and not shown",
		SpecFields: []string{"bucket", "endpoint", "region", "list_objects", "prefix", "path_style", "access_key_env", "secret_key_env", "session_token_env", "quota_bytes", "max_objects", "interval", "timeout"},
		Needs: "read-only credentials in the environment variables named by access_key_env and secret_key_env (default AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY), " +
			"allowed s3:ListBucket on the bucket, which HeadBucket and the listing both take. The check costs one request a round; " +
			"the listing costs one more per 1000 objects, which a store that bills requests charges for",
		Implemented: true,
		Facets:      []string{facet.NameStorage},
		Tier:        probe.TierToken,
	}, func() probe.Probe { return &Bucket{} })
}

// config is the parsed spec.
type config struct {
	target string
	bucket string
	base   string // the bucket's URL, ending in "/"
	region string
	prefix string
	creds  credentials
	// accessEnv and secretEnv name where the keys came from, for the
	// messages that say what to check.
	accessEnv, secretEnv string
	quota                facet.Num
	// listObjects asks for the listing; prefix and maxObjects only apply
	// with it.
	listObjects bool
	maxObjects  int
	tick        time.Duration
	interval    time.Duration
	timeout     time.Duration
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
	for _, k := range []string{"path_style", "list_objects"} {
		if v, ok := spec[k]; ok {
			if _, isBool := v.(bool); !isBool {
				return fmt.Errorf("%s must be true or false, got %T", k, v)
			}
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
	c.listObjects, _ = spec["list_objects"].(bool)
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

	c.accessEnv = probe.Str(spec, "access_key_env", defaultAccessEnv)
	c.secretEnv = probe.Str(spec, "secret_key_env", defaultSecretEnv)
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
		p.client = &http.Client{Timeout: c.timeout, Transport: probe.ReadOnly(nil)}
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

// Notes the detail carries about the listing.
const (
	// notListed is said when the binding did not ask for the listing.
	notListed = "object count not read; set list_objects: true to size the bucket, which lists every object under the prefix (one request per 1000 objects)"
	// needsListing is said of the settings that do nothing without it.
	needsListing = "without list_objects: true these settings do nothing: "
)

// keys names where the credentials come from.
func (c config) keys() string {
	if c.accessEnv == "" || c.secretEnv == "" {
		return "the access key and the secret key"
	}
	return "the keys in " + c.accessEnv + " and " + c.secretEnv
}

// host is the host of the bucket's URL, for messages.
func (c config) host() string {
	if u, err := url.Parse(c.base); err == nil && u.Host != "" {
		return u.Host
	}
	return c.base
}

// unreachable words a request that got no answer: what happened and what to
// check, with the request and the error of the client at the end.
func (c config) unreachable(method string, r response) string {
	cause := " (" + method + " " + c.base + ": " + r.err.Error() + ")"
	var dns *net.DNSError
	switch {
	case r.timeout:
		return fmt.Sprintf("no answer from %s within %s: check that this machine reaches the endpoint (VPN, firewall)", c.host(), c.timeout.Round(time.Millisecond)) + cause
	case errors.As(r.err, &dns):
		return fmt.Sprintf("the name %s does not resolve: check endpoint and region, and path_style for a store that has no name per bucket", dns.Name) + cause
	case errors.Is(r.err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(r.err.Error()), "connection refused"):
		return fmt.Sprintf("connection refused on %s: nothing listens there. Check the endpoint and its port", c.host()) + cause
	case isCertificate(r.err):
		return fmt.Sprintf("the certificate of %s was not accepted: check that endpoint is the name of the store, and http or https", c.host()) + cause
	}
	return method + " " + c.base + ": " + r.err.Error()
}

// isCertificate reports whether the store's certificate was not accepted.
func isCertificate(err error) bool {
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

// denied words a request the store turned down.
func (c config) denied(method string, status int) string {
	return fmt.Sprintf("access denied to bucket %s: check %s, and that they are allowed s3:ListBucket on the bucket (%s %s: HTTP %d)",
		c.bucket, c.keys(), method, c.base, status)
}

// listError words a listing that failed.
func (c config) listError(err error) string {
	var failed *statusError
	if errors.As(err, &failed) && (failed.status == http.StatusForbidden || failed.status == http.StatusUnauthorized) {
		return fmt.Sprintf("the bucket answers, the listing was denied: %s are allowed to check the bucket but not to list it; allow s3:ListBucket, or take list_objects out (%s)",
			c.keys(), err)
	}
	return "list: " + err.Error()
}

// statusError is a request the store answered with an error.
type statusError struct {
	status int
	detail string
}

// Error implements error.
func (e *statusError) Error() string { return e.detail }

// poll does one round: HeadBucket, then the listing when the binding asks
// for it. The storage facet writes the canonical form; the probe's
// first-seen tracker keeps NotReady and ConnectionRefused Since stable
// across rounds.
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
		o.Err = c.unreachable(http.MethodHead, head)
		if !head.timeout {
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
		p.h.Set(probe.HealthDegraded, s.NotReadyDetail+" at "+c.host()+": check the name of the bucket, the endpoint and the region")
		return o
	case http.StatusForbidden, http.StatusUnauthorized:
		o.Metrics = nil
		o.Err = c.denied(http.MethodHead, head.status)
		o.Detail["last_error"] = o.Err
		p.h.Set(probe.HealthFailed, o.Err)
		return o
	case http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		o.Metrics = nil
		o.Err = fmt.Sprintf("bucket %s lives in another region than %s", c.bucket, c.region)
		if r := head.header.Get("x-amz-bucket-region"); r != "" {
			o.Err += ": set region to " + r
		} else {
			o.Err += ": check region and endpoint"
		}
		o.Err += fmt.Sprintf(" (HEAD %s: HTTP %d)", c.base, head.status)
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

	if !c.listObjects {
		// The size and the count were not read: they stay out of the
		// numbers, and the detail says how to get them and what it costs.
		o.Detail["size"] = notListed
		if ignored := c.needListing(); ignored != "" {
			o.Detail["ignored"] = needsListing + ignored
		}
		facet.EmitStorage(&o, s, now)
		p.h.Set(probe.HealthOK, "")
		return o
	}

	health, msg := probe.HealthOK, ""
	objects, used, last, complete, err := p.list(ctx, c)
	switch {
	case err != nil:
		msg = c.listError(err)
		o.Detail["list_error"] = msg
		health = probe.HealthDegraded
	case complete:
		s.Objects, s.UsedBytes, s.LastWrite = facet.NI(objects), facet.N(float64(used)), last
	default:
		// The bucket is bigger than the listing budget: a partial sum is
		// not the size, so it stays out of the metrics.
		o.Detail["objects_at_least"] = objects
		o.Detail["used_bytes_at_least"] = used
		o.Detail["listing_capped_at"] = c.maxObjects
		msg = fmt.Sprintf("bucket listing skipped, more than %d objects: set prefix to size a part of the bucket, or raise max_objects, at one request per %d objects a round",
			c.maxObjects, pageSize)
		o.Detail["size"] = msg
		health = probe.HealthDegraded
	}
	facet.EmitStorage(&o, s, now)
	p.h.Set(health, msg)
	return o
}

// needListing names the settings of the binding that do nothing without the
// listing, "" when there are none.
func (c config) needListing() string {
	var set []string
	if c.prefix != "" {
		set = append(set, "prefix")
	}
	if c.maxObjects != defaultMaxObjects {
		set = append(set, "max_objects")
	}
	if c.quota.Set {
		set = append(set, "quota_bytes")
	}
	return strings.Join(set, ", ")
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
			return objects, used, last, false, &statusError{status: r.status, detail: errorDetail(r)}
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
