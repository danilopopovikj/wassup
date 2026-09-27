package k8s

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

const kindNode = "k8s.node"

func init() {
	probe.Register(probe.Access{
		Kind:        kindNode,
		Source:      "Kubernetes API (node, pod and event informers), metrics.k8s.io NodeMetrics and the kubelet /stats/summary through the API server proxy",
		Delivers:    "cpu_pct, mem_pct, disk_pct, pods, killed; NotReady, MemoryPressure, DiskPressure, Rebooted; node (reboot) events",
		SpecFields:  []string{"name (required)", "kubeconfig", "context"},
		Needs:       "get/list/watch on nodes, pods and events; get on nodes.metrics.k8s.io; get on nodes/proxy for disk usage (optional)",
		Implemented: true,
	}, func() probe.Probe { return &nodeProbe{base: base{kind: kindNode}} })
}

// rebootWindow is how far back a boot counts as a recent reboot.
const rebootWindow = 2 * time.Hour

// nodeProbe is k8s.node.
type nodeProbe struct {
	base
	lastBootID string
	rebootAt   time.Time
	emitted    map[string]bool
}

// Kind implements probe.Probe.
func (n *nodeProbe) Kind() string { return kindNode }

// Validate implements probe.Probe.
func (n *nodeProbe) Validate(spec map[string]any) error {
	return probe.RequireString(spec, "name")
}

