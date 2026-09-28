package hatchet

import (
	"context"
	"net/http"
	"testing"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestHealthReady(t *testing.T) {
	srv := serve(t, routes{
		"/api/ready":   jsonOK("ok"),
		"/api/live":    jsonOK("ok"),
		"/api/v1/meta": rawJSON(`{"version": "v0.55.1", "auth": {"schemes": ["basic"]}}`),
	})
	spec := specFor(t, srv, nil)
	// No token at all: the health probe must still work.
	t.Setenv("WASSUP_TEST_HATCHET_TOKEN", "")
	p := &HealthProbe{}
	st, err := p.setup(spec)
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if o.Metrics["ready"] != 1 || o.Metrics["live"] != 1 || o.Metrics["latency_ms"] < 0 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if len(o.Conditions) != 0 {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
	if o.Detail["version"] != "v0.55.1" || o.Detail["url"] != srv.URL {
		t.Fatalf("detail = %v", o.Detail)
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}
}

func TestHealthNotReady(t *testing.T) {
	srv := serve(t, routes{
		"/api/ready": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		},
		"/api/live": jsonOK("ok"),
	})
	p := &HealthProbe{}
	st, err := p.setup(specFor(t, srv, nil))
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if o.Metrics["ready"] != 0 || o.Metrics["live"] != 1 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	nr, ok := model.HasCondition(o.Conditions, model.CondNotReady)
	if !ok || nr.Detail != "503 Service Unavailable: database unreachable" || nr.Since.IsZero() {
		t.Fatalf("NotReady = %+v (%v)", nr, ok)
	}
	if _, ok := o.Detail["version"]; ok {
		t.Fatal("version fabricated without /api/v1/meta")
	}
	// A reachable but unready engine is data, not a probe failure.
	if h := p.Health(); h.State != probe.HealthOK {
		t.Fatalf("health = %+v", h)
	}
	since := nr.Since
	o = p.poll(context.Background(), st)
	nr, _ = model.HasCondition(o.Conditions, model.CondNotReady)
	if !nr.Since.Equal(since) {
		t.Fatalf("Since moved from %s to %s", since, nr.Since)
	}
}

func TestHealthUnreachable(t *testing.T) {
	srv := serve(t, routes{})
	spec := specFor(t, srv, nil)
	srv.Close()
	p := &HealthProbe{}
	st, err := p.setup(spec)
	if err != nil {
		t.Fatal(err)
	}
	o := p.poll(context.Background(), st)
	if o.Err == "" || o.Metrics != nil {
		t.Fatalf("observation = %+v", o)
	}
	if h := p.Health(); h.State != probe.HealthDegraded {
		t.Fatalf("health = %+v", h)
	}
}
