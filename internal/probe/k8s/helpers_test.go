package k8s

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// newTestClients builds Clients on the fake clientsets. The metrics fake's
// tracker files NodeMetrics under "nodemetricses" while the typed client
// asks for "nodes", so the metrics objects are served through reactors.
func newTestClients(objs []runtime.Object, metrics []runtime.Object, dyn dynamic.Interface) *Clients {
	mc := metricsfake.NewSimpleClientset()
	mc.PrependReactor("get", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ga := a.(k8stesting.GetAction)
		for _, o := range metrics {
			if nm, ok := o.(*metricsv1beta1.NodeMetrics); ok && nm.Name == ga.GetName() {
				return true, nm, nil
			}
		}
		return true, nil, errors.NewNotFound(schema.GroupResource{Group: "metrics.k8s.io", Resource: "nodes"}, ga.GetName())
	})
	mc.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		list := &metricsv1beta1.PodMetricsList{}
		for _, o := range metrics {
			if pm, ok := o.(*metricsv1beta1.PodMetrics); ok && (a.GetNamespace() == "" || pm.Namespace == a.GetNamespace()) {
				list.Items = append(list.Items, *pm)
			}
		}
		return true, list, nil
	})
	return &Clients{
		Core:    fake.NewSimpleClientset(objs...),
		Metrics: mc,
		Dynamic: dyn,
	}
}

// testSpec is a spec with a fast tick.
func testSpec(target string, kv ...string) map[string]any {
	spec := map[string]any{"_target": target, "_tick": 50 * time.Millisecond}
	for i := 0; i+1 < len(kv); i += 2 {
		spec[kv[i]] = kv[i+1]
	}
	return spec
}

// startProbe starts p and returns its observation channel and a cancel.
func startProbe(t *testing.T, p probe.Probe, spec map[string]any) (<-chan probe.Observation, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan probe.Observation, 64)
	if err := p.Start(ctx, spec, out); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(cancel)
	return out, cancel
}

// waitFor reads observations until pred accepts one or the timeout passes.
func waitFor(t *testing.T, out <-chan probe.Observation, pred func(probe.Observation) bool) probe.Observation {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var last probe.Observation
	for {
		select {
		case o := <-out:
			last = o
			if pred(o) {
				return o
			}
		case <-deadline:
			t.Fatalf("timed out waiting for observation; last: %+v", last)
		}
	}
}

// firstOK reads the first observation without an error.
func firstOK(t *testing.T, out <-chan probe.Observation) probe.Observation {
	t.Helper()
	return waitFor(t, out, func(o probe.Observation) bool { return o.Err == "" })
}