// Start implements probe.Probe.
func (n *nodeProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := n.Validate(spec); err != nil {
		n.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c, err := n.connect(spec)
	if err != nil {
		return err
	}
	name := probe.Str(spec, "name", "")
	target := targetOf(spec, name)
	n.emitted = map[string]bool{}

	inf := c.informers()
	f := inf.factory
	nodeInf := f.Core().V1().Nodes().Informer()
	evInf := f.Core().V1().Events().Informer()
	nodes := f.Core().V1().Nodes().Lister()
	events := f.Core().V1().Events().Lister()

	kick, notify := kicker()
	watch(ctx, nodeInf, func(obj any) bool {
		nd, ok := obj.(*corev1.Node)
		return ok && nd.Name == name
	}, notify)

	go func() {
		if err := n.syncOrFail(ctx, c, nodeInf, inf.pods, evInf); err != nil {
			probe.Send(ctx, out, probe.Observation{Target: target, Probe: kindNode, At: time.Now(), Err: err.Error()})
			return
		}
		runLoop(ctx, tickOf(spec), kick, func() {
			o, err := n.observe(ctx, c, nodes, inf.pods, events, name, target)
			if err != nil {
				n.fail(ctx, out, target, err)
				return
			}
			n.emit(ctx, out, o)
		})
	}()
	return nil
}

// observe reads the node once.
func (n *nodeProbe) observe(ctx context.Context, c *Clients, nodes corelisters.NodeLister, podInf cache.SharedIndexInformer, events corelisters.EventLister, name, target string) (probe.Observation, error) {
	now := time.Now()
	node, err := nodes.Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return probe.Observation{}, fmt.Errorf("node %s not found", name)
		}
		return probe.Observation{}, err
	}
	o := probe.Observation{Target: target, At: now, Metrics: map[string]float64{}, Detail: map[string]any{}}
	ref := "node/" + node.Name

	// Conditions.
	condStates := map[string]any{}
	for _, cnd := range node.Status.Conditions {
		condStates[string(cnd.Type)] = string(cnd.Status)
		switch cnd.Type {
		case corev1.NodeReady:
			if cnd.Status != corev1.ConditionTrue {
				o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondNotReady, Ref: ref, Since: cnd.LastTransitionTime.Time, Detail: firstNonEmpty(cnd.Message, cnd.Reason)})
			}
		case corev1.NodeMemoryPressure:
			if cnd.Status == corev1.ConditionTrue {
				o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondMemoryPressure, Ref: ref, Since: cnd.LastTransitionTime.Time, Detail: firstNonEmpty(cnd.Message, cnd.Reason)})
			}
		case corev1.NodeDiskPressure:
			if cnd.Status == corev1.ConditionTrue {
				o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondDiskPressure, Ref: ref, Since: cnd.LastTransitionTime.Time, Detail: firstNonEmpty(cnd.Message, cnd.Reason)})
			}
		}
	}
	if len(node.Status.Conditions) == 0 {
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondNotReady, Ref: ref, Since: node.CreationTimestamp.Time, Detail: "node has not reported any condition"})
	}

	// Pods on the node, and kills among them.
	var pods []*corev1.Pod
	if objs, err := podInf.GetIndexer().ByIndex(indexPodNode, node.Name); err == nil {
		for _, obj := range objs {
			if p, ok := obj.(*corev1.Pod); ok {
				pods = append(pods, p)
			}
		}
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	running := 0
	var kills []map[string]any
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodPending {
			running++
		}
		for _, cs := range allContainerStatuses(p) {
			term := cs.LastTerminationState.Terminated
			if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
				term = t
			}
			if term != nil && term.Reason == "OOMKilled" && within(term.FinishedAt.Time, restartWindow, now) {
				k := map[string]any{"pod": p.Name, "container": cs.Name, "at": term.FinishedAt.Time}
				if l := memLimit(containerSpec(p, cs.Name)); l != "" {
					k["limit"] = l
				}
				kills = append(kills, k)
			}
		}
	}
	o.Metrics["pods"] = float64(running)
	if len(kills) > 0 {
		o.Metrics["killed"] = float64(len(kills))
		o.Detail["kills"] = kills
	}

	// Usage against allocatable.
	if c.Metrics != nil {
		if nm, err := c.Metrics.MetricsV1beta1().NodeMetricses().Get(ctx, node.Name, metav1.GetOptions{}); err == nil {
			if alloc := node.Status.Allocatable.Cpu(); alloc != nil && alloc.MilliValue() > 0 {
				o.Metrics["cpu_pct"] = pct(float64(nm.Usage.Cpu().MilliValue()), float64(alloc.MilliValue()))
			}
			if alloc := node.Status.Allocatable.Memory(); alloc != nil && alloc.Value() > 0 {
				o.Metrics["mem_pct"] = pct(float64(nm.Usage.Memory().Value()), float64(alloc.Value()))
			}
		} else {
			o.Detail["metrics_error"] = err.Error()
		}
	}

	// Reboot detection: boot id change, kubelet Rebooted event, or a recent
	// boot reported by the stats summary.
	bootID := node.Status.NodeInfo.BootID
	if n.lastBootID != "" && bootID != "" && bootID != n.lastBootID {
		n.rebootAt = now
	}
	n.lastBootID = bootID
	if events != nil {
		if list, err := events.List(labels.Everything()); err == nil {
			for _, e := range list {
				if e.InvolvedObject.Kind == "Node" && e.InvolvedObject.Name == node.Name && e.Reason == "Rebooted" {
					if at := eventTime(e); within(at, rebootWindow, now) && at.After(n.rebootAt) {
						n.rebootAt = at
					}
				}
			}
		}
	}
	if s, err := nodeStats(ctx, c, node.Name); err == nil {
		if p, ok := s.Node.Fs.pctOf(); ok {
			o.Metrics["disk_pct"] = p
		}
		if within(s.Node.StartTime, rebootWindow, now) && s.Node.StartTime.After(n.rebootAt) {
			n.rebootAt = s.Node.StartTime
		}
	} else {
		o.Detail["stats_error"] = err.Error()
	}
	if within(n.rebootAt, rebootWindow, now) {
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondRebooted, Ref: ref, Since: n.rebootAt, Detail: "boot id " + bootID})
		key := n.rebootAt.Truncate(time.Minute).Format(time.RFC3339)
		if !n.emitted[key] {
			n.emitted[key] = true
			o.Events = append(o.Events, model.Event{At: n.rebootAt, Kind: "node", Target: target, Summary: "reboot of " + target, Ref: bootID})
		}
	}

	o.Detail["kubelet_version"] = node.Status.NodeInfo.KubeletVersion
	o.Detail["os_image"] = node.Status.NodeInfo.OSImage
	o.Detail["kernel"] = node.Status.NodeInfo.KernelVersion
	o.Detail["boot_id"] = bootID
	o.Detail["allocatable"] = quantityMap(node.Status.Allocatable)
	o.Detail["capacity"] = quantityMap(node.Status.Capacity)
	o.Detail["conditions"] = condStates
	o.Detail["unschedulable"] = node.Spec.Unschedulable
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			o.Detail["internal_ip"] = a.Address
		}
	}
	if len(node.Spec.Taints) > 0 {
		taints := make([]string, 0, len(node.Spec.Taints))
		for _, t := range node.Spec.Taints {
			taints = append(taints, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
		}
		o.Detail["taints"] = taints
	}
	return o, nil
}
