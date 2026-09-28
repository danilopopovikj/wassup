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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/danilopopovikj/wassup/internal/discover/redact"
)

// Inventory is what Discover found in a cluster. It is the input of the
// topology generator and is JSON friendly. It is printed and written to
// disk, so it never holds the value of a credential: environment values and
// command lines pass through the redact package before they are stored.
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
	// NetworkPolicies holds the policies that say what pods may reach.
	NetworkPolicies []NetworkPolicyInfo `json:"network_policies,omitempty"`
	Warnings        []string            `json:"warnings,omitempty"`
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
	// Taints lists "key=value:effect"; a pod that does not tolerate them
	// cannot run on the node.
	Taints []string `json:"taints,omitempty"`
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
	// Env lists container environment variables by name, with the host they
	// point at or the secret/configmap key they come from. Variables loaded
	// from a ConfigMap (envFrom, configMapKeyRef) are listed one by one.
	Env []EnvRef `json:"env,omitempty"`
	// Command joins command and args per container, credentials masked.
	Command []string `json:"command,omitempty"`
	// Labels are the pod template's labels, completed with the object's own.
	Labels map[string]string `json:"labels,omitempty"`
	// Release is the Helm release that installed the workload, when Helm did.
	Release string `json:"release,omitempty"`
}

// EnvRef is one environment variable of a container.
type EnvRef struct {
	Container string `json:"container"`
	Name      string `json:"name"`
	// Value is what may be stored of the value: the scheme and host of a
	// URL, a host, a bucket, a region. It is empty for anything else and for
	// every variable whose name looks like a credential.
	Value     string `json:"value,omitempty"`
	SecretRef string `json:"secret_ref,omitempty"` // "<secret>/<key>"
	ConfigRef string `json:"config_ref,omitempty"` // "<configmap>/<key>"
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
	// Backends describes each rule for a reader: "service:port /path".
	Backends []string `json:"backends,omitempty"`
	// Services names the Services the rules route to, each once: several
	// paths to one Service are one flow.
	Services []string `json:"services,omitempty"`
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

// NetworkPolicyInfo is the egress side of one network policy: the pods it
// applies to and what they may reach. A list of allowed hosts is written by
// whoever runs the system, which makes it a better list of the outside
// services than the hosts found in code.
type NetworkPolicyInfo struct {
	Kind string `json:"kind"`
	// Namespace is empty for a policy that applies to the whole cluster.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Selector holds the labels of the pods the policy applies to, as
	// "a=b,c=d". Empty means every pod of the namespace.
	Selector string `json:"selector,omitempty"`
	// Expressions is set when the selector also holds expressions. They are
	// not evaluated, so the pods the policy applies to are not known.
	Expressions bool `json:"expressions,omitempty"`
	// Hosts are the names the pods may reach (toFQDNs matchName).
	Hosts []string `json:"hosts,omitempty"`
	// Patterns are the names with a wildcard (toFQDNs matchPattern).
	Patterns []string `json:"patterns,omitempty"`
	// CIDRs are the address ranges the pods may reach.
	CIDRs []string `json:"cidrs,omitempty"`
	// Endpoints are the selectors of the pods and namespaces they may reach.
	Endpoints []string `json:"endpoints,omitempty"`
	// Open is set when a rule allows every destination: the policy is then
	// no list of what is reached.
	Open bool `json:"open,omitempty"`
}

// The Cilium policies, when Cilium is installed.
var (
	ciliumPolicyGVR        = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
	ciliumClusterPolicyGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumclusterwidenetworkpolicies"}
)

// Discover lists the cluster's nodes and, in the given namespaces (all of
// them when empty, minus kube-system), its workloads, services, ingresses,
// claims, cron jobs, CloudNativePG clusters and the egress rules of its
// network policies. Missing optional APIs (metrics, CNPG, Cilium) are left
// out and forbidden lists become Warnings instead of errors.
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
			for _, t := range n.Spec.Taints {
				taint := t.Key
				if t.Value != "" {
					taint += "=" + t.Value
				}
				ni.Taints = append(ni.Taints, taint+":"+string(t.Effect))
			}
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

	// ConfigMaps a workload loads its environment from, read once each.
	// Secrets are never read: a secret reference keeps its name only.
	configMaps := map[string]map[string]string{}
	configMap := func(ns, name string) (map[string]string, bool) {
		key := ns + "/" + name
		if data, seen := configMaps[key]; seen {
			return data, data != nil
		}
		cm, err := core.ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			configMaps[key] = nil
			warn("configmap "+key, err)
			return nil, false
		}
		data := cm.Data
		if data == nil {
			data = map[string]string{}
		}
		configMaps[key] = data
		return data, true
	}
	workload := func(kind string, meta metav1.ObjectMeta, sel *metav1.LabelSelector, t corev1.PodTemplateSpec, replicas, ready int32) WorkloadInfo {
		return WorkloadInfo{Namespace: meta.Namespace, Kind: kind, Name: meta.Name, Selector: selectorString(sel),
			Replicas: replicas, Ready: ready, Images: images(t), Nodes: nodesOf(meta.Namespace, sel),
			Env: envRefs(meta.Namespace, t, configMap), Command: commands(t),
			Labels: mergeLabels(t.Labels, meta.Labels), Release: meta.Annotations["meta.helm.sh/release-name"]}
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
			inv.Workloads = append(inv.Workloads, workload("Deployment", d.ObjectMeta, d.Spec.Selector, d.Spec.Template, replicas, d.Status.ReadyReplicas))
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
			inv.Workloads = append(inv.Workloads, workload("StatefulSet", s.ObjectMeta, s.Spec.Selector, s.Spec.Template, replicas, s.Status.ReadyReplicas))
		}
	} else {
		warn("statefulsets", err)
	}
	if dss, err := apps.DaemonSets(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		for _, d := range dss.Items {
			if !keep[d.Namespace] {
				continue
			}
			inv.Workloads = append(inv.Workloads, workload("DaemonSet", d.ObjectMeta, d.Spec.Selector, d.Spec.Template, d.Status.DesiredNumberScheduled, d.Status.NumberReady))
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
			services := map[string]bool{}
			if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil {
				services[b.Service.Name] = true
			}
			for _, r := range ing.Spec.Rules {
				if r.Host != "" {
					hosts[r.Host] = true
				}
				if r.HTTP != nil {
					for _, p := range r.HTTP.Paths {
						ii.Backends = append(ii.Backends, backendString(p.Backend, p.Path))
						if p.Backend.Service != nil && p.Backend.Service.Name != "" {
							services[p.Backend.Service.Name] = true
						}
					}
				}
			}
			ii.Services = sortedKeys(services)
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

	// Network policies: what the pods may reach. Only the egress rules are
	// kept, and nothing is read but the policies themselves.
	policies := func(objects []map[string]any) {
		for _, obj := range objects {
			for _, p := range EgressPolicies(obj) {
				if p.Namespace == "" || keep[p.Namespace] {
					inv.NetworkPolicies = append(inv.NetworkPolicies, p)
				}
			}
		}
	}
	if nps, err := c.Core.NetworkingV1().NetworkPolicies(listNS).List(ctx, metav1.ListOptions{}); err == nil {
		var objects []map[string]any
		for i := range nps.Items {
			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&nps.Items[i])
			if err != nil {
				continue
			}
			// The items of a typed list do not carry their kind.
			obj["apiVersion"], obj["kind"] = "networking.k8s.io/v1", "NetworkPolicy"
			objects = append(objects, obj)
		}
		policies(objects)
	} else {
		warn("network policies", err)
	}
	if c.Dynamic != nil {
		for _, gvr := range []schema.GroupVersionResource{ciliumPolicyGVR, ciliumClusterPolicyGVR} {
			res := c.Dynamic.Resource(gvr)
			var list *unstructured.UnstructuredList
			var err error
			if gvr == ciliumPolicyGVR {
				list, err = res.Namespace(listNS).List(ctx, metav1.ListOptions{})
			} else {
				list, err = res.List(ctx, metav1.ListOptions{})
			}
			switch {
			case err == nil:
				var objects []map[string]any
				for i := range list.Items {
					objects = append(objects, list.Items[i].Object)
				}
				policies(objects)
			case !errors.IsNotFound(err) && !isNoMatch(err):
				warn("cilium network policies ("+gvr.Resource+")", err)
			}
		}
	}
	sort.SliceStable(inv.NetworkPolicies, func(i, j int) bool {
		a, b := inv.NetworkPolicies[i], inv.NetworkPolicies[j]
		return a.Namespace+"/"+a.Kind+"/"+a.Name < b.Namespace+"/"+b.Kind+"/"+b.Name
	})
	return inv, nil
}

