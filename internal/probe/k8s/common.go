package k8s

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/danilopopovikj/wassup/internal/probe"
)

const (
	defaultTick = 5 * time.Second
	// debounce delays the observation triggered by informer events so a burst
	// of pod updates produces one re-read.
	debounce = 400 * time.Millisecond
	// restartWindow is the window over which restarts and kills are counted.
	restartWindow = 5 * time.Minute
	// lookback is the window for change markers derived on the first tick.
	lookback = 2 * time.Hour
)

// base holds what every probe of this package shares.
type base struct {
	kind    string
	h       probe.Health
	clients *Clients
}

// Health returns the probe's own health.
func (b *base) Health() probe.ProbeHealth { return b.h.Get() }

// connect returns the shared clients for the spec's kubeconfig and context,
// or the injected ones.
func (b *base) connect(spec map[string]any) (*Clients, error) {
	if b.clients != nil {
		return b.clients, nil
	}
	c, err := clientsFor(probe.Str(spec, "kubeconfig", ""), probe.Str(spec, "context", ""))
	if err != nil {
		b.h.Set(probe.HealthFailed, err.Error())
		return nil, err
	}
	b.clients = c
	return c, nil
}

// await waits until the API server answers and the caches of the informers
// are filled. A server that does not answer is reported at once, as an
// observation with the reason, and asked again until it does: a cache that
// never fills says nothing for half a minute, which looks like a hang, and
// a cluster that was away for a moment must not end the probe. It returns
// an error only when ctx is done.
func (b *base) await(ctx context.Context, c *Clients, out chan<- probe.Observation, target string, infs ...cache.SharedIndexInformer) error {
	for {
		err := c.reach(ctx)
		if err == nil {
			if err = c.informers().sync(ctx, syncTimeout, infs...); err == nil {
				return nil
			}
			err = fmt.Errorf("%w: the account may not be allowed to list and watch what %s reads; `wassup access` prints the role it needs", err, b.kind)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.fail(ctx, out, target, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reachRetry):
		}
	}
}

// fail sends an error observation and records degraded health.
func (b *base) fail(ctx context.Context, out chan<- probe.Observation, target string, err error) {
	b.h.Set(probe.HealthDegraded, err.Error())
	probe.Send(ctx, out, probe.Observation{Target: target, Probe: b.kind, At: time.Now(), Err: err.Error()})
}

// emit sends a healthy observation.
func (b *base) emit(ctx context.Context, out chan<- probe.Observation, o probe.Observation) {
	o.Probe = b.kind
	if o.At.IsZero() {
		o.At = time.Now()
	}
	b.h.Set(probe.HealthOK, "")
	probe.Send(ctx, out, o)
}

// tickOf reads the runtime-injected tick.
func tickOf(spec map[string]any) time.Duration {
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		return d
	}
	return defaultTick
}

// targetOf reads the runtime-injected element id, falling back to def.
func targetOf(spec map[string]any, def string) string {
	return probe.Str(spec, "_target", def)
}

// kicker returns a channel informer handlers signal on and the non-blocking
// notify function they call.
func kicker() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	return ch, func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// runLoop calls fn immediately, then every tick, and (debounced) whenever
// kick is signalled, until ctx is done.
func runLoop(ctx context.Context, tick time.Duration, kick <-chan struct{}, fn func()) {
	if tick <= 0 {
		tick = defaultTick
	}
	fn()
	t := time.NewTicker(tick)
	defer t.Stop()
	var pending <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		case <-kick:
			if pending == nil {
				pending = time.After(debounce)
			}
		case <-pending:
			pending = nil
			fn()
		}
	}
}

// watch registers handler on the informer for ctx's lifetime; every add,
// update and delete of an object accepted by match signals notify.
func watch(ctx context.Context, inf cache.SharedIndexInformer, match func(obj any) bool, notify func()) {
	reg, err := inf.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj any) bool {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			return match(obj)
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { notify() },
			UpdateFunc: func(any, any) { notify() },
			DeleteFunc: func(any) { notify() },
		},
	})
	if err != nil {
		return
	}
	go func() {
		<-ctx.Done()
		_ = inf.RemoveEventHandler(reg)
	}()
}

// pct returns used/total as a percentage rounded to one decimal.
func pct(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(used/total*1000) / 10
}

// round1 rounds to one decimal.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// podReady reports whether the pod's Ready condition is true.
func podReady(p *corev1.Pod) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podStart returns the pod's start time, or its creation time.
func podStart(p *corev1.Pod) time.Time {
	if p.Status.StartTime != nil && !p.Status.StartTime.IsZero() {
		return p.Status.StartTime.Time
	}
	return p.CreationTimestamp.Time
}

