package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// Inventory is what Discover found in a cluster. It is the input of the
// topology generator and is JSON friendly.
type Inventory struct {
	Context      string         `json:"context,omitempty"`
	Namespaces   []string       `json:"namespaces"`
	Nodes        []NodeInfo     `json:"nodes"`
	Workloads    []WorkloadInfo `json:"workloads"`
	Services     []ServiceInfo  `json:"services"`
	Ingresses    []IngressInfo  `json:"ingresses"`
	PVCs         []PVCInfo      `json:"pvcs"`
	CNPGClusters []CNPGInfo     `json:"cnpg_clusters"`
	CronJobs     []CronJobInfo  `json:"cronjobs"`
	Warnings     []string       `json:"warnings,omitempty"`
}

// NodeInfo is one node.
type NodeInfo struct {
	Name       string            `json:"name"`
	Ready      bool              `json:"ready"`
	Roles      []string          `json:"roles,omitempty"`
	InternalIP string            `json:"internal_ip,omitempty"`
	Kubelet    string            `json:"kubelet,omitempty"`
	OS         string            `json:"os,omitempty"`
	CPU        string            `json:"cpu,omitempty"`
	Memory     string            `json:"memory,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// WorkloadInfo is one Deployment, StatefulSet or DaemonSet.
type WorkloadInfo struct {
	Namespace string   `json:"namespace"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Selector  string   `json:"selector"`
	Replicas  int32    `json:"replicas"`
	Ready     int32    `json:"ready"`
	Images    []string `json:"images"`
	Nodes     []string `json:"nodes,omitempty"`
}

// ServiceInfo is one Service.
type ServiceInfo struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Selector  string   `json:"selector,omitempty"`
	Ports     []string `json:"ports,omitempty"`
	ClusterIP string   `json:"cluster_ip,omitempty"`
}

// IngressInfo is one Ingress.
type IngressInfo struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	Class      string   `json:"class,omitempty"`
	Hosts      []string `json:"hosts"`
	TLSSecrets []string `json:"tls_secrets,omitempty"`
	Backends   []string `json:"backends,omitempty"`
}

// PVCInfo is one PersistentVolumeClaim.
type PVCInfo struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	Capacity     string `json:"capacity,omitempty"`
	StorageClass string `json:"storage_class,omitempty"`
}

// CNPGInfo is one CloudNativePG Cluster.
type CNPGInfo struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Instances int64    `json:"instances"`
	Ready     int64    `json:"ready"`
	Primary   string   `json:"primary,omitempty"`
	Phase     string   `json:"phase,omitempty"`
	Pods      []string `json:"pods,omitempty"`
}

// CronJobInfo is one CronJob.
type CronJobInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Cron      string `json:"cron"`
	Schedule  string `json:"schedule"`
	Suspended bool   `json:"suspended,omitempty"`
}

