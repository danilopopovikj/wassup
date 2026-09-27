package history

import (
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
)

func frame(at time.Time, disk float64, sev model.Severity) Frame {
	return Frame{At: at, C: map[string]FrameElem{"db": {S: "flowing", V: sev, M: map[string]float64{"disk_pct": disk}}}, E: map[string]FrameElem{}}
}

func TestTrendsAndBuckets(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	r := NewRing(24 * time.Hour)
	for i := 0; i <= 60; i++ {
		at := now.Add(-time.Duration(60-i) * time.Minute)
		r.Add(frame(at, 80+float64(i)/30, model.Info)) // +2 percent per hour
	}
	tr := r.Trends(now, 30*time.Minute)
	slope := tr["db"]["disk_pct"]
	if slope < 1.9 || slope > 2.1 {
		t.Errorf("slope = %v, want about 2 per hour", slope)
	}
	b := r.Buckets(now, 6*time.Hour, 36)
	if len(b) != 36 || b[35].Worst != model.Info || !b[35].Has || b[0].Has {
		t.Errorf("buckets: %+v", b[35])
	}
	f, ok := r.At(now.Add(-30 * time.Minute))
	if !ok || !f.At.Equal(now.Add(-30*time.Minute)) {
		t.Errorf("At: %v %v", f.At, ok)
	}
}

func TestStoreRoundTripAndRollup(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	old := now.Add(-3 * 24 * time.Hour)
	for i := 0; i < 120; i++ { // 10 minutes of 5 s ticks, three days ago
		if err := s.AppendFrame(frame(old.Add(time.Duration(i)*5*time.Second), 50, model.Warn)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendFrame(frame(now, 51, model.Info)); err != nil {
		t.Fatal(err)
	}
	dropped, err := s.Rollup(now)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 118 {
		t.Errorf("rollup dropped %d, want 118 (one frame per 5 min kept)", dropped)
	}
	fs, err := s.LoadFrames(time.Time{})
	if err != nil || len(fs) != 3 {
		t.Errorf("frames after rollup: %d %v", len(fs), err)
	}
	if err := s.AppendEvents([]model.Event{{At: now, Kind: "deploy", Target: "api", Summary: "deploy of api"}}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.LoadEvents()
	if err != nil || len(evs) != 1 || evs[0].Letter() != "D" {
		t.Errorf("events: %v %v", evs, err)
	}
	snap := &model.Snapshot{GeneratedAt: now, Components: map[string]model.ElementState{"db": {State: model.Flowing, Metrics: map[string]float64{"rate": 1, "x": 2}}}}
	if err := s.WriteSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	back := ToSnapshot(FromSnapshot(snap), "t")
	if back.Components["db"].State != model.Flowing || back.Components["db"].Rate != 1 {
		t.Errorf("frame round trip: %+v", back.Components["db"])
	}
}
