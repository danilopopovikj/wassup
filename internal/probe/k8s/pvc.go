package k8s

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

const kindPVC = "k8s.pvc"

func init() {
	probe.Register(probe.Access{
		Kind:        kindPVC,
		Source:      "Kubernetes API (persistentvolumeclaim and pod informers) and the kubelet /stats/summary of the node mounting the claim",
		Delivers:    "used_bytes, total_bytes, disk_pct (disk_pct only when the kubelet stats are readable)",
		SpecFields:  []string{"namespace (required)", "name (required)", "kubeconfig", "context"},
		Needs:       "get/list/watch on persistentvolumeclaims and pods in the namespace; get on nodes/proxy for usage (optional)",
		Implemented: true,
		Facets:      []string{facet.NameStorage},
		RBAC:        []probe.Rule{probe.Reads("", "persistentvolumeclaims", "pods")},
	}, func() probe.Probe { return &pvcProbe{base: base{kind: kindPVC}} })
}

// pvcProbe is k8s.pvc.
type pvcProbe struct {
	base
}

// Kind implements probe.Probe.
func (p *pvcProbe) Kind() string { return kindPVC }

// Validate implements probe.Probe.
func (p *pvcProbe) Validate(spec map[string]any) error {
	return probe.RequireString(spec, "namespace", "name")
}

// Start implements probe.Probe.
func (p *pvcProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := p.Validate(spec); err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c, err := p.connect(spec)
	if err != nil {
		return err
	}
	ns, name := probe.Str(spec, "namespace", ""), probe.Str(spec, "name", "")
	target := targetOf(spec, name)

	inf := c.informers()
	f := inf.factory
	pvcInf := f.Core().V1().PersistentVolumeClaims().Informer()
	pvcs := f.Core().V1().PersistentVolumeClaims().Lister()
	pods := f.Core().V1().Pods().Lister()

	kick, notify := kicker()
	watch(ctx, pvcInf, func(obj any) bool {
		claim, ok := obj.(*corev1.PersistentVolumeClaim)
		return ok && claim.Namespace == ns && claim.Name == name
	}, notify)

	go func() {
		if err := p.await(ctx, c, out, target, pvcInf, inf.pods); err != nil {
			return
		}
		runLoop(ctx, tickOf(spec), kick, func() {
			o, err := observePVC(ctx, c, pvcs, pods, ns, name, target)
			if err != nil {
				p.fail(ctx, out, target, err)
				return
			}
			p.emit(ctx, out, o)
		})
	}()
	return nil
}

// observePVC reads the claim and, when a pod mounts it, the kubelet's usage
// numbers for the volume.
func observePVC(ctx context.Context, c *Clients, pvcs corelisters.PersistentVolumeClaimLister, pods corelisters.PodLister, ns, name, target string) (probe.Observation, error) {
	now := time.Now()
	claim, err := pvcs.PersistentVolumeClaims(ns).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return probe.Observation{}, fmt.Errorf("pvc %s/%s not found", ns, name)
		}
		return probe.Observation{}, err
	}
	o := probe.Observation{Target: target, At: now, Metrics: map[string]float64{}, Detail: map[string]any{}}
	// The kubelet reports no IOPS, so the facet's IOPS stays unset.
	var vol facet.StorageFacet
	if q, ok := claim.Status.Capacity[corev1.ResourceStorage]; ok && q.Value() > 0 {
		vol.TotalBytes = facet.N(float64(q.Value()))
	} else if q, ok := claim.Spec.Resources.Requests[corev1.ResourceStorage]; ok && q.Value() > 0 {
		vol.TotalBytes = facet.N(float64(q.Value()))
	}

	// Pods mounting the claim, and the node the first running one is on.
	var mounting []string
	node, podName := "", ""
	if list, err := pods.Pods(ns).List(labels.Everything()); err == nil {
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		for _, pd := range list {
			for _, v := range pd.Spec.Volumes {
				if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim.Name {
					mounting = append(mounting, pd.Name)
					if node == "" && pd.Spec.NodeName != "" && pd.Status.Phase == corev1.PodRunning {
						node, podName = pd.Spec.NodeName, pd.Name
					}
				}
			}
		}
	}
	if node != "" {
		if s, err := nodeStats(ctx, c, node); err == nil {
			for _, ps := range s.Pods {
				if ps.PodRef.Namespace != ns || ps.PodRef.Name != podName {
					continue
				}
				for _, v := range ps.Volumes {
					if v.PVCRef == nil || v.PVCRef.Name != claim.Name {
						continue
					}
					if v.UsedBytes != nil {
						vol.UsedBytes = facet.N(float64(*v.UsedBytes))
					}
					if v.CapacityBytes != nil && *v.CapacityBytes > 0 {
						vol.TotalBytes = facet.N(float64(*v.CapacityBytes))
					}
					if v.AvailableBytes != nil {
						o.Detail["available_bytes"] = *v.AvailableBytes
					}
				}
			}
		} else {
			o.Detail["stats_error"] = err.Error()
		}
		o.Detail["node"] = node
	}
	// The facet derives disk_pct when both sizes are known.
	facet.EmitStorage(&o, vol, now)

	o.Detail["phase"] = string(claim.Status.Phase)
	if claim.Spec.StorageClassName != nil {
		o.Detail["storage_class"] = *claim.Spec.StorageClassName
	}
	modes := make([]string, 0, len(claim.Spec.AccessModes))
	for _, m := range claim.Spec.AccessModes {
		modes = append(modes, string(m))
	}
	o.Detail["access_modes"] = modes
	if claim.Spec.VolumeName != "" {
		o.Detail["volume"] = claim.Spec.VolumeName
	}
	o.Detail["pods"] = mounting
	return o, nil
}
