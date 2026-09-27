package redisprobe

import (
	"testing"
	"time"
)

const celeryEnvelope = `{
  "body": "W1sxLCAyXSwge30sIHsiY2FsbGJhY2tzIjogbnVsbH1d",
  "content-encoding": "utf-8",
  "content-type": "application/json",
  "headers": {
    "lang": "py",
    "task": "reports.tasks.export_csv",
    "id": "5a7d1a3e-4f1c-4c8f-9c1e-4d2e8a1b6f00",
    "eta": "2026-09-27T09:50:00+00:00",
    "expires": null,
    "retries": 0,
    "origin": "gen12@worker-1"
  },
  "properties": {
    "correlation_id": "5a7d1a3e-4f1c-4c8f-9c1e-4d2e8a1b6f00",
    "delivery_mode": 2,
    "delivery_info": {"exchange": "", "routing_key": "exports"},
    "priority": 0,
    "body_encoding": "base64",
    "published_at": 1790502600
  }
}`

func TestParseEnvelope(t *testing.T) {
	e, err := ParseEnvelope([]byte(celeryEnvelope))
	if err != nil {
		t.Fatal(err)
	}
	if e.Task != "reports.tasks.export_csv" || e.ID != "5a7d1a3e-4f1c-4c8f-9c1e-4d2e8a1b6f00" || e.RoutingKey != "exports" || e.Origin != "gen12@worker-1" {
		t.Errorf("envelope = %+v", e)
	}
	if e.ETA.IsZero() || !e.ETA.Equal(time.Date(2026, 9, 27, 9, 50, 0, 0, time.UTC)) {
		t.Errorf("eta = %v", e.ETA)
	}
	if !e.PublishedAt.Equal(time.Unix(1790502600, 0)) {
		t.Errorf("published_at = %v", e.PublishedAt)
	}
	now := time.Unix(1790502600, 0).Add(90 * time.Second)
	if age, ok := e.Age(now); !ok || age != 90*time.Second {
		t.Errorf("age = %v %v (published_at wins over eta)", age, ok)
	}
}

func TestEnvelopeAgeFromETA(t *testing.T) {
	e, err := ParseEnvelope([]byte(`{"headers":{"task":"t","id":"1","eta":"2026-09-27T10:00:00Z"},"properties":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if age, ok := e.Age(time.Date(2026, 9, 27, 10, 5, 0, 0, time.UTC)); !ok || age != 5*time.Minute {
		t.Errorf("age = %v %v", age, ok)
	}
	// A task scheduled in the future has waited nothing yet.
	if age, ok := e.Age(time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)); !ok || age != 0 {
		t.Errorf("future eta age = %v %v", age, ok)
	}
	plain, err := ParseEnvelope([]byte(`{"headers":{"task":"t","id":"2","eta":null},"properties":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.Age(time.Now()); ok {
		t.Error("age should be unknown without eta or published_at")
	}
}

func TestParseEnvelopeErrors(t *testing.T) {
	if _, err := ParseEnvelope([]byte(`not json`)); err == nil {
		t.Error("garbage should fail")
	}
	if _, err := ParseEnvelope([]byte(`{"body":"x"}`)); err == nil {
		t.Error("envelope without task headers should fail")
	}
}

func TestDepthRingGrowth(t *testing.T) {
	r := &depthRing{}
	t0 := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	if _, ok := r.growth(); ok {
		t.Error("empty ring has no growth")
	}
	r.push(t0, 100)
	if _, ok := r.growth(); ok {
		t.Error("single reading has no growth")
	}
	r.push(t0.Add(30*time.Second), 130)
	if g, ok := r.growth(); !ok || g != 60 {
		t.Errorf("growth = %v %v, want 60/min", g, ok)
	}
	// Readings older than a minute fall off.
	r.push(t0.Add(90*time.Second), 190)
	if len(r.at) != 2 {
		t.Errorf("ring len = %d, want 2", len(r.at))
	}
	if g, ok := r.growth(); !ok || g != 60 {
		t.Errorf("growth = %v %v, want 60/min", g, ok)
	}
}

func TestSummarizeFlower(t *testing.T) {
	workers := map[string]FlowerWorker{}
	w1 := FlowerWorker{}
	w1.ActiveQueues = append(w1.ActiveQueues, struct {
		Name string `json:"name"`
	}{"default"}, struct {
		Name string `json:"name"`
	}{"exports"})
	t1 := FlowerTask{ID: "a", Name: "reports.tasks.export_csv", TimeStart: 1790502600}
	t1.DeliveryInfo.RoutingKey = "exports"
	t2 := FlowerTask{ID: "b", Name: "mail.send", TimeStart: 1790502600}
	t2.DeliveryInfo.RoutingKey = "default"
	w1.Active = []FlowerTask{t1, t2}
	w2 := FlowerWorker{}
	w2.ActiveQueues = append(w2.ActiveQueues, struct {
		Name string `json:"name"`
	}{"default"})
	workers["w1"] = w1
	workers["w2"] = w2

	s := SummarizeFlower(workers, "exports")
	if s.Consumers != 1 {
		t.Errorf("consumers = %d", s.Consumers)
	}
	if len(s.Running) != 1 || s.Running[0].ID != "a" {
		t.Errorf("running = %+v", s.Running)
	}
	rt := func(f float64) *float64 { return &f }
	p95, ok := P95Runtime([]FlowerFinished{
		{RoutingKey: "exports", Runtime: rt(1)}, {RoutingKey: "exports", Runtime: rt(2)}, {RoutingKey: "exports", Runtime: rt(3)},
		{RoutingKey: "exports", Runtime: rt(4)}, {RoutingKey: "exports", Runtime: rt(50)}, {RoutingKey: "default", Runtime: rt(999)},
		{RoutingKey: "exports"},
	}, "exports")
	if !ok || p95 != 4 {
		t.Errorf("p95 = %v %v", p95, ok)
	}
	if _, ok := P95Runtime(nil, "exports"); ok {
		t.Error("no tasks -> no p95")
	}
}

func TestCeleryValidate(t *testing.T) {
	p := &CeleryProbe{}
	if err := p.Validate(map[string]any{"broker": "redis://broker:6379/0"}); err == nil {
		t.Error("queue is required")
	}
	if err := p.Validate(map[string]any{"broker": "redis://broker:6379/0", "queue": "default", "flower_url": "http://flower:5555", "long_task": "15m"}); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := p.Validate(map[string]any{"broker": "redis://broker:6379/0", "queue": "default", "long_task": "soon"}); err == nil {
		t.Error("bad long_task should fail")
	}
	if err := p.Validate(map[string]any{"broker": "redis://broker:6379/0", "queue": "default", "flower_url": "flower"}); err == nil {
		t.Error("bad flower_url should fail")
	}
}
