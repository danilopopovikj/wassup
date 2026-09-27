package k8s

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestDiscover(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	sysPod := apiPod("coredns", true, 0, nil, nil)
	sysPod.Namespace = "kube-system"
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		node, apiDeployment("ghcr.io/bookstore/api:b7e9f21", 3), apiPod("api-1", true, 0, nil, nil), sysPod,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "api"}, Ports: []corev1.ServicePort{{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP}}}},
		ingressObj(),
		cronJobObj("docs-sync", "*/15 * * * *"),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "prod"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
	}
	dyn := newCNPGDynamic(cnpgCluster("db-1", nil))
	c := newTestClients(objs, nil, dyn)
	inv, err := Discover(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Namespaces) != 1 || inv.Namespaces[0] != "prod" {
		t.Errorf("namespaces = %v (kube-system must be skipped)", inv.Namespaces)
	}
	if len(inv.Nodes) != 1 || inv.Nodes[0].Roles[0] != "control-plane" || !inv.Nodes[0].Ready {
		t.Errorf("nodes = %+v", inv.Nodes)
	}
	if len(inv.Workloads) != 1 || inv.Workloads[0].Selector != "app=api" || inv.Workloads[0].Images[0] != "ghcr.io/bookstore/api:b7e9f21" || inv.Workloads[0].Nodes[0] != "node-1" {
		t.Errorf("workloads = %+v", inv.Workloads)
	}
	if len(inv.Services) != 1 || inv.Services[0].Ports[0] != "http:8000/TCP" {
		t.Errorf("services = %+v", inv.Services)
	}
	if len(inv.Ingresses) != 1 || inv.Ingresses[0].TLSSecrets[0] != "bookstore-tls" || inv.Ingresses[0].Hosts[0] != "bookstore.example" {
		t.Errorf("ingresses = %+v", inv.Ingresses)
	}
	if len(inv.CronJobs) != 1 || inv.CronJobs[0].Schedule != "every 15 min" {
		t.Errorf("cronjobs = %+v", inv.CronJobs)
	}
	if len(inv.PVCs) != 1 || inv.PVCs[0].Phase != "Bound" {
		t.Errorf("pvcs = %+v", inv.PVCs)
	}
	if len(inv.CNPGClusters) != 1 || inv.CNPGClusters[0].Instances != 3 || inv.CNPGClusters[0].Primary != "db-1" {
		t.Errorf("cnpg = %+v", inv.CNPGClusters)
	}
	if len(inv.Warnings) != 0 {
		t.Errorf("warnings = %v", inv.Warnings)
	}
	if _, err := json.Marshal(inv); err != nil {
		t.Errorf("json: %v", err)
	}

	// Explicit namespaces include kube-system when asked.
	inv, err = Discover(context.Background(), c, []string{"kube-system"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Workloads) != 0 || len(inv.Namespaces) != 1 {
		t.Errorf("kube-system inventory = %+v", inv)
	}
}
