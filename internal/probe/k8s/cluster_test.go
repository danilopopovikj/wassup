package k8s

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/danilopopovikj/wassup/internal/probe"
)

const bookstoreKubeconfig = `apiVersion: v1
kind: Config
current-context: staging
clusters:
  - name: bookstore-production
    cluster: {server: "https://k8s.bookstore.example:6443"}
  - name: bookstore-staging
    cluster: {server: "https://k8s.staging.bookstore.example:6443"}
contexts:
  - name: production
    context: {cluster: bookstore-production, user: admin}
  - name: staging
    context: {cluster: bookstore-staging, user: admin}
users:
  - name: admin
    user: {token: not-a-real-token}
`

func writeKubeconfig(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(bookstoreKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// share puts fake clients in the cache for a kubeconfig and a context, the
// way a first probe would have.
func share(t *testing.T, kubeconfig, kubeContext string, c *Clients) {
	t.Helper()
	key := kubeconfig + "\x00" + kubeContext
	clientCache.Lock()
	clientCache.m[key] = c
	clientCache.Unlock()
	t.Cleanup(func() {
		clientCache.Lock()
		delete(clientCache.m, key)
		clientCache.Unlock()
	})
}

func TestDescribeNamesTheClusterBeforeAnythingIsRead(t *testing.T) {
	path := writeKubeconfig(t, t.TempDir(), "kubeconfig.yaml")
	nodes := []string{"node-1", "node-2", "node-3"}
	core := fake.NewClientset()
	for _, n := range nodes {
		if err := core.Tracker().Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}}); err != nil {
			t.Fatal(err)
		}
	}
	share(t, path, "", &Clients{Core: core})
	share(t, path, "production", &Clients{Core: core})

	got, err := Describe(t.Context(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Context != "staging" || got.Server != "https://k8s.staging.bookstore.example:6443" || got.Nodes != 3 || got.Kubeconfig != path {
		t.Errorf("the current context: %+v", got)
	}
	got, err = Describe(t.Context(), path, "production")
	if err != nil {
		t.Fatal(err)
	}
	if got.Context != "production" || got.Server != "https://k8s.bookstore.example:6443" {
		t.Errorf("the named context: %+v", got)
	}
	if line := got.String(); !strings.Contains(line, "context production") || !strings.Contains(line, "3 nodes") || !strings.Contains(line, path) {
		t.Errorf("line %q", line)
	}
}

func TestDescribeAnUnknownContextListsTheKnownOnes(t *testing.T) {
	path := writeKubeconfig(t, t.TempDir(), "kubeconfig.yaml")
	_, err := Describe(t.Context(), path, "prod")
	if err == nil || !strings.Contains(err.Error(), "production, staging") {
		t.Fatalf("err = %v, want the contexts the file has", err)
	}
}

func TestFindKubeconfigsLooksWhereGitDoesNot(t *testing.T) {
	root := t.TempDir()
	want := writeKubeconfig(t, root, "infra/terraform/out/kubeconfig-production.yaml")
	writeKubeconfig(t, root, "node_modules/pkg/kubeconfig.yaml")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("infra/terraform/out/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deployment.yaml"), []byte("kind: Deployment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := FindKubeconfigs(root)
	if len(got) != 1 || got[0].Path != want {
		t.Fatalf("found %+v, want only %s", got, want)
	}
	k := got[0]
	if k.Current != "staging" || strings.Join(k.Contexts, ",") != "production,staging" || k.Servers["production"] != "https://k8s.bookstore.example:6443" {
		t.Errorf("read %+v", k)
	}
}

func TestViaNamespace(t *testing.T) {
	cases := map[string]string{
		"k8s.service/shop/bookstore-db-rw:5432": "shop",
		"k8s.pod/shop/bookstore-db-1:5432":      "shop",
		"bastion":                               "",
		"k8s.service/shop":                      "",
	}
	for via, want := range cases {
		if got := ViaNamespace(via); got != want {
			t.Errorf("ViaNamespace(%q) = %q, want %q", via, got, want)
		}
	}
}

// A cluster that refuses the connection is reported in the first second,
// with what to check, and not after the caches gave up.
func TestAnAPIServerThatRefusesIsReportedAtOnce(t *testing.T) {
	cfg := &rest.Config{Host: "https://127.0.0.1:1", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	tuneConfig(cfg)
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &Clients{Core: core, REST: cfg}
	b := &base{kind: kindWorkload}
	out := make(chan probe.Observation, 4)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	began := time.Now()
	go func() { done <- b.await(ctx, c, out, "api", c.informers().pods) }()

	select {
	case o := <-out:
		if took := time.Since(began); took > 2*time.Second {
			t.Errorf("reported after %s", took)
		}
		if o.Target != "api" || o.Probe != kindWorkload {
			t.Errorf("observation %+v", o)
		}
		for _, want := range []string{"refused the connection", "127.0.0.1:1", "kubeconfig"} {
			if !strings.Contains(o.Err, want) {
				t.Errorf("error %q lacks %q", o.Err, want)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was reported")
	}
	if h := b.Health(); h.State != probe.HealthDegraded {
		t.Errorf("health %+v, want degraded: the server is asked again", h)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("await returned without a cluster")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await did not stop with its context")
	}
}

func TestClientsWithoutAServerAreTakenAsReached(t *testing.T) {
	c := &Clients{Core: fake.NewClientset()}
	if err := c.reach(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestReachErrorSaysWhatToCheck(t *testing.T) {
	cases := map[string]string{
		"dial tcp 10.0.0.1:6443: connect: connection refused": "refused the connection",
		"Unauthorized": "a token may have expired",
		"dial tcp: lookup k8s.bookstore.example: no such host": "does not resolve",
		"tls: failed to verify certificate: x509":              "may belong to another cluster",
		"context deadline exceeded":                            "no answer from the API server",
	}
	for in, want := range cases {
		err := reachError("https://k8s.bookstore.example:6443", errors.New(in))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), in) {
			t.Errorf("reachError(%q) = %v, want %q and the cause", in, err, want)
		}
	}
	if reachError("x", nil) != nil {
		t.Error("no error is no error")
	}
}
