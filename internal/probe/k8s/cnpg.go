package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

const (
	kindCNPGCluster  = "cnpg.cluster"
	kindCNPGInstance = "cnpg.instance"

	// CloudNativePG pod labels.
	labelCNPGCluster = "cnpg.io/cluster"
	labelCNPGRole    = "cnpg.io/instanceRole"
	labelCNPGRoleOld = "role"
)

// CloudNativePG resources.
var (
	cnpgClusterGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	cnpgBackupGVR  = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}
)

func init() {
	probe.Register(probe.Access{
		Kind:        kindCNPGCluster,
		Source:      "CloudNativePG Cluster (postgresql.cnpg.io/v1) and Backup objects through the dynamic client, plus the pod informer",
		Delivers:    "replicas_ready, replicas_desired; ReplicationBroken, Backup, Switchover; switchover events",
		SpecFields:  []string{"namespace (required)", "cluster (required)", "kubeconfig", "context"},
		Needs:       "get/list on clusters.postgresql.cnpg.io and backups.postgresql.cnpg.io; list/watch on pods in the namespace",
		Implemented: true,
		Facets:      []string{facet.NameDatabase, facet.NameReplication},
	}, func() probe.Probe { return &cnpgClusterProbe{base: base{kind: kindCNPGCluster}} })
	probe.Register(probe.Access{
		Kind:        kindCNPGInstance,
		Source:      "the instance pod of a CloudNativePG cluster (pod informer) and metrics.k8s.io PodMetrics",
		Delivers:    "cpu_pct, mem_pct; NotReady",
		SpecFields:  []string{"namespace (required)", "cluster (required)", "role (primary|replica) or instance (pod name)", "kubeconfig", "context"},
		Needs:       "list/watch on pods in the namespace; get/list on pods.metrics.k8s.io",
		Implemented: true,
		Facets:      []string{facet.NameDatabase, facet.NameWorkload},
	}, func() probe.Probe { return &cnpgInstanceProbe{base: base{kind: kindCNPGInstance}} })
}

// cnpgClusterProbe is cnpg.cluster.
type cnpgClusterProbe struct {
	base
	lastPrimary string
	emitted     map[string]bool
}

// Kind implements probe.Probe.
func (p *cnpgClusterProbe) Kind() string { return kindCNPGCluster }

// Validate implements probe.Probe.
func (p *cnpgClusterProbe) Validate(spec map[string]any) error {
	return probe.RequireString(spec, "namespace", "cluster")
}

// Start implements probe.Probe.
func (p *cnpgClusterProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := p.Validate(spec); err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c, err := p.connect(spec)
	if err != nil {
		return err
	}
	ns, cluster := probe.Str(spec, "namespace", ""), probe.Str(spec, "cluster", "")
	target := targetOf(spec, cluster)
	p.emitted = map[string]bool{}

	inf := c.informers()
	pods := inf.factory.Core().V1().Pods().Lister()
	kick, notify := kicker()
	watch(ctx, inf.pods, func(obj any) bool {
		pd, ok := obj.(*corev1.Pod)
		return ok && pd.Namespace == ns && pd.Labels[labelCNPGCluster] == cluster
	}, notify)

	go func() {
		if err := p.syncOrFail(ctx, c, inf.pods); err != nil {
			probe.Send(ctx, out, probe.Observation{Target: target, Probe: kindCNPGCluster, At: time.Now(), Err: err.Error()})
			return
		}
		runLoop(ctx, tickOf(spec), kick, func() {
			o, err := p.observe(ctx, c, pods, ns, cluster, target)
			if err != nil {
				p.fail(ctx, out, target, err)
				return
			}
			p.emit(ctx, out, o)
		})
	}()
	return nil
}

