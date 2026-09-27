package netprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// KindPing is the probe kind of the HTTP ping.
const KindPing = "http.ping"

const (
	// pingWindow is the number of attempts error_rate and timeout_rate are
	// computed over.
	pingWindow = 10
	// pingStreak is the number of consecutive failed attempts that raise the
	// Timeout condition.
	pingStreak = 3
	// pingMaxRedirects bounds how far a ping follows redirects.
	pingMaxRedirects = 3
	// userAgent identifies wassup to the pinged service.
	userAgent = "wassup/1 (+https://github.com/danilopopovikj/wassup)"
)

func init() {
	probe.Register(probe.Access{
		Kind:        KindPing,
		Source:      "an HTTP endpoint",
		Delivers:    "latency_ms, error_rate and timeout_rate over the last 10 attempts, the last status, Timeout after 3 consecutive failures",
		SpecFields:  []string{"url", "method", "timeout", "interval", "expect_status"},
		Needs:       "outbound HTTPS to the endpoint; no credentials",
		Implemented: true,
	}, func() probe.Probe { return &Ping{} })
}

// attempt is the outcome of one ping.
type attempt struct {
	at      time.Time
	latency time.Duration
	status  int
	timeout bool // the request timed out
	failed  bool // transport failure (including timeouts) or unexpected status
	err     string
}

// Ping is the http.ping probe. A failed request is data about the external
// dependency, not a probe error: the probe stays healthy and reports the
// failure through error_rate, timeout_rate and the Timeout condition. Only a
// spec that cannot be turned into a request fails the probe itself.
type Ping struct {
	h probe.Health

	// now and client are overridable for tests.
	now    func() time.Time
	client *http.Client

	window []attempt
	streak []attempt // consecutive failed attempts, oldest first
}

// Kind implements probe.Probe.
func (p *Ping) Kind() string { return KindPing }

// Validate implements probe.Probe.
func (p *Ping) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "url"); err != nil {
		return err
	}
	u, err := url.Parse(probe.Str(spec, "url", ""))
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("url %q must be an absolute http(s) URL", u)
	}
	switch m := strings.ToUpper(probe.Str(spec, "method", http.MethodHead)); m {
	case http.MethodHead, http.MethodGet:
	default:
		return fmt.Errorf("method %q is not allowed, use HEAD or GET", m)
	}
	if v, ok := spec["expect_status"]; ok {
		if n, isNum := probe.Num(spec, "expect_status"); !isNum || n < 100 || n > 599 {
			return fmt.Errorf("expect_status %v must be an HTTP status code", v)
		}
	}
	if v, ok := spec["timeout"].(string); ok && v != "" {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
	}
	return nil
}

// Health implements probe.Probe.
func (p *Ping) Health() probe.ProbeHealth { return p.h.Get() }

// clock returns the current time from the test hook or the wall clock.
func (p *Ping) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Start implements probe.Probe.
func (p *Ping) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := p.Validate(spec); err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	target := targetOf(spec)
	rawURL := probe.Str(spec, "url", "")
	method := strings.ToUpper(probe.Str(spec, "method", http.MethodHead))
	timeout := probe.Dur(spec, "timeout", 5*time.Second)
	tick := tickOf(spec)
	interval := probe.Dur(spec, "interval", tick)
	expect := 0
	if n, ok := probe.Num(spec, "expect_status"); ok {
		expect = int(n)
	}
	if p.client == nil {
		p.client = newClient(timeout)
	}
	p.h.Set(probe.HealthOK, "pinging "+rawURL)
	go loop(ctx, tick, interval, out, func(ctx context.Context) probe.Observation {
		a := p.ping(ctx, method, rawURL, timeout, expect)
		return p.observe(target, rawURL, a)
	})
	return nil
}

// newClient builds the HTTP client: a per-request timeout, at most
// pingMaxRedirects redirects and no cookies or keep-alive surprises.
func newClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= pingMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", pingMaxRedirects)
			}
			return nil
		},
	}
}

// ping performs one request and classifies the outcome.
func (p *Ping) ping(ctx context.Context, method, rawURL string, timeout time.Duration, expect int) attempt {
	a := attempt{at: p.clock()}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, method, rawURL, nil)
	if err != nil {
		a.failed = true
		a.err = err.Error()
		return a
	}
	req.Header.Set("User-Agent", userAgent)
	start := time.Now()
	resp, err := p.client.Do(req)
	a.latency = time.Since(start)
	if err != nil {
		a.failed = true
		a.timeout = isTimeout(err)
		if a.timeout {
			a.err = fmt.Sprintf("%s timed out after %s", method, timeout.Round(time.Millisecond))
		} else {
			a.err = err.Error()
		}
		return a
	}
	resp.Body.Close()
	a.status = resp.StatusCode
	switch {
	case expect != 0 && a.status != expect:
		a.failed = true
		a.err = fmt.Sprintf("status %d, expected %d", a.status, expect)
	case expect == 0 && a.status >= 500:
		a.failed = true
		a.err = fmt.Sprintf("status %d", a.status)
	}
	return a
}

// isTimeout reports whether err is a deadline, timeout or refused/unreachable
// connection: the "could not reach it" family that counts toward the Timeout
// streak as a timeout rather than an application error.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// isConnectFailure reports whether err is a transport-level failure (as
// opposed to an HTTP response the probe did not like).
func isConnectFailure(a attempt) bool {
	return a.failed && a.status == 0
}

// observe folds one attempt into the rolling window and builds the observation.
func (p *Ping) observe(target, rawURL string, a attempt) probe.Observation {
	p.window = append(p.window, a)
	if len(p.window) > pingWindow {
		p.window = p.window[len(p.window)-pingWindow:]
	}
	if isConnectFailure(a) {
		p.streak = append(p.streak, a)
	} else {
		p.streak = p.streak[:0]
	}

	var errs, timeouts int
	for _, w := range p.window {
		if w.failed {
			errs++
		}
		if w.timeout {
			timeouts++
		}
	}
	n := float64(len(p.window))
	o := probe.Observation{
		Target: target,
		Probe:  KindPing,
		At:     a.at,
		Metrics: map[string]float64{
			"latency_ms":   float64(a.latency) / float64(time.Millisecond),
			"error_rate":   100 * float64(errs) / n,
			"timeout_rate": 100 * float64(timeouts) / n,
			"status":       float64(a.status),
		},
		Detail: map[string]any{
			"url":    rawURL,
			"status": a.status,
			"window": len(p.window),
		},
	}
	if a.err != "" {
		o.Detail["last_error"] = a.err
	}
	if len(p.streak) >= pingStreak {
		o.Conditions = append(o.Conditions, model.Condition{
			Kind:   model.CondTimeout,
			Ref:    rawURL,
			Since:  p.streak[0].at,
			Detail: a.err,
		})
	}
	return o
}
