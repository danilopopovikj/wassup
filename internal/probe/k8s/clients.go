package k8s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
)

// syncTimeout bounds the wait for the informer caches to fill.
const syncTimeout = 30 * time.Second

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
	cfg.UserAgent = "wassup"
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

// restConfig resolves the kubeconfig the way NewClients documents.
func restConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
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
