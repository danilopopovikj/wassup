package k8s

import (
	"testing"

	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestRegistered(t *testing.T) {
	for _, kind := range []string{kindWorkload, kindNode, kindCronJob, kindPVC, kindIngress, kindCNPGCluster, kindCNPGInstance, kindScrape} {
		a, ok := probe.AccessFor(kind)
		if !ok || !a.Implemented || len(a.SpecFields) == 0 || a.Needs == "" {
			t.Errorf("%s: access = %+v (ok %v)", kind, a, ok)
		}
		p, err := probe.New(kind)
		if err != nil || p.Kind() != kind {
			t.Errorf("%s: New = %v, %v", kind, p, err)
		}
		if h := p.Health(); h.State != probe.HealthDegraded {
			t.Errorf("%s: fresh probe health = %+v, want degraded (no data yet)", kind, h)
		}
		if err := p.Validate(map[string]any{}); err == nil {
			t.Errorf("%s: empty spec must not validate", kind)
		}
	}
}
