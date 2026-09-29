package signoz

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

const kindEdge = "signoz.edge"

const (
	// edgeInterval is how often a metric is asked for when the binding does
	// not say. SigNoz takes a new value of a counter once a minute, so asking
	// more often reads the same numbers again.
	edgeInterval = time.Minute
	// edgeWindow is what a rate is taken over. Five minutes keep a service
	// that is called now and then from reading idle between two calls.
	edgeWindow = 5 * time.Minute
	// step is the resolution SigNoz is asked for, in seconds.
	step = 60
	// minWindow is the shortest window a rate can be taken over: two steps.
	minWindow = 2 * step * time.Second
	// edgeKnown is how far back a series counts as known. A service that is
	// called now and then has no series in a window in which nobody called
	// it; that it was counted within the last week says its counter is there
	// and did not move, and when it last moved.
	edgeKnown = 7 * 24 * time.Hour
	// dayKnown is the first look back, read before the whole of known: most
	// quiet series were counted within it, and its steps of five minutes say
	// when closer than the hour a step of a week is.
	dayKnown = 24 * time.Hour
	// knownInterval is how often the day is read, by the bindings whose
	// window holds nothing of them.
	knownInterval = 10 * time.Minute
	// defaultTick is used when the runtime did not inject one.
	defaultTick = 5 * time.Second
)

func init() {
	probe.Register(probe.Access{
		Kind:   kindEdge,
		Source: "a counter SigNoz holds, such as the calls of one service to another, asked for with one read query per metric that the bindings share (POST /api/v5/query_range)",
		Delivers: "on an edge: rate (per second: what the series that match counted over the window, divided by the window) and error_rate (the percentage of it that the series matching errors make up); " +
			"on a database: rate, its transactions per second; a rate of 0 when the series that match were counted within known (a week) and not within the window; " +
			"no rate when no series matches in either; detail: the metric, the series matched, the window, count (what they counted in it), last_seen (the end of the last step that counted anything), unit",
		SpecFields: []string{"url (required)", "metric (required)", "match", "errors", "window", "known", "unit", "interval", "user_env", "password_env", "token_env"},
		Needs: "HTTP access to SigNoz, and a user with the viewer role: its name and password in the environment variables named by user_env and password_env " +
			"(default SIGNOZ_USER, SIGNOZ_PASSWORD), or an API key in the one named by token_env. wassup signs in with a POST and asks with a POST; neither changes anything in SigNoz. " +
			"A query costs SigNoz's database a read over the window, once per metric and interval, however many bindings use the metric; " +
			"while the window holds nothing of a binding, one more over the last day every ten minutes, in steps of five minutes, and when that holds nothing either, one over known (a week) in steps of an hour",
		Implemented: true,
		Facets:      []string{facet.NameTraffic, facet.NameDatabase},
		Tier:        probe.TierToken,
	}, func() probe.Probe { return &edgeProbe{} })
}

// edgeConfig is a validated signoz.edge spec.
type edgeConfig struct {
	base     string
	creds    credentials
	metric   string
	match    map[string]*regexp.Regexp
	errors   map[string]*regexp.Regexp
	labels   []string // the labels match and errors name, sorted
	interval time.Duration
	window   time.Duration
	known    time.Duration // 0 when the past is left alone
	unit     string        // what one count is, per second: "req/s", "queries/s"
}

// edgeConfigOf validates a spec.
func edgeConfigOf(spec map[string]any) (edgeConfig, error) {
	var cfg edgeConfig
	if err := probe.RequireString(spec, "url", "metric"); err != nil {
		return cfg, err
	}
	u, err := url.Parse(probe.Str(spec, "url", ""))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return cfg, fmt.Errorf("url must be the http(s) address of SigNoz, such as https://signoz.bookstore.example")
	}
	if u.User != nil {
		return cfg, fmt.Errorf("url must not carry a user or a password; name them with user_env and password_env")
	}
	cfg.base = strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
	cfg.creds = credentialsOf(spec)
	if cfg.metric = probe.Str(spec, "metric", ""); strings.ContainsAny(cfg.metric, "{ \t") {
		return cfg, fmt.Errorf("metric is the name of a counter alone; its labels go into match")
	}
	if cfg.match, err = probe.Matchers(spec, "match"); err != nil {
		return cfg, err
	}
	if cfg.errors, err = probe.Matchers(spec, "errors"); err != nil {
		return cfg, err
	}
	seen := map[string]bool{}
	for _, m := range []map[string]*regexp.Regexp{cfg.match, cfg.errors} {
		for label := range m {
			if !seen[label] {
				seen[label] = true
				cfg.labels = append(cfg.labels, label)
			}
		}
	}
	sort.Strings(cfg.labels)
	if cfg.unit, err = unitOf(spec, cfg.metric); err != nil {
		return cfg, err
	}
	if cfg.interval, err = probe.StrictDur(spec, "interval", edgeInterval); err != nil {
		return cfg, err
	}
	if cfg.window, err = probe.StrictDur(spec, "window", edgeWindow); err != nil {
		return cfg, err
	}
	if cfg.window < minWindow {
		cfg.window = minWindow // a rate takes two values of the counter
	}
	switch probe.Str(spec, "known", "") {
	case "0", "0s", "off":
	default:
		if cfg.known, err = probe.StrictDur(spec, "known", edgeKnown); err != nil {
			return cfg, fmt.Errorf("%w, or 0s to leave it out", err)
		}
	}
	return cfg, nil
}