// observe reads the Cluster object, its backups and instance pods once.
func (p *cnpgClusterProbe) observe(ctx context.Context, c *Clients, pods corelisters.PodLister, ns, cluster, target string) (probe.Observation, error) {
	now := time.Now()
	if c.Dynamic == nil {
		return probe.Observation{}, fmt.Errorf("no dynamic client")
	}
	obj, err := c.Dynamic.Resource(cnpgClusterGVR).Namespace(ns).Get(ctx, cluster, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return probe.Observation{}, fmt.Errorf("cnpg cluster %s/%s not found", ns, cluster)
		}
		if isNoMatch(err) {
			p.h.Set(probe.HealthFailed, "CloudNativePG is not installed: "+err.Error())
		}
		return probe.Observation{}, err
	}
	o := probe.Observation{Target: target, At: now, Metrics: map[string]float64{}, Detail: map[string]any{}}

	desired, _, _ := unstructured.NestedInt64(obj.Object, "spec", "instances")
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyInstances")
	instances, _, _ := unstructured.NestedInt64(obj.Object, "status", "instances")
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	phaseReason, _, _ := unstructured.NestedString(obj.Object, "status", "phaseReason")
	primary, _, _ := unstructured.NestedString(obj.Object, "status", "currentPrimary")
	targetPrimary, _, _ := unstructured.NestedString(obj.Object, "status", "targetPrimary")
	primaryTS, _, _ := unstructured.NestedString(obj.Object, "status", "currentPrimaryTimestamp")
	// The cluster's replica counts take the workload shape.
	facet.EmitWorkload(&o, facet.WorkloadFacet{ReplicasReady: facet.N(float64(ready)), ReplicasDesired: facet.N(float64(desired)), Ready: true}, now)

	// Instance health from status.instancesStatus and the pods: every
	// unhealthy instance is a broken replication stream, named by the
	// instance as its slot. The operator does not say since when.
	statusMap, _, _ := unstructured.NestedMap(obj.Object, "status", "instancesStatus")
	instStatus := map[string]any{}
	for k, v := range statusMap {
		names := toStringSlice(v)
		instStatus[k] = names
		if k == "healthy" {
			continue
		}
		for _, n := range names {
			facet.EmitReplication(&o, facet.ReplicationFacet{Slot: n, Detail: "instance " + k}, now)
		}
	}
	var instPods []*corev1.Pod
	if list, err := pods.Pods(ns).List(labels.SelectorFromSet(labels.Set{labelCNPGCluster: cluster})); err == nil {
		instPods = list
	}
	sort.Slice(instPods, func(i, j int) bool { return instPods[i].Name < instPods[j].Name })
	var podNames []string
	for _, pd := range instPods {
		podNames = append(podNames, pd.Name)
		if pd.Name == primary || podReady(pd) {
			continue
		}
		since := podStart(pd)
		for _, cnd := range pd.Status.Conditions {
			if cnd.Type == corev1.PodReady {
				since = firstNonZero(cnd.LastTransitionTime.Time, since)
			}
		}
		facet.EmitReplication(&o, facet.ReplicationFacet{Slot: pd.Name, BrokenSince: since, Detail: "replica pod not ready"}, now)
	}
	if targetPrimary != "" && primary != "" && targetPrimary != primary {
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondSwitchover, Ref: "pod/" + targetPrimary, Since: now, Detail: fmt.Sprintf("%s -> %s", primary, targetPrimary)})
	} else if strings.Contains(strings.ToLower(phase), "switchover") || strings.Contains(strings.ToLower(phase), "failover") {
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondSwitchover, Ref: "cluster/" + cluster, Since: now, Detail: phase})
	}
	if p.lastPrimary != "" && primary != "" && primary != p.lastPrimary {
		at := now
		if t, err := time.Parse(time.RFC3339Nano, primaryTS); err == nil {
			at = t
		}
		p.addEvent(&o, model.Event{At: at, Kind: "switchover", Target: target, Summary: "switchover to " + primary, Ref: primary})
	} else if p.lastPrimary == "" && primary != "" {
		if t, err := time.Parse(time.RFC3339Nano, primaryTS); err == nil && within(t, lookback, now) {
			p.addEvent(&o, model.Event{At: t, Kind: "switchover", Target: target, Summary: "switchover to " + primary, Ref: primary})
		}
	}
	if primary != "" {
		p.lastPrimary = primary
	}

	// Backups in flight: the first one found is the database's Backup task.
	db := facet.DatabaseFacet{Ready: true}
	if backups, err := c.Dynamic.Resource(cnpgBackupGVR).Namespace(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range backups.Items {
			b := &backups.Items[i]
			owner, _, _ := unstructured.NestedString(b.Object, "spec", "cluster", "name")
			if owner != cluster {
				continue
			}
			bphase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
			switch strings.ToLower(bphase) {
			case "started", "running", "pending", "":
				since := b.GetCreationTimestamp().Time
				if s, ok, _ := unstructured.NestedString(b.Object, "status", "startedAt"); ok {
					if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
						since = t
					}
				}
				if db.Backup == nil {
					db.Backup = &facet.Task{ID: "backup/" + b.GetName(), Name: b.GetName(), Started: since}
				}
			}
		}
	}
	facet.EmitDatabase(&o, db, now)
	if s, ok, _ := unstructured.NestedString(obj.Object, "status", "firstRecoverabilityPoint"); ok && s != "" {
		o.Detail["first_recoverability_point"] = s
	}
	if s, ok, _ := unstructured.NestedString(obj.Object, "status", "lastSuccessfulBackup"); ok && s != "" {
		o.Detail["last_successful_backup"] = s
	}
	if s, ok, _ := unstructured.NestedString(obj.Object, "status", "lastFailedBackup"); ok && s != "" {
		o.Detail["last_failed_backup"] = s
	}

	o.Conditions = dedupeConditions(o.Conditions)
	o.Detail["phase"] = phase
	if phaseReason != "" {
		o.Detail["phase_reason"] = phaseReason
	}
	o.Detail["primary"] = primary
	o.Detail["target_primary"] = targetPrimary
	o.Detail["instances"] = instances
	o.Detail["instances_status"] = instStatus
	o.Detail["pods"] = podNames
	if img, ok, _ := unstructured.NestedString(obj.Object, "spec", "imageName"); ok && img != "" {
		o.Detail["image"] = img
	}
	return o, nil
}

