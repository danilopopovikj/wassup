package k8s

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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
	dyn := newInventoryDynamic(cnpgCluster("db-1", nil))
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

// plainSecrets are the credentials the pod spec and the ConfigMap below
// hold in plain. The inventory is printed by `discover --json` and feeds
// evidence.json: none of them may be in it.
var plainSecrets = []string{"Pg-Pa55-7f3a", "Ch-Pa55-c0ffee", "Ch-Dsn-Pa55-9d41", "R3dis-Pa55-e2c1", "Tok-99f1", "Adm1n", "44e0", "In-A-Secret-0b7e", "db-user"}

// backendDeployment is a workload that takes its settings from a ConfigMap
// (envFrom and configMapKeyRef), from Secrets, and from values written in
// plain in its spec, credentials among them.
func backendDeployment() *appsv1.Deployment {
	d := apiDeployment("ghcr.io/bookstore/backend:4.1.0", 2)
	d.Name = "backend"
	d.Labels = map[string]string{"app.kubernetes.io/instance": "bookstore", "app": "from-the-object"}
	d.Annotations = map[string]string{"meta.helm.sh/release-name": "bookstore"}
	c := &d.Spec.Template.Spec.Containers[0]
	c.Name = "backend"
	c.Command = []string{"uvicorn", "app.main:app"}
	c.Args = []string{"--api-token", "Tok-99f1", "--admin-password", "Adm1n Pa55 44e0", "--db", "postgresql://db-user:Pg-Pa55-7f3a@bookstore-db-rw:5432/bookstore", "--workers", "4"}
	c.EnvFrom = []corev1.EnvFromSource{
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}}},
		{Prefix: "Q_", ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "queue-config"}}},
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "not-there"}}},
		{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-secrets"}}},
	}
	c.Env = []corev1.EnvVar{
		{Name: "POSTGRES_PASSWORD", Value: "Pg-Pa55-7f3a"},
		{Name: "CLICKHOUSE_PASSWORD", Value: "Ch-Pa55-c0ffee"},
		{Name: "CLICKHOUSE_DSN", Value: "tcp://clickhouse.platform:9000/?database=traces&username=admin&password=Ch-Dsn-Pa55-9d41"},
		{Name: "DATABASE_URL", Value: "postgresql://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@$(POSTGRES_SERVER):5432/$(POSTGRES_DB)"},
		{Name: "REPLICA_URL", Value: "postgresql://$(DB_USER):$(DB_PASS)@bookstore-db-ro:5432/bookstore"},
		{Name: "DB_USER", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "bookstore-db-app"}, Key: "username"}}},
		{Name: "ELECTRIC_URL", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "queue-config"}, Key: "ELECTRIC_URL"}}},
		{Name: "LOG_LEVEL", Value: "info"},
		{Name: "S3_BUCKET", Value: "bookstore-attachments"},
	}
	return d
}

