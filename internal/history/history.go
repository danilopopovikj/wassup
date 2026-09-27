// Package history writes the snapshot, keeps compact frames for the timeline
// and scrubbing, records change markers, and computes trends and baselines.
package history

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
)

// FrameElem is one element in a compact frame.
type FrameElem struct {
	S string             `json:"s"`           // state
	V model.Severity     `json:"v"`           // severity
	K string             `json:"k,omitempty"` // marker
	L string             `json:"l,omitempty"` // label
	M map[string]float64 `json:"m,omitempty"` // up to 8 key metrics
}

// Frame is one tick, compact.
type Frame struct {
	At   time.Time            `json:"t"`
	Tick int64                `json:"n"`
	C    map[string]FrameElem `json:"c"`
	E    map[string]FrameElem `json:"e"`
}

// keyMetrics are kept in frames, in priority order, at most 8 per element.
var keyMetrics = []string{
	"rate", "error_rate", "queued", "waiters", "depth", "cpu_pct", "mem_pct", "disk_pct",
	"connections_used", "connections_max", "lag_bytes", "restarts", "replicas_ready", "replicas_desired",
	"cert_days", "hit_rate", "targets_healthy", "targets_total", "oldest_age_s", "running_s", "timeout_rate", "latency_ms",
	"wal_retained_bytes", "ingest_rate", "killed", "active", "failed", "succeeded", "pool_used", "pool_max",
}

func compact(es model.ElementState) FrameElem {
	fe := FrameElem{S: string(es.State), V: es.Severity, K: string(es.Marker), L: es.Label}
	if len(es.Metrics) > 0 {
		fe.M = map[string]float64{}
		for _, k := range keyMetrics {
			if v, ok := es.Metrics[k]; ok {
				fe.M[k] = v
				if len(fe.M) >= 8 {
					break
				}
			}
		}
	}
	return fe
}

// FromSnapshot compacts a snapshot into a frame.
func FromSnapshot(s *model.Snapshot) Frame {
	f := Frame{At: s.GeneratedAt, Tick: s.Tick, C: map[string]FrameElem{}, E: map[string]FrameElem{}}
	for id, es := range s.Components {
		f.C[id] = compact(es)
	}
	for id, es := range s.Edges {
		f.E[id] = compact(es)
	}
	return f
}

// ToSnapshot expands a frame back into a snapshot for replay rendering.
func ToSnapshot(f Frame, name string) *model.Snapshot {
	s := &model.Snapshot{GeneratedAt: f.At, Tick: f.Tick, Name: name, Replay: true,
		Components: map[string]model.ElementState{}, Edges: map[string]model.ElementState{}}
	expand := func(fe FrameElem) model.ElementState {
		es := model.ElementState{State: model.State(fe.S), Severity: fe.V, Marker: model.Marker(fe.K), Label: fe.L, Metrics: fe.M}
		if fe.M != nil {
			es.Rate = fe.M["rate"]
			es.Queued = fe.M["queued"]
			es.ErrorRate = fe.M["error_rate"]
		}
		return es
	}
	for id, fe := range f.C {
		s.Components[id] = expand(fe)
	}
	for id, fe := range f.E {
		s.Edges[id] = expand(fe)
	}
	return s
}

// Ring keeps recent frames in memory, oldest first.
type Ring struct {
	mu     sync.RWMutex
	frames []Frame
	keep   time.Duration
}

// NewRing keeps frames for the given duration (24h by default).
func NewRing(keep time.Duration) *Ring {
	if keep <= 0 {
		keep = 24 * time.Hour
	}
	return &Ring{keep: keep}
}

// Add appends a frame and drops frames older than the retention.
func (r *Ring) Add(f Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, f)
	cut := f.At.Add(-r.keep)
	i := 0
	for i < len(r.frames) && r.frames[i].At.Before(cut) {
		i++
	}
	if i > 0 {
		r.frames = append([]Frame(nil), r.frames[i:]...)
	}
}

// AddAll appends frames, sorting by time.
func (r *Ring) AddAll(fs []Frame) {
	r.mu.Lock()
	r.frames = append(r.frames, fs...)
	sort.SliceStable(r.frames, func(i, j int) bool { return r.frames[i].At.Before(r.frames[j].At) })
	r.mu.Unlock()
}

// Frames returns a copy of the frames.
func (r *Ring) Frames() []Frame {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Frame(nil), r.frames...)
}