// addEvent appends an event once per (summary, minute).
func (p *cnpgClusterProbe) addEvent(o *probe.Observation, e model.Event) {
	key := e.Summary + "|" + e.At.Truncate(time.Minute).Format(time.RFC3339)
	if p.emitted[key] {
		return
	}
	p.emitted[key] = true
	o.Events = append(o.Events, e)
}

// toStringSlice converts an unstructured list to strings.
func toStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// cnpgInstanceProbe is cnpg.instance.
type cnpgInstanceProbe struct {
	base
}

// Kind implements probe.Probe.
func (p *cnpgInstanceProbe) Kind() string { return kindCNPGInstance }

// Validate implements probe.Probe.
func (p *cnpgInstanceProbe) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "namespace", "cluster"); err != nil {
		return err
	}
	role, instance := probe.Str(spec, "role", ""), probe.Str(spec, "instance", "")
	if role == "" && instance == "" {
		return fmt.Errorf("\"role\" (primary|replica) or \"instance\" is required")
	}
	if role != "" && role != "primary" && role != "replica" {
		return fmt.Errorf("\"role\" must be primary or replica, got %q", role)
	}
	return nil
}

// Start implements probe.Probe.
func (p *cnpgInstanceProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := p.Validate(spec); err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c, err := p.connect(spec)
	if err != nil {
		return err
	}
	ns, cluster := probe.Str(spec, "namespace", ""), probe.Str(spec, "cluster", "")
	role, instance := probe.Str(spec, "role", ""), probe.Str(spec, "instance", "")
	target := targetOf(spec, firstNonEmpty(instance, cluster+"-"+role))

	inf := c.informers()
	pods := inf.factory.Core().V1().Pods().Lister()
	kick, notify := kicker()
	watch(ctx, inf.pods, func(obj any) bool {
		pd, ok := obj.(*corev1.Pod)
		return ok && pd.Namespace == ns && pd.Labels[labelCNPGCluster] == cluster
	}, notify)

	go func() {
		if err := p.syncOrFail(ctx, c, inf.pods); err != nil {
			probe.Send(ctx, out, probe.Observation{Target: target, Probe: kindCNPGInstance, At: time.Now(), Err: err.Error()})
			return
		}
		runLoop(ctx, tickOf(spec), kick, func() {
			o, err := observeCNPGInstance(ctx, c, pods, ns, cluster, role, instance, target)
			if err != nil {
				p.fail(ctx, out, target, err)
				return
			}
			p.emit(ctx, out, o)
		})
	}()
	return nil
}

