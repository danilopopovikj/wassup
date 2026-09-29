package k8s

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

func int32p(v int32) *int32 { return &v }

func apiPod(name string, ready bool, restarts int32, waiting *corev1.ContainerStateWaiting, lastTerm *corev1.ContainerStateTerminated) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", Labels: map[string]string{"app": "api"}, UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "api", Image: "ghcr.io/bookstore/api:b7e9f21",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &metav1.Time{Time: time.Now().Add(-time.Hour)},
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: restarts, Ready: ready}}},
	}
	cs := &p.Status.ContainerStatuses[0]
	if waiting != nil {
		cs.State.Waiting = waiting
	} else {
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.Now()}
	}
	if lastTerm != nil {
		cs.LastTerminationState.Terminated = lastTerm
	}
	return p
}

func apiDeployment(image string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod", UID: "dep-api", Annotations: map[string]string{"deployment.kubernetes.io/revision": "3"}},
		Spec: appsv1.DeploymentSpec{Replicas: int32p(replicas), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: image}}}}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, Replicas: 3},
	}
}

func podMetrics(name string, cpuMilli, memBytes int64) *metricsv1beta1.PodMetrics {
	return &metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod"},
		Containers: []metricsv1beta1.ContainerMetrics{{Name: "api", Usage: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(memBytes, resource.BinarySI)}}}}
}

func TestWorkloadCrashLoopAndDeploy(t *testing.T) {
	crashedAt := time.Now().Add(-90 * time.Second)
	crash := apiPod("api-7d9f4b-x2k9", false, 6,
		&corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"},
		&corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, FinishedAt: metav1.Time{Time: crashedAt}})
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "api-7d9f4b", Namespace: "prod", Labels: map[string]string{"app": "api"},
		Annotations:       map[string]string{"deployment.kubernetes.io/revision": "3"},
		CreationTimestamp: metav1.Time{Time: time.Now().Add(-2 * time.Minute)},
		OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: "dep-api"}}}}
	objs := []runtime.Object{
		apiDeployment("ghcr.io/bookstore/api:b7e9f21", 3), rs,
		apiPod("api-7d9f4b-aaaa", true, 0, nil, nil),
		apiPod("api-7d9f4b-bbbb", true, 0, nil, nil),
		crash,
	}
	metrics := []runtime.Object{podMetrics("api-7d9f4b-aaaa", 400, 512<<20), podMetrics("api-7d9f4b-bbbb", 400, 512<<20)}
	c := newTestClients(objs, metrics, nil)
	p := &workloadProbe{base: base{kind: kindWorkload, clients: c}}
	spec := testSpec("api", "namespace", "prod", "selector", "app=api")
	if err := p.Validate(spec); err != nil {
		t.Fatal(err)
	}
	out, _ := startProbe(t, p, spec)
	o := firstOK(t, out)

	if o.Target != "api" || o.Probe != kindWorkload {
		t.Fatalf("target/probe = %q/%q", o.Target, o.Probe)
	}
	if o.Metrics["replicas_desired"] != 3 || o.Metrics["replicas_ready"] != 2 {
		t.Errorf("replicas = %v", o.Metrics)
	}
	if o.Metrics["restarts"] != 1 || o.Metrics["restart_window_s"] != 300 {
		t.Errorf("restarts = %v want 1 in 300s window", o.Metrics)
	}
	// Two pods at 400m of 1000m and 512Mi of 1Gi: 40% and 50%.
	if o.Metrics["cpu_pct"] != 40 || o.Metrics["mem_pct"] != 50 {
		t.Errorf("usage = cpu %v mem %v", o.Metrics["cpu_pct"], o.Metrics["mem_pct"])
	}
	cond, ok := model.HasCondition(o.Conditions, model.CondCrashLoopBackOff)
	if !ok {
		t.Fatalf("no CrashLoopBackOff in %+v", o.Conditions)
	}
	if cond.Ref != "pod/api-7d9f4b-x2k9" || !cond.Since.Equal(crashedAt) || cond.Detail == "" {
		t.Errorf("condition = %+v", cond)
	}
	if o.Detail["image"] != "ghcr.io/bookstore/api:b7e9f21" || o.Detail["kind"] != "Deployment" {
		t.Errorf("detail = %v", o.Detail)
	}
	// The rollout two minutes ago is placed as a deploy marker on the first tick.
	if len(o.Events) != 1 || o.Events[0].Kind != "deploy" || o.Events[0].Summary != "deploy of api b7e9f21" || o.Events[0].Ref != "b7e9f21" {
		t.Errorf("first-tick events = %+v", o.Events)
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Errorf("health = %+v", h)
	}

	// Another restart shows up as a delta.
	crash.Status.ContainerStatuses[0].RestartCount = 7
	crash.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = metav1.Now()
	if _, err := c.Core.CoreV1().Pods("prod").UpdateStatus(context.Background(), crash, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, func(o probe.Observation) bool { return o.Metrics["restarts"] == 2 })

	// A new image tag is a deploy event.
	dep := apiDeployment("ghcr.io/bookstore/api:c0ffee1", 3)
	if _, err := c.Core.AppsV1().Deployments("prod").Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	o = waitFor(t, out, func(o probe.Observation) bool {
		for _, e := range o.Events {
			if e.Kind == "deploy" && e.Ref == "c0ffee1" {
				return true
			}
		}
		return false
	})
	if o.Events[len(o.Events)-1].Summary != "deploy of api c0ffee1" {
		t.Errorf("deploy event = %+v", o.Events)
	}

	// Scaling is a scale event.
	dep = apiDeployment("ghcr.io/bookstore/api:c0ffee1", 6)
	if _, err := c.Core.AppsV1().Deployments("prod").Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, func(o probe.Observation) bool {
		for _, e := range o.Events {
			if e.Kind == "scale" && e.Summary == "scale of api to 6" {
				return true
			}
		}
		return false
	})
}