// Len returns the number of frames.
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.frames)
}

// At returns the latest frame at or before t.
func (r *Ring) At(t time.Time) (Frame, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	idx := sort.Search(len(r.frames), func(i int) bool { return r.frames[i].At.After(t) })
	if idx == 0 {
		if len(r.frames) > 0 && !r.frames[0].At.After(t.Add(time.Minute)) {
			return r.frames[0], true
		}
		return Frame{}, false
	}
	return r.frames[idx-1], true
}

// Bounds returns the oldest and newest frame time.
func (r *Ring) Bounds() (time.Time, time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.frames) == 0 {
		return time.Time{}, time.Time{}, false
	}
	return r.frames[0].At, r.frames[len(r.frames)-1].At, true
}

// trendMetrics are the gauges that get trend arrows.
var trendMetrics = []string{"disk_pct", "mem_pct", "cpu_pct", "depth", "lag_bytes", "wal_retained_bytes", "connections_used"}

// Trends returns, per element and metric, the slope over the window in
// units per hour (percent per hour for percent gauges), from a least squares
// fit. A slope is only reported when the frames cover at least half the window.
func (r *Ring) Trends(now time.Time, window time.Duration) map[string]map[string]float64 {
	frames := r.Frames()
	cut := now.Add(-window)
	type acc struct {
		n, sx, sy, sxx, sxy float64
		first, last         time.Time
	}
	accs := map[string]map[string]*acc{}
	for _, f := range frames {
		if f.At.Before(cut) || f.At.After(now) {
			continue
		}
		x := f.At.Sub(cut).Hours()
		visit := func(id string, fe FrameElem) {
			for _, k := range trendMetrics {
				v, ok := fe.M[k]
				if !ok {
					continue
				}
				if accs[id] == nil {
					accs[id] = map[string]*acc{}
				}
				a := accs[id][k]
				if a == nil {
					a = &acc{first: f.At}
					accs[id][k] = a
				}
				a.n++
				a.sx += x
				a.sy += v
				a.sxx += x * x
				a.sxy += x * v
				a.last = f.At
			}
		}
		for id, fe := range f.C {
			visit(id, fe)
		}
		for id, fe := range f.E {
			visit(id, fe)
		}
	}
	out := map[string]map[string]float64{}
	for id, ms := range accs {
		for k, a := range ms {
			if a.n < 3 || a.last.Sub(a.first) < window/2 {
				continue
			}
			den := a.n*a.sxx - a.sx*a.sx
			if den == 0 {
				continue
			}
			slope := (a.n*a.sxy - a.sx*a.sy) / den
			if out[id] == nil {
				out[id] = map[string]float64{}
			}
			out[id][k] = slope
		}
	}
	return out
}

// Baselines returns the average edge rate around the same time yesterday.
func (r *Ring) Baselines(now time.Time) map[string]float64 {
	frames := r.Frames()
	center := now.Add(-24 * time.Hour)
	lo, hi := center.Add(-15*time.Minute), center.Add(15*time.Minute)
	sum := map[string]float64{}
	n := map[string]float64{}
	for _, f := range frames {
		if f.At.Before(lo) || f.At.After(hi) {
			continue
		}
		for id, fe := range f.E {
			if v, ok := fe.M["rate"]; ok {
				sum[id] += v
				n[id]++
			}
		}
	}
	out := map[string]float64{}
	for id := range sum {
		out[id] = sum[id] / n[id]
	}
	return out
}

// Bucket is one column of the timeline strip.
type Bucket struct {
	From, To time.Time
	Worst    model.Severity
	Has      bool
}

// Buckets splits the last span before now into n columns with the worst
// severity per column.
func (r *Ring) Buckets(now time.Time, span time.Duration, n int) []Bucket {
	if n <= 0 {
		return nil
	}
	frames := r.Frames()
	out := make([]Bucket, n)
	width := span / time.Duration(n)
	start := now.Add(-span)
	for i := range out {
		out[i].From = start.Add(time.Duration(i) * width)
		out[i].To = out[i].From.Add(width)
	}
	for _, f := range frames {
		if f.At.Before(start) || f.At.After(now) {
			continue
		}
		i := int(f.At.Sub(start) / width)
		if i < 0 || i >= n {
			continue
		}
		out[i].Has = true
		for _, fe := range f.C {
			out[i].Worst = out[i].Worst.Max(fe.V)
		}
		for _, fe := range f.E {
			out[i].Worst = out[i].Worst.Max(fe.V)
		}
	}
	return out
}

