package k8s

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// syncTimeout bounds the wait for the informer caches to fill.
const syncTimeout = 30 * time.Second

const (
	// reachTimeout bounds the question whether the API server answers.
	reachTimeout = 5 * time.Second
	// reachFresh is how long an answer counts for the probes that ask
	// next: they all start in the same second.
	reachFresh = 3 * time.Second
	// reachRetry is the pause before a server that did not answer is asked
	// again.
	reachRetry = 5 * time.Second
)

const (
	// clientQPS and clientBurst replace the library's defaults of 5 and 10,
	// which are sized for a controller that writes. wassup only reads, and
	// starts the informers of every bound probe in the same second: with the
	// defaults the requests queue up behind the client's own rate limiter,
	// the first picture arrives late and the library reports each wait.
	clientQPS   = 50
	clientBurst = 100

	// debugEnv is the environment variable that leaves the Kubernetes
	// client's own log on stderr.
	debugEnv = "WASSUP_DEBUG"
)

// defaultErrorHandlers is the library's handler list as it was at start, put
// back when the client's log is wanted.
var defaultErrorHandlers = utilruntime.ErrorHandlers

// The Kubernetes client logs through a process-wide logger, and what it has
// to say (throttling waits, reflector retries, port-forward stream errors)
// lands on stderr in the middle of the diagram. A probe reports its failures
// through Observation.Err and its health, so nothing is lost by dropping the
// library's copy. It is set once, before any goroutine logs.
func init() { quietClientLog(os.Getenv(debugEnv) == "") }

// quietClientLog sends the Kubernetes client's log and its unhandled-error
// reports nowhere when quiet is true, and back to stderr when it is false.
func quietClientLog(quiet bool) {
	if !quiet {
		klog.ClearLogger()
		klog.LogToStderr(true)
		utilruntime.ErrorHandlers = defaultErrorHandlers
		return
	}
	// A logger takes every message whatever its severity; the two settings
	// after it cover code that writes to the log's files directly.
	klog.SetSlogLogger(slog.New(slog.DiscardHandler))
	klog.LogToStderr(false)
	klog.SetOutput(io.Discard)
	utilruntime.ErrorHandlers = []utilruntime.ErrorHandler{dropError}
}

// dropError is the unhandled-error handler of a quiet run.
func dropError(context.Context, error, string, ...any) {}

// tuneConfig sets what every client of wassup shares: its name towards the
// API server, a rate limit that fits a reader starting many informers, and
// a transport that only reads. Every client is built from a config that
// went through here, the port-forward's included, so a create, an update, a
// patch or a delete is refused before it reaches the API server.
func tuneConfig(cfg *rest.Config) {
	cfg.UserAgent = "wassup"
	cfg.QPS = clientQPS
	cfg.Burst = clientBurst
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return probe.ReadOnlyExcept(rt, opensPortForward)
	})
}

// opensPortForward reports whether a request opens a port-forward to a pod.
// It is a POST because it upgrades the connection to a stream; it creates
// nothing in the cluster, and what goes through the stream is up to the
// probe that asked for it, which only reads.
func opensPortForward(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	// The path ends in api/v1/namespaces/<namespace>/pods/<pod>/portforward;
	// what comes before is the prefix of an API server behind a proxy.
	p := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	n := len(p)
	return n >= 7 && p[n-7] == "api" && p[n-6] == "v1" && p[n-5] == "namespaces" &&
		p[n-3] == "pods" && p[n-1] == "portforward"
}

// Clients bundles every client a probe may need for one cluster. Probes bound
// to the same kubeconfig and context share one Clients and, through it, one
// shared informer factory.
type Clients struct {
	Core    kubernetes.Interface
	Metrics metricsclientset.Interface
	Dynamic dynamic.Interface
	REST    *rest.Config
	Context string

	// proxyGet fetches a raw path on the API server, used for the kubelet
	// proxy (/api/v1/nodes/<name>/proxy/stats/summary). It is replaceable in
	// tests; the default goes through Core's REST client.
	proxyGet func(ctx context.Context, path string) ([]byte, error)

	infOnce sync.Once
	inf     *informerSet

	// The last answer to reach, shared by the probes of one cluster.
	reachMu  sync.Mutex
	reachAt  time.Time
	reachErr error
}

// reach asks the API server for its version, which is the cheapest request
// there is, so that a server that refuses the connection or the credentials
// is reported in the first second and not after the caches gave up. Clients
// without a server behind them (tests) are taken as reached.
func (c *Clients) reach(ctx context.Context) error {
	if c.Core == nil {
		return nil
	}
	rc, ok := c.Core.CoreV1().RESTClient().(*rest.RESTClient)
	if !ok || rc == nil {
		return nil
	}
	c.reachMu.Lock()
	defer c.reachMu.Unlock()
	if !c.reachAt.IsZero() && time.Since(c.reachAt) < reachFresh {
		return c.reachErr
	}
	rctx, cancel := context.WithTimeout(ctx, reachTimeout)
	defer cancel()
	_, err := rc.Get().AbsPath("/version").DoRaw(rctx)
	if err != nil && ctx.Err() != nil {
		// The probe was stopped: that says nothing about the server.
		return ctx.Err()
	}
	host := ""
	if c.REST != nil {
		host = c.REST.Host
	}
	c.reachAt, c.reachErr = time.Now(), reachError(host, err)
	return c.reachErr
}