func TestWorkloadByNameAndOOM(t *testing.T) {
	killedAt := time.Now().Add(-2 * time.Minute)
	oom := apiPod("api-7d9f4b-oom1", true, 1, nil,
		&corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.Time{Time: killedAt}})
	evicted := apiPod("api-7d9f4b-evic", false, 0, nil, nil)
	evicted.Status.Phase = corev1.PodFailed
	evicted.Status.Reason = "Evicted"
	evicted.Status.Message = "The node was low on resource: memory."
	c := newTestClients([]runtime.Object{apiDeployment("ghcr.io/bookstore/api:b7e9f21", 3), oom, evicted}, nil, nil)
	p := &workloadProbe{base: base{kind: kindWorkload, clients: c}}
	out, _ := startProbe(t, p, testSpec("api", "namespace", "prod", "name", "api", "kind", "Deployment"))
	o := firstOK(t, out)
	if o.Metrics["killed"] != 1 || o.Metrics["evicted"] != 1 || o.Metrics["restarts"] != 1 {
		t.Errorf("metrics = %v", o.Metrics)
	}
	cond, ok := model.HasCondition(o.Conditions, model.CondOOMKilled)
	if !ok || cond.Detail != "limit 1Gi" || cond.Ref != "pod/api-7d9f4b-oom1" {
		t.Errorf("OOMKilled = %+v (ok %v)", cond, ok)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondEvicted); !ok {
		t.Errorf("no Evicted in %+v", o.Conditions)
	}
	if _, ok := o.Metrics["cpu_pct"]; ok {
		t.Errorf("cpu_pct must be omitted without metrics, got %v", o.Metrics)
	}
}

func TestWorkloadValidate(t *testing.T) {
	p := &workloadProbe{}
	cases := []struct {
		spec map[string]any
		ok   bool
	}{
		{map[string]any{"namespace": "prod", "selector": "app=api"}, true},
		{map[string]any{"namespace": "prod", "name": "api", "kind": "Deployment"}, true},
		{map[string]any{"namespace": "prod", "name": "api"}, false},
		{map[string]any{"namespace": "prod", "name": "api", "kind": "Job"}, false},
		{map[string]any{"selector": "app=api"}, false},
		{map[string]any{"namespace": "prod", "selector": "app in ("}, false},
	}
	for _, tc := range cases {
		if err := p.Validate(tc.spec); (err == nil) != tc.ok {
			t.Errorf("Validate(%v) = %v, want ok=%v", tc.spec, err, tc.ok)
		}
	}
}

func TestWorkloadNotFoundIsDegraded(t *testing.T) {
	c := newTestClients(nil, nil, nil)
	p := &workloadProbe{base: base{kind: kindWorkload, clients: c}}
	out, _ := startProbe(t, p, testSpec("api", "namespace", "prod", "name", "api", "kind", "Deployment"))
	o := waitFor(t, out, func(o probe.Observation) bool { return o.Err != "" })
	if o.Err != "deployment prod/api not found" {
		t.Errorf("err = %q", o.Err)
	}
	if h := p.Health(); h.State != probe.HealthDegraded {
		t.Errorf("health = %+v", h)
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/bookstore/api:b7e9f21":       "b7e9f21",
		"localhost:5000/api:v1":               "v1",
		"nginx":                               "latest",
		"ghcr.io/x/y@sha256:abcdef0123456789": "abcdef012345",
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// A pod that ran to its end keeps the labels of the workload. It holds no
// place on its machine, so the machine does not read as one with a replica
// that is not ready.
func TestWorkloadPlacementLeavesOutPodsThatEnded(t *testing.T) {
	migrated := apiPod("api-migrate-x7k2p", false, 0, nil, nil)
	migrated.Status.Phase = corev1.PodSucceeded
	evicted := apiPod("api-7d9f4b-cccc", false, 0, nil, nil)
	evicted.Status.Phase, evicted.Status.Reason = corev1.PodFailed, "Evicted"
	elsewhere := apiPod("api-7d9f4b-bbbb", true, 0, nil, nil)
	elsewhere.Spec.NodeName = "node-2"
	objs := []runtime.Object{
		apiDeployment("ghcr.io/bookstore/api:b7e9f21", 2),
		apiPod("api-7d9f4b-aaaa", true, 0, nil, nil), elsewhere, migrated, evicted,
	}
	// the pod on node-2 has no usage yet: node-2 has no share, not 0 %
	metrics := []runtime.Object{podMetrics("api-7d9f4b-aaaa", 250, 256<<20)}
	p := &workloadProbe{base: base{kind: kindWorkload, clients: newTestClients(objs, metrics, nil)}}
	out, _ := startProbe(t, p, testSpec("api", "namespace", "prod", "selector", "app=api"))
	o := firstOK(t, out)
	placement, _ := o.Detail["placement"].(map[string]any)
	want := map[string]map[string]any{
		"node-1": {"pods": 1, "ready": 1, "cpu_pct": 25.0, "mem_pct": 25.0},
		"node-2": {"pods": 1, "ready": 1},
	}
	if len(placement) != len(want) {
		t.Fatalf("placement = %v", o.Detail["placement"])
	}
	for node, w := range want {
		got, _ := placement[node].(map[string]any)
		if len(got) != len(w) {
			t.Errorf("%s = %v, want %v", node, got, w)
		}
		for k, v := range w {
			if got[k] != v {
				t.Errorf("%s %s = %v, want %v", node, k, got[k], v)
			}
		}
	}
}