func TestDiscoverKeepsNoCredentialAndFollowsConfigMaps(t *testing.T) {
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}},
		backendDeployment(), apiPod("backend-1", true, 0, nil, nil),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "prod"}, Data: map[string]string{
			"POSTGRES_SERVER": "bookstore-db-rw", "POSTGRES_USER": "db-user", "POSTGRES_DB": "bookstore", "CACHE_HOST": "cache",
			"HATCHET_CLIENT_HOST_PORT": "hatchet-engine.prod:7070", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://signoz-otel-collector.platform:4317",
			"REDIS_PASSWORD": "R3dis-Pa55-e2c1", "BROKER_URL": "redis://:R3dis-Pa55-e2c1@cache.prod:6379/1"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "queue-config", Namespace: "prod"}, Data: map[string]string{"ELECTRIC_URL": "http://electric.prod:3000", "HOST": "queue.prod"}},
		// a ConfigMap of the same name elsewhere must not be the one read
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "other"}, Data: map[string]string{"POSTGRES_SERVER": "wrong-db"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "app-secrets", Namespace: "prod"}, Data: map[string][]byte{"SESSION_KEY": []byte("In-A-Secret-0b7e")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bookstore-db-app", Namespace: "prod"}, Data: map[string][]byte{"username": []byte("db-user")}},
	}
	c := newTestClients(objs, nil, newInventoryDynamic())
	inv, err := Discover(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(inv, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plainSecrets {
		if strings.Contains(string(b), s) {
			t.Errorf("the inventory holds %q:\n%s", s, b)
		}
	}
	if strings.Contains(string(b), "@") {
		t.Errorf("a URL kept its user:\n%s", b)
	}
	// Secrets are named, never read.
	for _, a := range c.Core.(*fake.Clientset).Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Errorf("discovery touched a Secret: %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if len(inv.Workloads) != 1 {
		t.Fatalf("workloads = %+v", inv.Workloads)
	}
	w := inv.Workloads[0]
	env := map[string]EnvRef{}
	for _, e := range w.Env {
		if e.Name == "*" {
			env["*"+e.SecretRef+e.ConfigRef] = e
			continue
		}
		env[e.Name] = e
	}
	want := map[string]string{
		// envFrom, one variable per key of the ConfigMap
		"POSTGRES_SERVER": "bookstore-db-rw", "CACHE_HOST": "cache", "HATCHET_CLIENT_HOST_PORT": "hatchet-engine.prod:7070",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://signoz-otel-collector.platform:4317", "BROKER_URL": "redis://cache.prod:6379/1",
		"Q_HOST": "queue.prod", "Q_ELECTRIC_URL": "http://electric.prod:3000",
		// configMapKeyRef
		"ELECTRIC_URL": "http://electric.prod:3000",
		// $(VAR): from the ConfigMap, from the spec, and unknown because it is in a Secret
		"DATABASE_URL": "postgresql://bookstore-db-rw:5432", "REPLICA_URL": "postgresql://bookstore-db-ro:5432",
		"CLICKHOUSE_DSN": "tcp://clickhouse.platform:9000", "S3_BUCKET": "bookstore-attachments",
		// names without a value
		"POSTGRES_PASSWORD": "", "CLICKHOUSE_PASSWORD": "", "REDIS_PASSWORD": "", "LOG_LEVEL": "", "POSTGRES_USER": "", "DB_USER": "",
	}
	for name, value := range want {
		e, ok := env[name]
		if !ok {
			t.Errorf("%s is missing from the environment", name)
		} else if e.Value != value {
			t.Errorf("%s = %q, want %q", name, e.Value, value)
		}
	}
	if e := env["POSTGRES_SERVER"]; e.ConfigRef != "app-config/POSTGRES_SERVER" {
		t.Errorf("a variable from a ConfigMap says which: %+v", e)
	}
	if e := env["DB_USER"]; e.SecretRef != "bookstore-db-app/username" {
		t.Errorf("a variable from a Secret says which: %+v", e)
	}
	if _, ok := env["*app-secrets/*"]; !ok {
		t.Errorf("envFrom a Secret keeps the reference: %+v", w.Env)
	}
	if _, ok := env["*not-there/*"]; !ok || len(inv.Warnings) != 1 || !strings.Contains(inv.Warnings[0], "prod/not-there") {
		t.Errorf("a ConfigMap that cannot be read keeps the reference and warns: %v", inv.Warnings)
	}
	if len(w.Command) != 1 || !strings.Contains(w.Command[0], "uvicorn app.main:app") || !strings.Contains(w.Command[0], "--workers 4") ||
		!strings.Contains(w.Command[0], "postgresql://bookstore-db-rw:5432") {
		t.Errorf("the command should stay readable: %v", w.Command)
	}
	if w.Release != "bookstore" || w.Labels["app.kubernetes.io/instance"] != "bookstore" || w.Labels["app"] != "api" {
		t.Errorf("labels = %v (the pod's win over the object's), release = %q", w.Labels, w.Release)
	}
}

func TestDiscoverIngressServices(t *testing.T) {
	ing := ingressObj()
	pt := networkingv1.PathTypePrefix
	backend := func(svc string, port int32) networkingv1.IngressBackend {
		return networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: svc, Port: networkingv1.ServiceBackendPort{Number: port}}}
	}
	ing.Spec.Rules[0].HTTP.Paths = []networkingv1.HTTPIngressPath{
		{Path: "/api/v1/login", PathType: &pt, Backend: backend("backend", 8000)},
		{Path: "/api/v1/items", PathType: &pt, Backend: backend("backend", 8000)},
		{Path: "/shapes", PathType: &pt, Backend: backend("electric", 3000)},
		{Path: "/", PathType: &pt, Backend: backend("backend", 8000)},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}}}}
	c := newTestClients([]runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}, ing, node}, nil, newInventoryDynamic())
	inv, err := Discover(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Ingresses) != 1 {
		t.Fatalf("ingresses = %+v", inv.Ingresses)
	}
	got := inv.Ingresses[0]
	if strings.Join(got.Services, ",") != "backend,electric" {
		t.Errorf("four paths to two Services are two Services: %v", got.Services)
	}
	if len(got.Backends) != 4 || got.Backends[0] != "backend:8000 /api/v1/login" {
		t.Errorf("the rules stay readable: %v", got.Backends)
	}
	if got.Namespace != "prod" || got.Name != "web" {
		t.Errorf("the Ingress keeps its own namespace and name: %+v", got)
	}
	if len(inv.Nodes) != 1 || strings.Join(inv.Nodes[0].Taints, ",") != "dedicated=db:NoSchedule" {
		t.Errorf("taints = %+v", inv.Nodes)
	}
}
