package signoz

import (
	"context"
	"sort"
	"time"
)

// Call is what one service was counted doing over a lookback: calls to one
// address, or queries, how many, and when last.
type Call struct {
	Service string    `json:"service"`
	Address string    `json:"address,omitempty"`
	Count   float64   `json:"count"`
	Last    time.Time `json:"last"`
}

// Survey is what SigNoz knows of the services that report to it, over a
// lookback: what each calls, what each asks its databases, which send
// spans and which send only metrics. It is what wassup measure places on
// the diagram and binds, from the same counters signoz.edge reads.
type Survey struct {
	Lookback time.Duration `json:"lookback"`
	// External lists the calls of a service to an address outside it.
	External []Call `json:"external"`
	// Queries lists, per service, the calls it made to a database.
	Queries []Call `json:"queries"`
	// Spans lists, per service, the spans it sent.
	Spans []Call `json:"spans"`
	// Served lists, per service, the HTTP requests it answered, as the
	// OpenTelemetry HTTP server instrumentation counts them.
	Served []Call `json:"served,omitempty"`
	// Quiet names the services that sent metrics of their outgoing calls
	// but no span: SigNoz cannot say where their calls went.
	Quiet []string `json:"quiet,omitempty"`
}

// The counters a survey reads. They are the ones SigNoz's span metrics
// write for every service that sends traces, and the one the OpenTelemetry
// HTTP client instrumentation writes whether spans are sent or not.
const (
	metricExternal   = "signoz_external_call_latency_count"
	metricQueries    = "signoz_db_latency_count"
	metricSpans      = "signoz_calls_total"
	metricClientHTTP = "http.client.duration.count"
	metricServerHTTP = "http.server.duration.count"
)

// Surveyed reads a survey from the SigNoz a spec names (url and the
// credentials of a signoz.edge binding), five queries over the lookback.
func Surveyed(ctx context.Context, spec map[string]any, lookback time.Duration) (Survey, error) {
	cfg, err := edgeConfigOf(withMetric(spec))
	if err != nil {
		return Survey{}, err
	}
	sess := sessionFor(cfg.base, cfg.creds)
	now := time.Now()
	s := Survey{Lookback: lookback}
	read := func(metric string, by ...string) ([]row, error) {
		tab, err := ask(ctx, sess, metric, by, lookback, now)
		return tab.rows, err
	}
	rows, err := read(metricExternal, "service.name", "address")
	if err != nil {
		return s, err
	}
	s.External = callsOf(rows, "address")
	if rows, err = read(metricQueries, "service.name"); err != nil {
		return s, err
	}
	s.Queries = callsOf(rows, "")
	if rows, err = read(metricSpans, "service.name"); err != nil {
		return s, err
	}
	s.Spans = callsOf(rows, "")
	// A counter nobody writes is no error here: a system without HTTP
	// metrics has no requests served to say, and no quiet services to name.
	if rows, err = read(metricServerHTTP, "service.name"); err == nil {
		s.Served = callsOf(rows, "")
	}
	if rows, err = read(metricClientHTTP, "service.name"); err == nil {
		traced := map[string]bool{}
		for _, c := range s.Spans {
			traced[c.Service] = true
		}
		for _, c := range callsOf(rows, "") {
			if !traced[c.Service] {
				s.Quiet = append(s.Quiet, c.Service)
			}
		}
	}
	return s, nil
}

// withMetric completes a spec that names no metric: a survey reads several.
func withMetric(spec map[string]any) map[string]any {
	out := map[string]any{"metric": metricExternal}
	for k, v := range spec {
		out[k] = v
	}
	return out
}

// callsOf turns the rows of a table into calls, the busiest first, leaving
// out what counted nothing.
func callsOf(rows []row, address string) []Call {
	var out []Call
	for _, r := range rows {
		if r.count <= 0 {
			continue
		}
		c := Call{Service: r.labels["service.name"], Count: r.count, Last: r.last}
		if address != "" {
			c.Address = r.labels[address]
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Service+out[i].Address < out[j].Service+out[j].Address
	})
	return out
}