// Discover lists the cluster's nodes and, in the given namespaces (all of
// them when empty, minus kube-system), its workloads, services, ingresses,
// claims, cron jobs and CloudNativePG clusters. Missing optional APIs
// (metrics, CNPG) and forbidden lists become Warnings instead of errors.
func Discover(ctx context.Context, c *Clients, namespaces []string) (*Inventory, error) {
	if c == nil || c.Core == nil {
		return nil, fmt.Errorf("no kubernetes client")
	}
	inv := &Inventory{Context: c.Context}
	warn := func(what string, err error) {
		inv.Warnings = append(inv.Warnings, what+": "+err.Error())
	}
	core := c.Core.CoreV1()

	// Namespaces.
	explicit := len(namespaces) > 0
	if explicit {
		inv.Namespaces = append(inv.Namespaces, namespaces...)
	} else {
		list, err := core.Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list namespaces: %w", err)
		}
		for _, ns := range list.Items {
			if ns.Name == metav1.NamespaceSystem {
				continue
			}
			inv.Namespaces = append(inv.Namespaces, ns.Name)
		}
	}
	sort.Strings(inv.Namespaces)
	keep := map[string]bool{}
	for _, ns := range inv.Namespaces {
		keep[ns] = true
	}
	// Cluster-wide lists are cheaper than one list per namespace; the
	// filter drops what was not asked for.
	listNS := metav1.NamespaceAll
	if explicit && len(namespaces) == 1 {
		listNS = namespaces[0]
	}

	// Nodes.
	if nodes, err := core.Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		for _, n := range nodes.Items {
			ni := NodeInfo{Name: n.Name, Kubelet: n.Status.NodeInfo.KubeletVersion, OS: n.Status.NodeInfo.OSImage,
				CPU: n.Status.Allocatable.Cpu().String(), Memory: n.Status.Allocatable.Memory().String(), Labels: n.Labels}
			for _, cnd := range n.Status.Conditions {
				if cnd.Type == corev1.NodeReady {
					ni.Ready = cnd.Status == corev1.ConditionTrue
				}
			}
			for _, a := range n.Status.Addresses {
				if a.Type == corev1.NodeInternalIP {
					ni.InternalIP = a.Address
				}
			}
			for k := range n.Labels {
				if strings.HasPrefix(k, "node-role.kubernetes.io/") {
					ni.Roles = append(ni.Roles, strings.TrimPrefix(k, "node-role.kubernetes.io/"))
				}
			}
			sort.Strings(ni.Roles)
			inv.Nodes = append(inv.Nodes, ni)
		}
	} else {
		warn("nodes", err)
	}

	// Pods, for the node placement of workloads and CNPG instances.
	podsByNS := map[string][]corev1.Pod{}
	if pods, err := core.Pods(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range pods.Items {
			if keep[p.Namespace] {
				podsByNS[p.Namespace] = append(podsByNS[p.Namespace], p)
			}
		}
	} else {
		warn("pods", err)
	}
	nodesOf := func(ns string, sel *metav1.LabelSelector) []string {
		ls, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil || ls.Empty() {
			return nil
		}
		set := map[string]bool{}
		for _, p := range podsByNS[ns] {
			if p.Spec.NodeName != "" && ls.Matches(labels.Set(p.Labels)) {
				set[p.Spec.NodeName] = true
			}
		}
		return sortedKeys(set)
	}

	// Workloads.
	apps := c.Core.AppsV1()
	if deps, err := apps.Deployments(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, d := range deps.Items {
			if !keep[d.Namespace] {
				continue
			}
			replicas := int32(1)
			if d.Spec.Replicas != nil {
				replicas = *d.Spec.Replicas
			}
			inv.Workloads = append(inv.Workloads, WorkloadInfo{Namespace: d.Namespace, Kind: "Deployment", Name: d.Name, Selector: selectorString(d.Spec.Selector),
				Replicas: replicas, Ready: d.Status.ReadyReplicas, Images: images(d.Spec.Template), Nodes: nodesOf(d.Namespace, d.Spec.Selector)})
		}
	} else {
		warn("deployments", err)
	}
	if sts, err := apps.StatefulSets(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, s := range sts.Items {
			if !keep[s.Namespace] {
				continue
			}
			replicas := int32(1)
			if s.Spec.Replicas != nil {
				replicas = *s.Spec.Replicas
			}
			inv.Workloads = append(inv.Workloads, WorkloadInfo{Namespace: s.Namespace, Kind: "StatefulSet", Name: s.Name, Selector: selectorString(s.Spec.Selector),
				Replicas: replicas, Ready: s.Status.ReadyReplicas, Images: images(s.Spec.Template), Nodes: nodesOf(s.Namespace, s.Spec.Selector)})
		}
	} else {
		warn("statefulsets", err)
	}
	if dss, err := apps.DaemonSets(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, d := range dss.Items {
			if !keep[d.Namespace] {
				continue
			}
			inv.Workloads = append(inv.Workloads, WorkloadInfo{Namespace: d.Namespace, Kind: "DaemonSet", Name: d.Name, Selector: selectorString(d.Spec.Selector),
				Replicas: d.Status.DesiredNumberScheduled, Ready: d.Status.NumberReady, Images: images(d.Spec.Template), Nodes: nodesOf(d.Namespace, d.Spec.Selector)})
		}
	} else {
		warn("daemonsets", err)
	}
	sort.Slice(inv.Workloads, func(i, j int) bool {
		a, b := inv.Workloads[i], inv.Workloads[j]
		return a.Namespace+"/"+a.Kind+"/"+a.Name < b.Namespace+"/"+b.Kind+"/"+b.Name
	})

	// Services.
	if svcs, err := core.Services(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, s := range svcs.Items {
			if !keep[s.Namespace] {
				continue
			}
			si := ServiceInfo{Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type), Selector: labels.Set(s.Spec.Selector).String(), ClusterIP: s.Spec.ClusterIP}
			for _, p := range s.Spec.Ports {
				port := fmt.Sprintf("%d/%s", p.Port, p.Protocol)
				if p.Name != "" {
					port = p.Name + ":" + port
				}
				si.Ports = append(si.Ports, port)
			}
			inv.Services = append(inv.Services, si)
		}
	} else {
		warn("services", err)
	}

	// Ingresses.
	if ings, err := c.Core.NetworkingV1().Ingresses(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, ing := range ings.Items {
			if !keep[ing.Namespace] {
				continue
			}
			ii := IngressInfo{Namespace: ing.Namespace, Name: ing.Name}
			if ing.Spec.IngressClassName != nil {
				ii.Class = *ing.Spec.IngressClassName
			}
			hosts := map[string]bool{}
			for _, r := range ing.Spec.Rules {
				if r.Host != "" {
					hosts[r.Host] = true
				}
				if r.HTTP != nil {
					for _, p := range r.HTTP.Paths {
						ii.Backends = append(ii.Backends, backendString(p.Backend, p.Path))
					}
				}
			}
			for _, t := range ing.Spec.TLS {
				for _, h := range t.Hosts {
					hosts[h] = true
				}
				if t.SecretName != "" {
					ii.TLSSecrets = append(ii.TLSSecrets, t.SecretName)
				}
			}
			ii.Hosts = sortedKeys(hosts)
			inv.Ingresses = append(inv.Ingresses, ii)
		}
	} else {
		warn("ingresses", err)
	}

	// Claims.
	if pvcs, err := core.PersistentVolumeClaims(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range pvcs.Items {
			if !keep[p.Namespace] {
				continue
			}
			pi := PVCInfo{Namespace: p.Namespace, Name: p.Name, Phase: string(p.Status.Phase)}
			if q, ok := p.Status.Capacity[corev1.ResourceStorage]; ok {
				pi.Capacity = q.String()
			} else if q, ok := p.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				pi.Capacity = q.String()
			}
			if p.Spec.StorageClassName != nil {
				pi.StorageClass = *p.Spec.StorageClassName
			}
			inv.PVCs = append(inv.PVCs, pi)
		}
	} else {
		warn("persistentvolumeclaims", err)
	}

	// Cron jobs.
	if cjs, err := c.Core.BatchV1().CronJobs(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, cj := range cjs.Items {
			if !keep[cj.Namespace] {
				continue
			}
			inv.CronJobs = append(inv.CronJobs, CronJobInfo{Namespace: cj.Namespace, Name: cj.Name, Cron: cj.Spec.Schedule, Schedule: cronPhrase(cj.Spec.Schedule),
				Suspended: cj.Spec.Suspend != nil && *cj.Spec.Suspend})
		}
	} else {
		warn("cronjobs", err)
	}

	// CloudNativePG clusters, when the operator is installed.
	if c.Dynamic != nil {
		if list, err := c.Dynamic.Resource(cnpgClusterGVR).Namespace(listNS).List(ctx, metav1.ListOptions{}); err == nil {
			for i := range list.Items {
				cl := &list.Items[i]
				if !keep[cl.GetNamespace()] {
					continue
				}
				inv.CNPGClusters = append(inv.CNPGClusters, cnpgInfo(cl, podsByNS[cl.GetNamespace()]))
			}
		} else if !errors.IsNotFound(err) && !isNoMatch(err) {
			warn("cnpg clusters", err)
		}
	}
	return inv, nil
}

