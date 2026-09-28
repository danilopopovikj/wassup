package stubs

import (
	"context"
	"testing"

	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestStubsRegistered(t *testing.T) {
	required := map[string][]string{
		"signoz.edge":   {"from", "to", "url"},
		"signoz.health": {"url"},
		"s3.bucket":     {"bucket", "endpoint", "region"},
		"kubelet.stats": {"node"},
	}
	for kind, fields := range required {
		a, ok := probe.AccessFor(kind)
		if !ok {
			t.Errorf("%s not registered", kind)
			continue
		}
		if a.Implemented {
			t.Errorf("%s must report Implemented=false", kind)
		}
		if a.Delivers == "" || a.Needs == "" || len(a.SpecFields) == 0 {
			t.Errorf("%s access docs incomplete: %+v", kind, a)
		}
		p, err := probe.New(kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Validate(map[string]any{}); err == nil {
			t.Errorf("%s: empty spec should fail validation", kind)
		}
		full := map[string]any{}
		for _, f := range fields {
			full[f] = "x"
		}
		if err := p.Validate(full); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
		if err := p.Start(context.Background(), full, nil); err != nil {
			t.Errorf("%s: Start: %v", kind, err)
		}
		if h := p.Health(); h.State != probe.HealthFailed {
			t.Errorf("%s: health = %+v, want failed", kind, h)
		}
	}
	if probe.Known("dns.record") {
		t.Log("dns.record is registered elsewhere; not owned by stubs")
	}
}
