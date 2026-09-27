package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestPVCUsageFromKubeletStats(t *testing.T) {
	sc := "local-path"
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "db-1", Namespace: "prod"},
		Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: &sc, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}}
	pod := apiPod("db-1", true, 0, nil, nil)
	pod.Spec.Volumes = []corev1.Volume{{Name: "pgdata", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "db-1"}}}}
	c := newTestClients([]runtime.Object{pvc, pod}, nil, nil)
	c.proxyGet = func(_ context.Context, path string) ([]byte, error) {
		return []byte(`{"node":{"nodeName":"node-1"},"pods":[{"podRef":{"name":"db-1","namespace":"prod"},"volume":[
			{"name":"pgdata","usedBytes":6100000000,"capacityBytes":10000000000,"availableBytes":3900000000,"pvcRef":{"name":"db-1","namespace":"prod"}}]}]}`), nil
	}
	p := &pvcProbe{base: base{kind: kindPVC, clients: c}}
	out, _ := startProbe(t, p, testSpec("db-primary", "namespace", "prod", "name", "db-1"))
	o := firstOK(t, out)
	if o.Metrics["used_bytes"] != 6.1e9 || o.Metrics["total_bytes"] != 1e10 || o.Metrics["disk_pct"] != 61 {
		t.Errorf("metrics = %v", o.Metrics)
	}
	if o.Detail["node"] != "node-1" || o.Detail["phase"] != "Bound" || o.Detail["storage_class"] != "local-path" {
		t.Errorf("detail = %v", o.Detail)
	}

	// Without the kubelet stats only the capacity is known.
	c2 := newTestClients([]runtime.Object{pvc, pod}, nil, nil)
	p2 := &pvcProbe{base: base{kind: kindPVC, clients: c2}}
	out2, _ := startProbe(t, p2, testSpec("db-primary", "namespace", "prod", "name", "db-1"))
	o = firstOK(t, out2)
	if _, ok := o.Metrics["disk_pct"]; ok {
		t.Errorf("disk_pct must be omitted without stats: %v", o.Metrics)
	}
	if o.Metrics["total_bytes"] != float64(10<<30) {
		t.Errorf("total_bytes = %v", o.Metrics["total_bytes"])
	}
	if h := p2.Health(); h.State != probe.HealthOK {
		t.Errorf("health = %+v", h)
	}
}
