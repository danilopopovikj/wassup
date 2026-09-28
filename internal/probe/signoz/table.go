package signoz

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	// rowLimit is how many series one answer may hold.
	rowLimit = 1000
	// labelKeep is how long a label stays in the query after the last
	// binding asked for it.
	labelKeep = 10 * time.Minute
	// settle is how long the first query of a metric waits for the bindings
	// that start in the same moment to say which labels they match on.
	settle = 150 * time.Millisecond
)

// row is one series of a metric: its labels and its rate per second.
type row struct {
	labels map[string]string
	value  float64
}

// table is one answer of SigNoz: the rate of every series of a metric over
// a window, grouped by the labels the bindings match on.
type table struct {
	at   time.Time
	by   map[string]bool
	rows []row
	full bool // the answer held as many series as it may
	err  error
}

// slot holds the table of one metric and what the bindings want of it.
type slot struct {
	// want is guarded by the store: the labels asked for, and when last.
	want map[string]time.Time
	// mu guards tab and is held while SigNoz is asked, so that the bindings
	// that come at the same moment wait for one answer.
	mu  sync.Mutex
	tab table
}

// tableStore shares the answers of SigNoz between the probes of one
// process: ten bindings on the calls of one service are one query per
// interval, not ten. The query groups by every label one of them matches
// on, and each binding adds up the rows it means.
type tableStore struct {
	mu     sync.Mutex
	slots  map[string]*slot
	now    func() time.Time
	settle time.Duration
	// ask runs the query; replaced in tests.
	ask func(ctx context.Context, sess *session, metric string, by []string, window time.Duration, now time.Time) (table, error)
}

var tables = &tableStore{slots: map[string]*slot{}, now: time.Now, settle: settle, ask: ask}

// want returns the slot of a binding's metric and notes the labels the
// binding matches on. A probe calls it when it starts, before anything is
// asked, and the store on every read.
func (s *tableStore) want(sess *session, cfg edgeConfig) *slot {
	key := fmt.Sprintf("%p %s %s", sess, cfg.metric, cfg.window)
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := s.slots[key]
	if sl == nil {
		sl = &slot{want: map[string]time.Time{}}
		s.slots[key] = sl
	}
	now := s.now()
	for _, l := range cfg.labels {
		sl.want[l] = now
	}
	return sl
}

// wanted lists the labels the bindings of a slot match on, and forgets the
// ones nobody asked for in a while.
func (s *tableStore) wanted(sl *slot) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var by []string
	for l, at := range sl.want {
		if now.Sub(at) > labelKeep {
			delete(sl.want, l)
			continue
		}
		by = append(by, l)
	}
	sort.Strings(by)
	return by
}

// read returns the table of the binding's metric, asked for no longer than
// the binding's interval ago and grouped by at least its labels. An answer
// that was an error is kept as long as a good one, so a SigNoz that is down
// is not asked by every binding on every tick.
func (s *tableStore) read(ctx context.Context, sess *session, cfg edgeConfig) (table, error) {
	sl := s.want(sess, cfg)
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if !sl.tab.at.IsZero() && s.now().Sub(sl.tab.at) < cfg.interval && (sl.tab.err != nil || covers(sl.tab.by, cfg.labels)) {
		return sl.tab, sl.tab.err
	}
	if sl.tab.at.IsZero() && s.settle > 0 {
		select {
		case <-ctx.Done():
			return table{}, ctx.Err()
		case <-time.After(s.settle):
		}
	}
	by := s.wanted(sl)
	now := s.now()
	tab, err := s.ask(ctx, sess, cfg.metric, by, cfg.window, now)
	if err != nil && ctx.Err() != nil {
		// The binding that asked was stopped, or ran out of time. That says
		// nothing about SigNoz, so the next one asks again.
		return table{}, err
	}
	tab.at, tab.err = now, err
	tab.by = map[string]bool{}
	for _, l := range by {
		tab.by[l] = true
	}
	sl.tab = tab
	return tab, err
}

// covers reports whether a table is grouped by every label of a binding.
func covers(by map[string]bool, labels []string) bool {
	for _, l := range labels {
		if !by[l] {
			return false
		}
	}
	return true
}