// allContainerStatuses returns init, regular and ephemeral container statuses.
func allContainerStatuses(p *corev1.Pod) []corev1.ContainerStatus {
	out := make([]corev1.ContainerStatus, 0, len(p.Status.ContainerStatuses)+len(p.Status.InitContainerStatuses))
	out = append(out, p.Status.InitContainerStatuses...)
	out = append(out, p.Status.ContainerStatuses...)
	return out
}

// containerSpec finds a container by name among the pod's containers.
func containerSpec(p *corev1.Pod, name string) *corev1.Container {
	for i := range p.Spec.InitContainers {
		if p.Spec.InitContainers[i].Name == name {
			return &p.Spec.InitContainers[i]
		}
	}
	for i := range p.Spec.Containers {
		if p.Spec.Containers[i].Name == name {
			return &p.Spec.Containers[i]
		}
	}
	return nil
}

// memLimit formats a container's memory limit, or "".
func memLimit(c *corev1.Container) string {
	if c == nil {
		return ""
	}
	if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		return q.String()
	}
	return ""
}

// usage is the metric basis of a pod set: usage summed over containers and
// the limits (or requests) they run against.
type usage struct {
	cpuUsed, cpuBasis float64
	memUsed, memBasis float64
}

// podUsage computes cpu_pct and mem_pct for pods against their limits when
// any container sets one, else against requests. Containers without the
// chosen basis are left out of both sides. The booleans say whether a
// percentage could be computed at all.
func podUsage(pods []*corev1.Pod, metrics map[string]*metricsv1beta1.PodMetrics) (cpu, mem float64, haveCPU, haveMem bool) {
	basis := func(res corev1.ResourceName) func(c *corev1.Container) (float64, bool) {
		anyLimit := false
		for _, p := range pods {
			for _, c := range p.Spec.Containers {
				if _, ok := c.Resources.Limits[res]; ok {
					anyLimit = true
				}
			}
		}
		return func(c *corev1.Container) (float64, bool) {
			var q resource.Quantity
			var ok bool
			if anyLimit {
				q, ok = c.Resources.Limits[res]
			} else {
				q, ok = c.Resources.Requests[res]
			}
			if !ok {
				return 0, false
			}
			if res == corev1.ResourceCPU {
				return float64(q.MilliValue()), true
			}
			return float64(q.Value()), true
		}
	}
	cpuBasis := basis(corev1.ResourceCPU)
	memBasis := basis(corev1.ResourceMemory)
	var u usage
	for _, p := range pods {
		pm := metrics[p.Name]
		if pm == nil {
			continue
		}
		for i := range p.Spec.Containers {
			c := &p.Spec.Containers[i]
			var cm *metricsv1beta1.ContainerMetrics
			for j := range pm.Containers {
				if pm.Containers[j].Name == c.Name {
					cm = &pm.Containers[j]
				}
			}
			if cm == nil {
				continue
			}
			if b, ok := cpuBasis(c); ok && b > 0 {
				u.cpuBasis += b
				u.cpuUsed += float64(cm.Usage.Cpu().MilliValue())
			}
			if b, ok := memBasis(c); ok && b > 0 {
				u.memBasis += b
				u.memUsed += float64(cm.Usage.Memory().Value())
			}
		}
	}
	if u.cpuBasis > 0 {
		cpu, haveCPU = pct(u.cpuUsed, u.cpuBasis), true
	}
	if u.memBasis > 0 {
		mem, haveMem = pct(u.memUsed, u.memBasis), true
	}
	return cpu, mem, haveCPU, haveMem
}

// podMetricsByName lists PodMetrics in a namespace keyed by pod name. A
// missing metrics API is reported as an error the caller may ignore.
func podMetricsByName(ctx context.Context, c *Clients, namespace string) (map[string]*metricsv1beta1.PodMetrics, error) {
	if c.Metrics == nil {
		return nil, fmt.Errorf("no metrics client")
	}
	list, err := c.Metrics.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]*metricsv1beta1.PodMetrics, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out, nil
}

// imageTag returns the tag (or short digest) of a container image reference.
func imageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		digest := image[i+1:]
		if j := strings.Index(digest, ":"); j >= 0 {
			digest = digest[j+1:]
		}
		if len(digest) > 12 {
			digest = digest[:12]
		}
		return digest
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[colon+1:]
	}
	return "latest"
}

// sortedKeys returns the keys of a map in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// quantityMap renders a ResourceList as strings for Detail.
func quantityMap(rl corev1.ResourceList) map[string]any {
	out := make(map[string]any, len(rl))
	for k, v := range rl {
		out[string(k)] = v.String()
	}
	return out
}

// within reports whether t is non-zero and at most d before now.
func within(t time.Time, d time.Duration, now time.Time) bool {
	return !t.IsZero() && now.Sub(t) <= d && !t.After(now.Add(time.Minute))
}

// firstNonZero returns the first non-zero time.
func firstNonZero(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// earliest returns the earlier of two times, ignoring zero values.
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Before(a):
		return b
	}
	return a
}

// metaTime unwraps a *metav1.Time.
func metaTime(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}