// Store persists snapshot, frames and events under <dir>/state.
type Store struct {
	Dir string // the .wassup directory
	mu  sync.Mutex
}

// NewStore returns a store for the given .wassup directory.
func NewStore(dir string) *Store { return &Store{Dir: dir} }

func (s *Store) stateDir() string { return filepath.Join(s.Dir, "state") }

// WriteSnapshot writes state/snapshot.json atomically.
func (s *Store) WriteSnapshot(snap *model.Snapshot) error {
	return model.WriteJSON(filepath.Join(s.stateDir(), "snapshot.json"), snap)
}

// AppendFrame appends a compact frame to state/history/<date>.jsonl.
func (s *Store) AppendFrame(f Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.stateDir(), "history")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	fh, err := os.OpenFile(filepath.Join(dir, f.At.UTC().Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.Write(append(b, '\n'))
	return err
}

// LoadFrames reads every frame since the given time, oldest first.
func (s *Store) LoadFrames(since time.Time) ([]Frame, error) {
	dir := filepath.Join(s.stateDir(), "history")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Frame
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(e.Name(), ".jsonl"))
		if err != nil || day.Add(24*time.Hour).Before(since) {
			continue
		}
		fs, err := readFrames(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			if !f.At.Before(since) {
				out = append(out, f)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func readFrames(path string) ([]Frame, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []Frame
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var f Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			continue // a torn last line is not fatal
		}
		out = append(out, f)
	}
	return out, sc.Err()
}

// Rollup thins frames older than 24 hours to one per 5 minutes and removes
// files older than 7 days. It returns how many frames were dropped.
func (s *Store) Rollup(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.stateDir(), "history")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	dropped := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(e.Name(), ".jsonl"))
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if day.Add(24 * time.Hour).Before(now.Add(-7 * 24 * time.Hour)) {
			if err := os.Remove(p); err == nil {
				continue
			}
		}
		if !day.Add(24 * time.Hour).Before(now.Add(-24 * time.Hour)) {
			continue // today or yesterday: still full resolution
		}
		fs, err := readFrames(p)
		if err != nil {
			return dropped, err
		}
		var kept []Frame
		var last time.Time
		for _, f := range fs {
			if last.IsZero() || f.At.Sub(last) >= 5*time.Minute {
				kept = append(kept, f)
				last = f.At
			} else {
				dropped++
			}
		}
		if len(kept) == len(fs) {
			continue
		}
		var b strings.Builder
		for _, f := range kept {
			jb, _ := json.Marshal(f)
			b.Write(jb)
			b.WriteByte('\n')
		}
		if err := model.WriteAtomic(p, []byte(b.String())); err != nil {
			return dropped, err
		}
	}
	return dropped, nil
}

// AppendEvents appends change markers to state/events.jsonl.
func (s *Store) AppendEvents(evs []model.Event) error {
	if len(evs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.stateDir(), 0o755); err != nil {
		return err
	}
	fh, err := os.OpenFile(filepath.Join(s.stateDir(), "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := bufio.NewWriter(fh)
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	return w.Flush()
}

// LoadEvents reads state/events.jsonl, oldest first.
func (s *Store) LoadEvents() ([]model.Event, error) {
	fh, err := os.Open(filepath.Join(s.stateDir(), "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()
	var out []model.Event
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e model.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, sc.Err()
}

// Recent filters events to the lookback window before now.
func Recent(evs []model.Event, now time.Time, lookback time.Duration) []model.Event {
	var out []model.Event
	for _, e := range evs {
		if !e.At.After(now) && e.At.After(now.Add(-lookback)) {
			out = append(out, e)
		}
	}
	return out
}

// ExportDir returns state/export, creating it.
func (s *Store) ExportDir() (string, error) {
	d := filepath.Join(s.stateDir(), "export")
	return d, os.MkdirAll(d, 0o755)
}

// Describe returns a one-line summary of the history on disk.
func (s *Store) Describe() string {
	fs, err := s.LoadFrames(time.Time{})
	if err != nil || len(fs) == 0 {
		return "no history"
	}
	return fmt.Sprintf("%d frames from %s to %s", len(fs), fs[0].At.Format(time.RFC3339), fs[len(fs)-1].At.Format(time.RFC3339))
}