// EgressPolicies reads the egress side of one network policy as it is
// decoded from YAML or JSON: a NetworkPolicy, a CiliumNetworkPolicy or a
// CiliumClusterwideNetworkPolicy. It is the one reader for the manifests of
// a repository and for the objects of a cluster. Any other object, a policy
// without egress rules and a policy for the nodes themselves give nothing;
// a Cilium policy with several specs gives one entry per spec.
func EgressPolicies(obj map[string]any) []NetworkPolicyInfo {
	kind, api := policyText(obj["kind"]), policyText(obj["apiVersion"])
	meta := policyMap(obj["metadata"])
	base := NetworkPolicyInfo{Kind: kind, Namespace: policyText(meta["namespace"]), Name: policyText(meta["name"])}
	if base.Name == "" {
		return nil
	}
	switch {
	case kind == "NetworkPolicy" && strings.HasPrefix(api, "networking.k8s.io/"):
		spec := policyMap(obj["spec"])
		_, hasRules := spec["egress"]
		isEgress := hasRules
		for _, t := range policyList(spec["policyTypes"]) {
			if policyText(t) == "Egress" {
				isEgress = true
			}
		}
		if !isEgress {
			return nil
		}
		p := base
		p.Selector, _, p.Expressions = policySelector(policyMap(spec["podSelector"]))
		for _, rv := range policyList(spec["egress"]) {
			to := policyList(policyMap(rv)["to"])
			if len(to) == 0 {
				p.Open = true // a rule without destinations allows them all
				continue
			}
			for _, tv := range to {
				peer := policyMap(tv)
				if cidr := policyText(policyMap(peer["ipBlock"])["cidr"]); cidr != "" {
					p.addCIDR(cidr)
				}
				var parts []string
				if sel, ok := peer["namespaceSelector"]; ok {
					parts = append(parts, "namespaces "+selectorText(policyMap(sel)))
				}
				if sel, ok := peer["podSelector"]; ok {
					parts = append(parts, "pods "+selectorText(policyMap(sel)))
				}
				if len(parts) > 0 {
					p.Endpoints = append(p.Endpoints, strings.Join(parts, ", "))
				}
			}
		}
		return []NetworkPolicyInfo{p.sorted()}
	case (kind == "CiliumNetworkPolicy" || kind == "CiliumClusterwideNetworkPolicy") && strings.HasPrefix(api, "cilium.io/"):
		if kind == "CiliumClusterwideNetworkPolicy" {
			base.Namespace = ""
		}
		var specs []map[string]any
		if spec := policyMap(obj["spec"]); len(spec) > 0 {
			specs = append(specs, spec)
		}
		for _, sv := range policyList(obj["specs"]) {
			specs = append(specs, policyMap(sv))
		}
		var out []NetworkPolicyInfo
		for _, spec := range specs {
			if _, ok := spec["egress"]; !ok {
				continue
			}
			if _, nodes := spec["nodeSelector"]; nodes {
				continue // a policy for the nodes, not for pods
			}
			p := base
			var ns string
			p.Selector, ns, p.Expressions = policySelector(policyMap(spec["endpointSelector"]))
			if p.Namespace == "" {
				p.Namespace = ns
			}
			for _, rv := range policyList(spec["egress"]) {
				rule := policyMap(rv)
				for _, fv := range policyList(rule["toFQDNs"]) {
					fqdn := policyMap(fv)
					if name := hostText(fqdn["matchName"]); name != "" {
						p.Hosts = append(p.Hosts, name)
					}
					if pattern := hostText(fqdn["matchPattern"]); pattern != "" {
						p.Patterns = append(p.Patterns, pattern)
					}
				}
				for _, ev := range policyList(rule["toEndpoints"]) {
					p.Endpoints = append(p.Endpoints, "pods "+selectorText(policyMap(ev)))
				}
				for _, cv := range policyList(rule["toCIDR"]) {
					p.addCIDR(policyText(cv))
				}
				for _, cv := range policyList(rule["toCIDRSet"]) {
					p.addCIDR(policyText(policyMap(cv)["cidr"]))
				}
				for _, ev := range policyList(rule["toEntities"]) {
					if e := policyText(ev); e == "world" || e == "all" {
						p.Open = true
					}
				}
			}
			out = append(out, p.sorted())
		}
		return out
	}
	return nil
}