// The query of SigNoz's builder, as far as the probe uses it.
type (
	queryRequest struct {
		SchemaVersion  string         `json:"schemaVersion"`
		Start          int64          `json:"start"` // milliseconds
		End            int64          `json:"end"`
		RequestType    string         `json:"requestType"`
		CompositeQuery compositeQuery `json:"compositeQuery"`
	}
	compositeQuery struct {
		Queries []builderQuery `json:"queries"`
	}
	builderQuery struct {
		Type string    `json:"type"`
		Spec querySpec `json:"spec"`
	}
	querySpec struct {
		Name         string        `json:"name"`
		Signal       string        `json:"signal"`
		Disabled     bool          `json:"disabled"`
		Aggregations []aggregation `json:"aggregations"`
		GroupBy      []groupKey    `json:"groupBy"`
		StepInterval int           `json:"stepInterval"`
		Limit        int           `json:"limit"`
	}
	aggregation struct {
		MetricName       string `json:"metricName"`
		TimeAggregation  string `json:"timeAggregation"`
		SpaceAggregation string `json:"spaceAggregation"`
		ReduceTo         string `json:"reduceTo"`
	}
	groupKey struct {
		Name string `json:"name"`
	}
	queryAnswer struct {
		Data struct {
			Results []struct {
				Columns []struct {
					Name string `json:"name"`
					Type string `json:"columnType"`
				} `json:"columns"`
				Data [][]any `json:"data"`
			} `json:"results"`
		} `json:"data"`
	}
)

// queryOf builds the query for the rate of a counter per second, averaged
// over the window and summed per group of labels.
func queryOf(metric string, by []string, window time.Duration, now time.Time) queryRequest {
	spec := querySpec{
		Name: "A", Signal: "metrics",
		Aggregations: []aggregation{{MetricName: metric, TimeAggregation: "rate", SpaceAggregation: "sum", ReduceTo: "avg"}},
		GroupBy:      []groupKey{},
		StepInterval: stepOf(window),
		Limit:        rowLimit,
	}
	for _, l := range by {
		spec.GroupBy = append(spec.GroupBy, groupKey{Name: l})
	}
	return queryRequest{
		SchemaVersion: "v1", Start: now.Add(-window).UnixMilli(), End: now.UnixMilli(), RequestType: "scalar",
		CompositeQuery: compositeQuery{Queries: []builderQuery{{Type: "builder_query", Spec: spec}}},
	}
}

// stepOf is the resolution a window is asked for in: a minute, which is how
// often SigNoz takes a value, and for more than an hour sixty steps, so that
// a day costs what an hour does.
func stepOf(window time.Duration) int {
	if window <= time.Hour {
		return step
	}
	return int(window.Seconds()) / 60
}

// ask runs the query of a metric and reads the answer.
func ask(ctx context.Context, sess *session, metric string, by []string, window time.Duration, now time.Time) (table, error) {
	var a queryAnswer
	if err := sess.query(ctx, queryOf(metric, by, window, now), &a); err != nil {
		return table{}, err
	}
	return tableOf(a, by)
}

// tableOf reads an answer: one row per group, the labels in the columns
// that carry their names and the rate in the column of the aggregation,
// which is the last one. A rate SigNoz did not know (null) is left out.
func tableOf(a queryAnswer, by []string) (table, error) {
	var t table
	if len(a.Data.Results) == 0 {
		return t, nil
	}
	res := a.Data.Results[0]
	if len(res.Columns) == 0 {
		return t, nil
	}
	value := len(res.Columns) - 1
	for i, c := range res.Columns {
		if c.Type == "aggregation" {
			value = i
		}
	}
	at := map[string]int{}
	for i, c := range res.Columns {
		if i != value {
			at[c.Name] = i
		}
	}
	for _, l := range by {
		if _, ok := at[l]; !ok {
			return t, fmt.Errorf("SigNoz %s: the answer has no column for the label %s", queryPath, l)
		}
	}
	for _, cells := range res.Data {
		if len(cells) != len(res.Columns) {
			return t, fmt.Errorf("SigNoz %s: a row of %d values under %d columns", queryPath, len(cells), len(res.Columns))
		}
		v, ok := number(cells[value])
		if !ok {
			continue
		}
		r := row{labels: make(map[string]string, len(by)), value: v}
		for _, l := range by {
			r.labels[l] = text(cells[at[l]])
		}
		t.rows = append(t.rows, r)
	}
	t.full = len(res.Data) >= rowLimit
	return t, nil
}

// number reads a value of the answer.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// text reads a label of the answer; a series without the label has none.
func text(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	}
	return fmt.Sprint(v)
}