// cnpgInfo summarises one Cluster object.
func cnpgInfo(cl *unstructured.Unstructured, pods []corev1.Pod) CNPGInfo {
	info := CNPGInfo{Namespace: cl.GetNamespace(), Name: cl.GetName()}
	info.Instances, _, _ = unstructured.NestedInt64(cl.Object, "spec", "instances")
	info.Ready, _, _ = unstructured.NestedInt64(cl.Object, "status", "readyInstances")
	info.Primary, _, _ = unstructured.NestedString(cl.Object, "status", "currentPrimary")
	info.Phase, _, _ = unstructured.NestedString(cl.Object, "status", "phase")
	for _, p := range pods {
		if p.Labels[labelCNPGCluster] == cl.GetName() {
			info.Pods = append(info.Pods, p.Name)
		}
	}
	sort.Strings(info.Pods)
	return info
}

// images lists the distinct container images of a pod template.
func images(t corev1.PodTemplateSpec) []string {
	set := map[string]bool{}
	for _, c := range t.Spec.Containers {
		set[c.Image] = true
	}
	return sortedKeys(set)
}

// selectorString renders a label selector.
func selectorString(s *metav1.LabelSelector) string {
	if s == nil {
		return ""
	}
	ls, err := metav1.LabelSelectorAsSelector(s)
	if err != nil {
		return ""
	}
	return ls.String()
}