// addCIDR records an address range; the range that holds every address
// makes the policy open.
func (p *NetworkPolicyInfo) addCIDR(cidr string) {
	switch cidr {
	case "":
	case "0.0.0.0/0", "::/0":
		p.Open = true
	default:
		p.CIDRs = append(p.CIDRs, cidr)
	}
}

// sorted returns the policy with its lists in order and each entry once.
func (p NetworkPolicyInfo) sorted() NetworkPolicyInfo {
	for _, list := range []*[]string{&p.Hosts, &p.Patterns, &p.CIDRs, &p.Endpoints} {
		if *list = uniqueStrings(*list); len(*list) == 0 {
			*list = nil
		}
	}
	return p
}

// namespaceLabel is the label Cilium selects the namespace of a pod by.
const namespaceLabel = "io.kubernetes.pod.namespace"

// policySelector reads the matchLabels of a selector as "a=b,c=d", without
// the source prefix Cilium allows on a key (k8s:app). The namespace label
// is returned apart. expressions reports whether the selector also holds
// matchExpressions, which are not evaluated.
func policySelector(sel map[string]any) (selector, namespace string, expressions bool) {
	labels := policyMap(sel["matchLabels"])
	var parts []string
	for _, k := range sortedKeys(labels) {
		key := strings.TrimPrefix(strings.TrimPrefix(k, "k8s:"), "any:")
		if key == namespaceLabel {
			namespace = policyText(labels[k])
			continue
		}
		parts = append(parts, key+"="+policyText(labels[k]))
	}
	return strings.Join(parts, ","), namespace, len(policyList(sel["matchExpressions"])) > 0
}

