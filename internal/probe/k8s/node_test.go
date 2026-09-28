package k8s

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestNodeNotReadyMemoryPressure(t *testing.T) {
	notReadySince := time.Now().Add(-14 * time.Minute)
	pressureSince := time.Now().Add(-10 * time.Minute)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
			NodeInfo:    corev1.NodeSystemInfo{KubeletVersion: "v1.33.4", BootID: "boot-1"},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionUnknown, Message: "kubelet stopped posting node status", LastTransitionTime: metav1.Time{Time: notReadySince}},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue, Reason: "KubeletHasInsufficientMemory", LastTransitionTime: metav1.Time{Time: pressureSince}},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse},
			},
		},
	}
	oom := apiPod("worker-6c9d-k2pq", true, 1, nil, &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.Now()})
	oom.Spec.NodeName = "node-2"
	other := apiPod("api-1", true, 0, nil, nil)
	other.Spec.NodeName = "node-2"
	elsewhere := apiPod("api-2", true, 0, nil, nil)
	nm := &metricsv1beta1.NodeMetrics{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}, Usage: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("2200m"), corev1.ResourceMemory: resource.MustParse("7680Mi")}}
	c := newTestClients([]runtime.Object{node, oom, other, elsewhere}, []runtime.Object{nm}, nil)
	var proxied string
	c.proxyGet = func(_ context.Context, path string) ([]byte, error) {
		proxied = path
		return []byte(`{"node":{"nodeName":"node-2","startTime":"2020-01-01T00:00:00Z","fs":{"usedBytes":48,"capacityBytes":100}}}`), nil
	}
	p := &nodeProbe{base: base{kind: kindNode, clients: c}}
	out, _ := startProbe(t, p, testSpec("node-2", "name", "node-2"))
	o := firstOK(t, out)

	if proxied != "/api/v1/nodes/node-2/proxy/stats/summary" {
		t.Errorf("proxy path = %q", proxied)
	}
	if o.Metrics["pods"] != 2 || o.Metrics["killed"] != 1 || o.Metrics["disk_pct"] != 48 {
		t.Errorf("metrics = %v", o.Metrics)
	}
	if o.Metrics["cpu_pct"] != 55 || o.Metrics["mem_pct"] != 93.8 {
		t.Errorf("usage = cpu %v mem %v", o.Metrics["cpu_pct"], o.Metrics["mem_pct"])
	}
	nr, ok := model.HasCondition(o.Conditions, model.CondNotReady)
	if !ok || nr.Ref != "node/node-2" || !nr.Since.Equal(notReadySince) || nr.Detail != "kubelet stopped posting node status" {
		t.Errorf("NotReady = %+v (ok %v)", nr, ok)
	}
	mp, ok := model.HasCondition(o.Conditions, model.CondMemoryPressure)
	if !ok || !mp.Since.Equal(pressureSince) {
		t.Errorf("MemoryPressure = %+v (ok %v)", mp, ok)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondDiskPressure); ok {
		t.Errorf("unexpected DiskPressure")
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondRebooted); ok {
		t.Errorf("unexpected Rebooted with an old boot")
	}
	if o.Detail["name"] != "node-2" {
		t.Errorf("the name the cluster knows the machine by: %v", o.Detail["name"])
	}
	if o.Detail["kubelet_version"] != "v1.33.4" {
		t.Errorf("detail = %v", o.Detail)
	}

	// A boot id change while watching is a reboot.
	node.Status.NodeInfo.BootID = "boot-2"
	if _, err := c.Core.CoreV1().Nodes().UpdateStatus(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	o = waitFor(t, out, func(o probe.Observation) bool {
		_, ok := model.HasCondition(o.Conditions, model.CondRebooted)
		return ok
	})
	if len(o.Events) != 1 || o.Events[0].Kind != "node" || o.Events[0].Summary != "reboot of node-2" {
		t.Errorf("events = %+v", o.Events)
	}
}

func TestNodeProxyDenied(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	c := newTestClients([]runtime.Object{node}, nil, nil)
	p := &nodeProbe{base: base{kind: kindNode, clients: c}}
	out, _ := startProbe(t, p, testSpec("node-1", "name", "node-1"))
	o := firstOK(t, out)
	if _, ok := o.Metrics["disk_pct"]; ok {
		t.Errorf("disk_pct must be omitted when the proxy is unavailable: %v", o.Metrics)
	}
	if len(o.Conditions) != 0 {
		t.Errorf("conditions = %+v", o.Conditions)
	}
	if o.Detail["stats_error"] == nil {
		t.Errorf("expected stats_error in detail: %v", o.Detail)
	}
}
