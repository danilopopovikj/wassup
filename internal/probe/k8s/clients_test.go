package k8s

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// testKubeconfig is a kubeconfig for a cluster nothing listens on: building
// the clients reads it, no request is sent.
const testKubeconfig = `apiVersion: v1
kind: Config
current-context: bookstore
clusters:
  - name: bookstore
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: bookstore
    context:
      cluster: bookstore
      user: reader
users:
  - name: reader
    user:
      token: not-a-real-token
`

func TestTuneConfigRaisesTheRateLimit(t *testing.T) {
	cfg := &rest.Config{}
	tuneConfig(cfg)
	if cfg.QPS != 50 || cfg.Burst != 100 {
		t.Errorf("QPS = %v, Burst = %v, want 50 and 100", cfg.QPS, cfg.Burst)
	}
	if cfg.UserAgent != "wassup" {
		t.Errorf("UserAgent = %q", cfg.UserAgent)
	}
}

// The configuration the clients are really built from carries the raised
// limits, not only the helper.
func TestNewClientsRaisesTheRateLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClients(path, "")
	if err != nil {
		t.Fatalf("NewClients: %v", err)
	}
	if c.REST == nil {
		t.Fatal("no REST config on the clients")
	}
	// The library's defaults are 5 and 10, and a zero value means the default.
	if c.REST.QPS < 50 || c.REST.Burst < 100 {
		t.Errorf("QPS = %v, Burst = %v, want at least 50 and 100", c.REST.QPS, c.REST.Burst)
	}
	if c.REST.Host != "https://127.0.0.1:1" {
		t.Errorf("Host = %q, the kubeconfig was not the one read", c.REST.Host)
	}
}

// captureStderr runs fn with the process's stderr replaced by a pipe and
// returns what was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	got := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		got <- string(b)
	}()
	fn()
	klog.Flush()
	os.Stderr = old
	_ = w.Close()
	out := <-got
	_ = r.Close()
	return out
}

// logEverywhere writes through every door the Kubernetes client uses.
func logEverywhere() {
	klog.Info("Waited before sending request: client-side throttling")
	klog.Warning("a warning from the client")
	klog.Error("an error from the client")
	klog.ErrorS(errors.New("reflector"), "failed to watch")
	klog.FromContext(context.Background()).Info("Waited before sending request", "reason", "client-side throttling")
	klog.FromContext(context.Background()).Error(errors.New("stream"), "lost connection to pod")
	utilruntime.HandleError(errors.New("an unhandled error from an informer"))
	utilruntime.HandleErrorWithContext(context.Background(), errors.New("forward"), "an error occurred forwarding")
}

// The package quiets the log when it is loaded, unless the debug variable is
// set: no caller has to remember to ask. This test stands before the ones
// that switch the log themselves, so it sees what loading left behind.
func TestClientLogIsQuietedOnLoad(t *testing.T) {
	if os.Getenv(debugEnv) != "" {
		t.Skipf("%s is set", debugEnv)
	}
	if len(utilruntime.ErrorHandlers) != 1 {
		t.Fatalf("%d unhandled-error handlers, want the one that drops", len(utilruntime.ErrorHandlers))
	}
	if out := captureStderr(t, logEverywhere); out != "" {
		t.Errorf("the client's log reached stderr:\n%s", out)
	}
}

func TestClientLogGoesNowhereByDefault(t *testing.T) {
	quietClientLog(true)
	if out := captureStderr(t, logEverywhere); out != "" {
		t.Errorf("the client's log reached stderr:\n%s", out)
	}
}

func TestClientLogReachesStderrWithDebug(t *testing.T) {
	t.Cleanup(func() { quietClientLog(true) })
	quietClientLog(false)
	out := captureStderr(t, logEverywhere)
	for _, want := range []string{"client-side throttling", "an error from the client", "an unhandled error from an informer"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
}