// reachError words why the API server did not answer, with what to check.
func reachError(host string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("the API server at %s refused the connection: check the kubeconfig and the context, and that this machine reaches the cluster (VPN, firewall) (%w)", host, err)
	case apierrors.IsUnauthorized(err) || strings.Contains(msg, "unauthorized"):
		return fmt.Errorf("the API server at %s does not accept the credentials of the kubeconfig: a token may have expired (%w)", host, err)
	case strings.Contains(msg, "no such host"):
		return fmt.Errorf("the name of the API server %s does not resolve on this machine: check the kubeconfig, and the VPN when the name is a private one (%w)", host, err)
	case strings.Contains(msg, "certificate"):
		return fmt.Errorf("the certificate of the API server at %s was not accepted: the kubeconfig may belong to another cluster (%w)", host, err)
	case strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "timeout"):
		return fmt.Errorf("no answer from the API server at %s within %s: check that this machine reaches the cluster (VPN, firewall) (%w)", host, reachTimeout, err)
	}
	return fmt.Errorf("the API server at %s did not answer: %w", host, err)
}

// NewClients builds the clients for a cluster. The kubeconfig is, in order,
// the explicit argument, WASSUP_KUBECONFIG, KUBECONFIG, ~/.kube/config and
// finally the in-cluster service account. An empty context keeps the
// kubeconfig's current context.
func NewClients(kubeconfig, kubeContext string) (*Clients, error) {
	cfg, err := restConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	tuneConfig(cfg)
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	metrics, err := metricsclientset.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("metrics client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	return &Clients{Core: core, Metrics: metrics, Dynamic: dyn, REST: cfg, Context: kubeContext}, nil
}

// kubeconfigPath resolves the kubeconfig the way NewClients documents. It
// returns "" when there is none, which leaves the in-cluster service account.
func kubeconfigPath(kubeconfig string) string {
	path := kubeconfig
	if path == "" {
		path = os.Getenv("WASSUP_KUBECONFIG")
	}
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, ".kube", "config")
			if _, err := os.Stat(p); err == nil {
				path = p
			}
		}
	}
	return path
}

// restConfig builds the configuration of the clients for a kubeconfig and a
// context.
func restConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	path := kubeconfigPath(kubeconfig)
	if path == "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("no kubeconfig found and not running in a cluster: %w", err)
		}
		return cfg, nil
	}
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: filepath.SplitList(path)}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	return cfg, nil
}

// clientCache shares one Clients per (kubeconfig, context). It is the only
// package-level mutable state besides the probe registry.
var clientCache = struct {
	sync.Mutex
	m map[string]*Clients
}{m: map[string]*Clients{}}

// clientsFor returns the shared Clients for a kubeconfig and context,
// building them on first use.
func clientsFor(kubeconfig, kubeContext string) (*Clients, error) {
	key := kubeconfig + "\x00" + kubeContext
	clientCache.Lock()
	defer clientCache.Unlock()
	if c, ok := clientCache.m[key]; ok {
		return c, nil
	}
	c, err := NewClients(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	clientCache.m[key] = c
	return c, nil
}

// proxy fetches a raw API server path.
func (c *Clients) proxy(ctx context.Context, path string) ([]byte, error) {
	if c.proxyGet != nil {
		return c.proxyGet(ctx, path)
	}
	if c.Core == nil {
		return nil, fmt.Errorf("no core client")
	}
	rc, ok := c.Core.CoreV1().RESTClient().(*rest.RESTClient)
	if !ok || rc == nil {
		return nil, fmt.Errorf("no REST client for the API server proxy")
	}
	return rc.Get().AbsPath(path).DoRaw(ctx)
}

// indexPodNode is the pod informer index by spec.nodeName.
const indexPodNode = "node"

// informerSet is the shared informer factory of one Clients. The factory is
// started lazily and lives as long as the process: probes come and go but
// the caches stay warm. Resync is left to the probe tick.
type informerSet struct {
	factory informers.SharedInformerFactory
	stop    chan struct{}
	pods    cache.SharedIndexInformer
}

// informers returns the shared informer set, creating it on first use.
func (c *Clients) informers() *informerSet {
	c.infOnce.Do(func() {
		f := informers.NewSharedInformerFactory(c.Core, 0)
		s := &informerSet{factory: f, stop: make(chan struct{})}
		s.pods = f.Core().V1().Pods().Informer()
		// Indexers must be added before the informer starts; the pod informer
		// is created eagerly for that reason.
		_ = s.pods.AddIndexers(cache.Indexers{indexPodNode: func(obj any) ([]string, error) {
			p, ok := obj.(*corev1.Pod)
			if !ok || p.Spec.NodeName == "" {
				return nil, nil
			}
			return []string{p.Spec.NodeName}, nil
		}})
		c.inf = s
	})
	return c.inf
}

// sync starts every informer requested so far and waits until the given ones
// have filled their stores, or until ctx is done or the timeout passes.
func (s *informerSet) sync(ctx context.Context, timeout time.Duration, infs ...cache.SharedIndexInformer) error {
	s.factory.Start(s.stop)
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	synced := make([]cache.InformerSynced, 0, len(infs))
	for _, inf := range infs {
		synced = append(synced, inf.HasSynced)
	}
	if !cache.WaitForCacheSync(wctx.Done(), synced...) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("informer caches did not sync within %s", timeout)
	}
	return nil
}