// edgeProbe is signoz.edge.
type edgeProbe struct {
	h probe.Health
	// tables is the shared store, replaced in tests.
	tables *tableStore
}

// Kind implements probe.Probe.
func (p *edgeProbe) Kind() string { return kindEdge }

// Health implements probe.Probe.
func (p *edgeProbe) Health() probe.ProbeHealth { return p.h.Get() }

// Validate implements probe.Probe.
func (p *edgeProbe) Validate(spec map[string]any) error {
	_, err := edgeConfigOf(spec)
	return err
}

// Start implements probe.Probe.
func (p *edgeProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	cfg, err := edgeConfigOf(spec)
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	store := p.tables
	if store == nil {
		store = tables
	}
	sess := sessionFor(cfg.base, cfg.creds)
	// Said before the first query is sent, so that the bindings that start
	// together are answered by one query and not by one each.
	store.want(sess, cfg)
	target := probe.Str(spec, "_target", cfg.metric)
	tick := defaultTick
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		tick = d
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			rctx, cancel := context.WithTimeout(ctx, requestTimeout+5*time.Second)
			o, err := p.observe(rctx, store, sess, cfg, target)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				state := probe.HealthDegraded
				if misconfigured(err) {
					state = probe.HealthFailed
				}
				p.h.Set(state, err.Error())
				o = probe.Observation{Target: target, Probe: kindEdge, At: time.Now(), Err: err.Error()}
			} else {
				p.h.Set(probe.HealthOK, "")
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

// matched is what the series of a table that a binding means add up to.
type matched struct {
	total, failed float64 // per second
	count         float64 // over the window
	matched       int
	last          time.Time
}

// sum adds up the series of a table that a binding means.
func sum(rows []row, cfg edgeConfig) matched {
	var m matched
	for _, r := range rows {
		if !probe.Matches(r.labels, cfg.match) {
			continue
		}
		m.matched++
		m.total += r.value
		m.count += r.count
		if r.last.After(m.last) {
			m.last = r.last
		}
		if len(cfg.errors) > 0 && probe.Matches(r.labels, cfg.errors) {
			m.failed += r.value
		}
	}
	return m
}

// observe reads the metric's table and adds up the series the binding means.
// When the window holds none of them it looks further back, the last day
// first and then the whole of known: a series that was counted then is
// known, its rate now is zero, and when it last counted is said.
func (p *edgeProbe) observe(ctx context.Context, store *tableStore, sess *session, cfg edgeConfig, target string) (probe.Observation, error) {
	tab, err := store.read(ctx, sess, cfg)
	if err != nil {
		return probe.Observation{}, err
	}
	m := sum(tab.rows, cfg)
	rows, held, quiet := tab.rows, cfg.window, false
	var pastErr error
	if m.matched == 0 {
		for _, back := range lookBack(cfg) {
			past := cfg
			past.window, past.interval = back, knownInterval
			pt, err := store.read(ctx, sess, past)
			if err != nil {
				if ctx.Err() != nil {
					return probe.Observation{}, err
				}
				pastErr = err
				break
			}
			rows, held = pt.rows, back
			if pm := sum(pt.rows, cfg); pm.matched > 0 {
				m.matched, m.last, quiet = pm.matched, pm.last, true
				break
			}
		}
	}
	if len(rows) == 0 {
		return probe.Observation{}, fmt.Errorf("SigNoz holds no value of %s in the last %s: the name may be wrong, or nothing reports it", cfg.metric, minutes(held))
	}
	now := time.Now()
	o := probe.Observation{Target: target, Probe: kindEdge, At: now, Detail: map[string]any{
		"metric": cfg.metric, "series": m.matched, "window_s": int(cfg.window.Seconds()), "read_at": tab.at.UTC().Format(time.RFC3339),
		"unit": cfg.unit,
	}}
	if pastErr != nil {
		o.Detail["known_note"] = "the last " + minutes(cfg.known) + " could not be read, so a series that is quiet now is not told from one that does not exist: " + pastErr.Error()
	}
	if m.matched == 0 {
		// Nobody counted what matches nothing: that is no rate of zero, it
		// is no rate.
		o.Detail["match_note"] = "no series of " + cfg.metric + " matches in the last " + minutes(held) + ", so no rate is reported: nothing was counted, or match names a value that does not exist; label_values lists the values SigNoz holds"
		o.Detail["label_values"] = valuesOf(rows, cfg.match)
		return o, nil
	}
	if !m.last.IsZero() {
		o.Detail["last_seen"] = m.last.UTC().Format(time.RFC3339)
	}
	if quiet {
		o.Detail["quiet_note"] = "nothing was counted in the last " + minutes(cfg.window) + "; what matches was counted within the last " + minutes(held) + ", so the rate is 0"
		m.total, m.failed, m.count = 0, 0, 0
	}
	o.Detail["count"] = probe.Round(m.count, 2)
	if tab.full {
		o.Detail["rows_note"] = fmt.Sprintf("SigNoz answered with %d series, which is all it was asked for: the rate may leave some out; match on fewer labels", len(tab.rows))
	}
	if !strings.Contains(target, "->") {
		// on a database: its transactions a second, and nothing else
		facet.EmitDatabase(&o, facet.DatabaseFacet{Rate: facet.N(probe.Round(m.total, rateDecimals)), Ready: true}, now)
		return o, nil
	}
	t := facet.TrafficFacet{Rate: facet.N(probe.Round(m.total, rateDecimals))}
	if len(cfg.errors) > 0 {
		t.ErrorRate = facet.N(0)
		if m.total > 0 {
			t.ErrorRate = facet.N(probe.Round(100*m.failed/m.total, 1))
		}
	}
	facet.EmitTraffic(&o, t, now)
	return o, nil
}

// lookBack lists how far back a binding whose window holds nothing looks:
// the last day, then the whole of known when that is more.
func lookBack(cfg edgeConfig) []time.Duration {
	var out []time.Duration
	if cfg.known > cfg.window {
		out = append(out, min(dayKnown, cfg.known))
	}
	if cfg.known > dayKnown && cfg.known > cfg.window {
		out = append(out, cfg.known)
	}
	return out
}

// unitOf reads what one count of the metric is. A binding says it with
// unit, a plural noun ("queries"); without one, the calls of a service to a
// database are queries and everything else a request.
func unitOf(spec map[string]any, metric string) (string, error) {
	u := probe.Str(spec, "unit", "")
	if u == "" {
		if metric == "signoz_db_latency_count" {
			return "queries/s", nil
		}
		return "req/s", nil
	}
	u = strings.TrimSuffix(u, "/s")
	if !unitWord.MatchString(u) {
		return "", fmt.Errorf("unit is what one count is, a word such as queries or spans")
	}
	return u + "/s", nil
}

var unitWord = regexp.MustCompile(`^[a-z]{2,16}$`)

// rateDecimals keep a call an hour apart from none: 0.000278 per second.
const rateDecimals = 6

// seenValues is how many values of a label a binding that matched nothing
// is shown.
const seenValues = 20

// valuesOf lists, for each label a binding matches on, the values the
// table holds, the busiest first. It is what somebody who wrote a match
// that finds nothing needs to see to write one that does.
func valuesOf(rows []row, match map[string]*regexp.Regexp) map[string][]string {
	out := map[string][]string{}
	for label := range match {
		rate := map[string]float64{}
		for _, r := range rows {
			rate[r.labels[label]] += r.value
		}
		values := make([]string, 0, len(rate))
		for v := range rate {
			values = append(values, v)
		}
		sort.Slice(values, func(i, j int) bool {
			if rate[values[i]] != rate[values[j]] {
				return rate[values[i]] > rate[values[j]]
			}
			return values[i] < values[j]
		})
		if len(values) > seenValues {
			values = values[:seenValues]
		}
		out[label] = values
	}
	return out
}

// minutes says a window in words.
func minutes(d time.Duration) string {
	if d >= 2*time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return d.String()
}