// selectorText words a selector for a note.
func selectorText(sel map[string]any) string {
	s, ns, expr := policySelector(sel)
	if ns != "" {
		s = strings.Trim(namespaceLabel+"="+ns+","+s, ",")
	}
	switch {
	case s == "" && expr:
		return "chosen by an expression"
	case s == "":
		return "(all)"
	case expr:
		return s + " and an expression"
	}
	return s
}

// hostText reads a host name of a policy: lowercase, without the final dot.
func hostText(v any) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(policyText(v)), "."))
}

// policyMap, policyList and policyText read a decoded document without
// copying it. The helpers of the unstructured package copy, and refuse the
// plain int a YAML decoder yields for a port.
func policyMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func policyList(v any) []any {
	l, _ := v.([]any)
	return l
}

func policyText(v any) string {
	s, _ := v.(string)
	return s
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

// envRefs lists the environment of every container: the names, and of the
// values only what redact.EnvValue lets through. Variables that come from a
// ConfigMap are resolved through configMap; variables that come from a
// Secret keep the reference and nothing else. $(VAR) references are
// interpolated from the same container before the value is reduced, so a
// URL assembled from parts still yields its host.
func envRefs(ns string, t corev1.PodTemplateSpec, configMap func(ns, name string) (map[string]string, bool)) []EnvRef {
	var out []EnvRef
	for _, c := range t.Spec.Containers {
		// raw holds the literal values for interpolation. It never leaves
		// this function.
		raw := map[string]string{}
		lookup := func(name string) (string, bool) { v, ok := raw[name]; return v, ok }
		// envFrom comes first: Kubernetes lets env override it.
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil {
				out = append(out, EnvRef{Container: c.Name, Name: "*", SecretRef: ef.SecretRef.Name + "/*"})
			}
			if ef.ConfigMapRef == nil {
				continue
			}
			data, ok := configMap(ns, ef.ConfigMapRef.Name)
			if !ok {
				out = append(out, EnvRef{Container: c.Name, Name: "*", ConfigRef: ef.ConfigMapRef.Name + "/*"})
				continue
			}
			for _, k := range sortedKeys(data) {
				raw[ef.Prefix+k] = data[k]
			}
			for _, k := range sortedKeys(data) {
				name := ef.Prefix + k
				out = append(out, EnvRef{Container: c.Name, Name: name, ConfigRef: ef.ConfigMapRef.Name + "/" + k,
					Value: redact.EnvValue(name, redact.Expand(data[k], lookup))})
			}
		}
		for _, e := range c.Env {
			r := EnvRef{Container: c.Name, Name: e.Name}
			value := e.Value
			if e.ValueFrom != nil {
				if e.ValueFrom.SecretKeyRef != nil {
					r.SecretRef = e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
				}
				if ref := e.ValueFrom.ConfigMapKeyRef; ref != nil {
					r.ConfigRef = ref.Name + "/" + ref.Key
					if data, ok := configMap(ns, ref.Name); ok {
						value = data[ref.Key]
					}
				}
			}
			if r.SecretRef == "" && value != "" {
				value = redact.Expand(value, lookup)
				raw[e.Name] = value
				r.Value = redact.EnvValue(e.Name, value)
			}
			out = append(out, r)
		}
	}
	return out
}

// commands joins command and args per container. Credentials in a flag, an
// assignment or a URL are masked before the line is kept.
func commands(t corev1.PodTemplateSpec) []string {
	var out []string
	for _, c := range t.Spec.Containers {
		parts := redact.Args(append(append([]string{}, c.Command...), c.Args...))
		if len(parts) > 0 {
			out = append(out, c.Name+": "+strings.Join(parts, " "))
		}
	}
	return out
}

// mergeLabels returns the pod template's labels completed with the labels
// of the object itself: charts often put app.kubernetes.io/instance and
// part-of on the Deployment and leave them off the pods.
func mergeLabels(template, object map[string]string) map[string]string {
	if len(template) == 0 && len(object) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range object {
		out[k] = v
	}
	for k, v := range template {
		out[k] = v
	}
	return out
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
