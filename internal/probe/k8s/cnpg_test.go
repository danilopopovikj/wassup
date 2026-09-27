package k8s

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

func cnpgPod(name, role string, ready bool) *corev1.Pod {
	p := apiPod(name, ready, 0, nil, nil)
	p.Labels = map[string]string{labelCNPGCluster: "db", labelCNPGRole: role}
	return p
}

func cnpgCluster(primary string, failed []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "db", "namespace": "prod"},
		"spec":     map[string]any{"instances": int64(3)},
		"status": map[string]any{"readyInstances": int64(2), "instances": int64(3), "phase": "Cluster in healthy state",
			"currentPrimary": primary, "targetPrimary": primary,
			"instancesStatus": map[string]any{"healthy": []any{"db-1", "db-2"}, "failed": failed}},
	}}
}

func newCNPGDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpgClusterGVR: "ClusterList", cnpgBackupGVR: "BackupList"}, objs...)
}

func TestCNPGClusterReplicationBrokenAndSwitchover(t *testing.T) {
	backup := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "db-nightly", "namespace": "prod"},
		"spec":     map[string]any{"cluster": map[string]any{"name": "db"}},
		"status":   map[string]any{"phase": "running", "startedAt": time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
	}}
	dyn := newCNPGDynamic(cnpgCluster("db-1", []any{"db-3"}), backup)
	c := newTestClients([]runtime.Object{cnpgPod("db-1", "primary", true), cnpgPod("db-2", "replica", true), cnpgPod("db-3", "replica", false)}, nil, dyn)
	p := &cnpgClusterProbe{base: base{kind: kindCNPGCluster, clients: c}}
	out, _ := startProbe(t, p, testSpec("db", "namespace", "prod", "cluster", "db"))
	o := firstOK(t, out)
	if o.Metrics["replicas_ready"] != 2 || o.Metrics["replicas_desired"] != 3 {
		t.Errorf("metrics = %v", o.Metrics)
	}
	rb, ok := model.HasCondition(o.Conditions, model.CondReplicationBroken)
	if !ok || rb.Ref != "pod/db-3" {
		t.Errorf("ReplicationBroken = %+v (ok %v)", rb, ok)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondBackup); !ok {
		t.Errorf("no Backup in %+v", o.Conditions)
	}
	if o.Detail["primary"] != "db-1" || o.Detail["phase"] != "Cluster in healthy state" {
		t.Errorf("detail = %v", o.Detail)
	}

	// The primary moves: a switchover event.
	updated := cnpgCluster("db-2", nil)
	if _, err := dyn.Resource(cnpgClusterGVR).Namespace("prod").Update(context.Background(), updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	o = waitFor(t, out, func(o probe.Observation) bool { return len(o.Events) > 0 })
	if o.Events[0].Kind != "switchover" || o.Events[0].Summary != "switchover to db-2" {
		t.Errorf("event = %+v", o.Events[0])
	}
}

func TestCNPGInstance(t *testing.T) {
	pods := []runtime.Object{cnpgPod("db-1", "primary", true), cnpgPod("db-2", "replica", false)}
	metrics := []runtime.Object{podMetrics("db-1", 250, 256<<20)}
	c := newTestClients(pods, metrics, nil)

	p := &cnpgInstanceProbe{base: base{kind: kindCNPGInstance, clients: c}}
	out, _ := startProbe(t, p, testSpec("db-primary", "namespace", "prod", "cluster", "db", "role", "primary"))
	o := firstOK(t, out)
	if o.Metrics["cpu_pct"] != 25 || o.Metrics["mem_pct"] != 25 || len(o.Conditions) != 0 {
		t.Errorf("primary: %v %+v", o.Metrics, o.Conditions)
	}
	if o.Detail["pod"] != "db-1" || o.Detail["role"] != "primary" || o.Detail["node"] != "node-1" {
		t.Errorf("detail = %v", o.Detail)
	}

	p2 := &cnpgInstanceProbe{base: base{kind: kindCNPGInstance, clients: c}}
	out2, _ := startProbe(t, p2, testSpec("db-r1", "namespace", "prod", "cluster", "db", "instance", "db-2"))
	o = firstOK(t, out2)
	if nr, ok := model.HasCondition(o.Conditions, model.CondNotReady); !ok || nr.Ref != "pod/db-2" {
		t.Errorf("NotReady = %+v (ok %v)", nr, ok)
	}

	bad := &cnpgInstanceProbe{}
	if err := bad.Validate(map[string]any{"namespace": "prod", "cluster": "db"}); err == nil {
		t.Errorf("role or instance must be required")
	}
	if err := bad.Validate(map[string]any{"namespace": "prod", "cluster": "db", "role": "standby"}); err == nil {
		t.Errorf("bad role must be rejected")
	}
	if h := p.Health(); h.State != probe.HealthOK {
		t.Errorf("health = %+v", h)
	}
}
