package k8s

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport/spdy"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// apiServer records what reaches it and answers every request with an
// empty object.
type apiServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newAPIServer(t *testing.T) (*apiServer, *rest.Config) {
	t.Helper()
	s := &apiServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "cnpg") {
			_, _ = w.Write([]byte(`{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)
	cfg := &rest.Config{Host: s.URL, TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	tuneConfig(cfg)
	return s, cfg
}

func (s *apiServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// Every way a client has to change the cluster is refused before the
// request is sent; the reads go through.
func TestClientsRefuseToChangeTheCluster(t *testing.T) {
	srv, cfg := newAPIServer(t)
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop"}}
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "bookstore-db", "namespace": "shop"},
	}}
	pods, clusters := core.CoreV1().Pods("shop"), dyn.Resource(cnpgClusterGVR).Namespace("shop")

	writes := map[string]func() error{
		"create a pod": func() error { _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); return err },
		"update a pod": func() error { _, err := pods.Update(ctx, pod, metav1.UpdateOptions{}); return err },
		"patch a pod": func() error {
			_, err := pods.Patch(ctx, "api-1", types.MergePatchType, []byte(`{}`), metav1.PatchOptions{})
			return err
		},
		"delete a pod":    func() error { return pods.Delete(ctx, "api-1", metav1.DeleteOptions{}) },
		"delete all pods": func() error { return pods.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{}) },
		"scale a deployment": func() error {
			_, err := core.AppsV1().Deployments("shop").Patch(ctx, "api", types.MergePatchType, []byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{}, "scale")
			return err
		},
		"cordon a node": func() error {
			_, err := core.CoreV1().Nodes().Patch(ctx, "node-1", types.MergePatchType, []byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{})
			return err
		},
		"delete a namespace": func() error { return core.CoreV1().Namespaces().Delete(ctx, "shop", metav1.DeleteOptions{}) },
		"run a command in a pod": func() error {
			return core.CoreV1().RESTClient().Post().Resource("pods").Namespace("shop").Name("api-1").SubResource("exec").Do(ctx).Error()
		},
		"create a database cluster": func() error { _, err := clusters.Create(ctx, cluster, metav1.CreateOptions{}); return err },
		"update a database cluster": func() error { _, err := clusters.Update(ctx, cluster, metav1.UpdateOptions{}); return err },
		"delete a database cluster": func() error { return clusters.Delete(ctx, "bookstore-db", metav1.DeleteOptions{}) },
	}
	for name, write := range writes {
		if err := write(); !errors.Is(err, probe.ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrReadOnly", name, err)
		}
	}
	if got := srv.requests(); len(got) != 0 {
		t.Fatalf("the API server saw %v, a refused request was sent", got)
	}

	if _, err := pods.Get(ctx, "api-1", metav1.GetOptions{}); err != nil {
		t.Errorf("get a pod: %v", err)
	}
	if _, err := pods.List(ctx, metav1.ListOptions{}); err != nil {
		t.Errorf("list pods: %v", err)
	}
	if _, err := clusters.Get(ctx, "bookstore-db", metav1.GetOptions{}); err != nil {
		t.Errorf("get a database cluster: %v", err)
	}
	want := []string{
		"GET /api/v1/namespaces/shop/pods/api-1",
		"GET /api/v1/namespaces/shop/pods",
		"GET /apis/postgresql.cnpg.io/v1/namespaces/shop/clusters/bookstore-db",
	}
	got := srv.requests()
	if len(got) != len(want) {
		t.Fatalf("the API server saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The clients NewClients hands out are built from a guarded configuration.
func TestNewClientsAreReadOnly(t *testing.T) {
	cfg := &rest.Config{}
	tuneConfig(cfg)
	if cfg.WrapTransport == nil {
		t.Fatal("the configuration has no transport wrapper")
	}
	rt := cfg.WrapTransport(http.DefaultTransport)
	req := httptest.NewRequest(http.MethodDelete, "https://cluster.example/api/v1/namespaces/shop", nil)
	if _, err := rt.RoundTrip(req); !errors.Is(err, probe.ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
}

// The port-forward is the one POST that is sent, and only to a pod's
// portforward: the same transport refuses exec and attach.
func TestOnlyThePortForwardIsAPost(t *testing.T) {
	srv, cfg := newAPIServer(t)
	rt, _, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"exec", "attach", "eviction", "binding", "ephemeralcontainers"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/namespaces/shop/pods/db-1/"+sub, nil)
		if _, err := rt.RoundTrip(req); !errors.Is(err, probe.ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrReadOnly", sub, err)
		}
	}
	if got := srv.requests(); len(got) != 0 {
		t.Fatalf("the API server saw %v, a refused request was sent", got)
	}
	// The port-forward itself gets through the guard and reaches the server,
	// which here does not upgrade the connection.
	open := srv.URL + "/api/v1/namespaces/shop/pods/db-1/portforward"
	req, _ := http.NewRequest(http.MethodPost, open, nil)
	if _, err := rt.RoundTrip(req); errors.Is(err, probe.ErrReadOnly) {
		t.Fatalf("the port-forward was refused: %v", err)
	}
	if got := srv.requests(); len(got) != 1 || got[0] != "POST /api/v1/namespaces/shop/pods/db-1/portforward" {
		t.Fatalf("the API server saw %v, want the port-forward", got)
	}

	for path, want := range map[string]bool{
		"/api/v1/namespaces/shop/pods/db-1/portforward":      true,
		"/api/v1/namespaces/shop/services/db/portforward":    false,
		"/api/v1/namespaces/shop/pods/portforward":           false,
		"/api/v1/namespaces/portforward/pods/db-1":           false,
		"/api/v1/namespaces/shop/pods/db-1/portforward/x":    false,
		"/api/v1/namespaces/shop/pods/db-1/exec/portforward": false,
	} {
		req := httptest.NewRequest(http.MethodPost, "https://cluster.example"+path, nil)
		if got := opensPortForward(req); got != want {
			t.Errorf("opensPortForward(%s) = %v, want %v", path, got, want)
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(method, "https://cluster.example/api/v1/namespaces/shop/pods/db-1/portforward", nil)
		if opensPortForward(req) {
			t.Errorf("%s to portforward is allowed", method)
		}
	}
}