// findInstance picks the instance pod by name or by role.
func findInstance(pods corelisters.PodLister, ns, cluster, role, instance string) (*corev1.Pod, error) {
	if instance != "" {
		pd, err := pods.Pods(ns).Get(instance)
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, fmt.Errorf("instance pod %s/%s not found", ns, instance)
			}
			return nil, err
		}
		return pd, nil
	}
	list, err := pods.Pods(ns).List(labels.SelectorFromSet(labels.Set{labelCNPGCluster: cluster}))
	if err != nil {
		return nil, err
	}
	var matches []*corev1.Pod
	for _, pd := range list {
		if pd.Labels[labelCNPGRole] == role || pd.Labels[labelCNPGRoleOld] == role {
			matches = append(matches, pd)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no %s instance of cnpg cluster %s/%s", role, ns, cluster)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	return matches[0], nil
}

// observeCNPGInstance reads one instance pod and its metrics.
func observeCNPGInstance(ctx context.Context, c *Clients, pods corelisters.PodLister, ns, cluster, role, instance, target string) (probe.Observation, error) {
	now := time.Now()
	pd, err := findInstance(pods, ns, cluster, role, instance)
	if err != nil {
		return probe.Observation{}, err
	}
	o := probe.Observation{Target: target, At: now, Metrics: map[string]float64{}, Detail: map[string]any{}}
	// One instance pod is a workload of one; the facet writes cpu_pct,
	// mem_pct and NotReady.
	wl := facet.WorkloadFacet{Ready: true}
	if !podReady(pd) {
		since := podStart(pd)
		detail := string(pd.Status.Phase)
		for _, cnd := range pd.Status.Conditions {
			if cnd.Type == corev1.PodReady {
				since = firstNonZero(cnd.LastTransitionTime.Time, since)
				detail = firstNonEmpty(cnd.Message, cnd.Reason, detail)
			}
		}
		wl.Ready, wl.NotReadySince, wl.NotReadyDetail = false, since, firstNonEmpty(detail, "pod not ready")
	}
	if metrics, err := podMetricsByName(ctx, c, ns); err == nil {
		cpu, mem, haveCPU, haveMem := podUsage([]*corev1.Pod{pd}, metrics)
		if haveCPU {
			wl.CPUPct = facet.N(cpu)
		}
		if haveMem {
			wl.MemPct = facet.N(mem)
		}
	} else {
		o.Detail["metrics_error"] = err.Error()
	}
	facet.EmitWorkload(&o, wl, now)
	var restarts int32
	for _, cs := range pd.Status.ContainerStatuses {
		restarts += cs.RestartCount
	}
	o.Detail["pod"] = pd.Name
	o.Detail["role"] = firstNonEmpty(pd.Labels[labelCNPGRole], pd.Labels[labelCNPGRoleOld], role)
	o.Detail["node"] = pd.Spec.NodeName
	o.Detail["phase"] = string(pd.Status.Phase)
	o.Detail["restarts"] = restarts
	o.Detail["cluster"] = cluster
	if img := firstImage(pd.Spec.Containers); img != "" {
		o.Detail["image"] = img
	}
	return o, nil
}
