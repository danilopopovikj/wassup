package k8s

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newInventoryDynamic is a dynamic client that knows every custom resource
// Discover lists: CloudNativePG clusters and the Cilium policies.
func newInventoryDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			cnpgClusterGVR: "ClusterList", cnpgBackupGVR: "BackupList",
			ciliumPolicyGVR: "CiliumNetworkPolicyList", ciliumClusterPolicyGVR: "CiliumClusterwideNetworkPolicyList",
		}, objs...)
}

// decode reads one YAML document the way a manifest is read.
func decode(t *testing.T, doc string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

const paymentsPolicy = `
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: api-egress
  namespace: prod
spec:
  endpointSelector:
    matchLabels:
      "k8s:app": api
  egress:
    - toFQDNs:
        - matchName: API.Payments.Example.
        - matchPattern: "*.mail.example"
      toPorts:
        - ports:
            - port: "443"
    - toEndpoints:
        - matchLabels:
            app: redis
    - toCIDR:
        - 10.20.0.0/16
      toCIDRSet:
        - cidr: 192.0.2.0/24
`

func TestEgressPolicies(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []NetworkPolicyInfo
	}{
		{"cilium names, patterns, pods and ranges", paymentsPolicy, []NetworkPolicyInfo{{
			Kind: "CiliumNetworkPolicy", Namespace: "prod", Name: "api-egress", Selector: "app=api",
			Hosts: []string{"api.payments.example"}, Patterns: []string{"*.mail.example"},
			CIDRs: []string{"10.20.0.0/16", "192.0.2.0/24"}, Endpoints: []string{"pods app=redis"}}}},
		{"cilium clusterwide takes the namespace from the selector, one entry per spec", `
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: outside
specs:
  - endpointSelector:
      matchLabels:
        io.kubernetes.pod.namespace: prod
    egress:
      - toFQDNs:
          - matchName: hooks.chat.example
  - endpointSelector:
      matchExpressions:
        - {key: app, operator: In, values: [api, worker]}
    egress:
      - toEntities: [world]
  - nodeSelector:
      matchLabels: {role: edge}
    egress:
      - toEntities: [world]
`, []NetworkPolicyInfo{
			{Kind: "CiliumClusterwideNetworkPolicy", Namespace: "prod", Name: "outside", Hosts: []string{"hooks.chat.example"}},
			{Kind: "CiliumClusterwideNetworkPolicy", Name: "outside", Expressions: true, Open: true},
		}},
		{"cilium without egress rules is no allowlist", `
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata: {name: ingress-only, namespace: prod}
spec:
  endpointSelector: {}
  ingress:
    - fromEndpoints: [{}]
`, nil},
		{"network policy with ranges and pods", `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: worker-egress, namespace: prod}
spec:
  podSelector:
    matchLabels: {app: worker}
  policyTypes: [Egress]
  egress:
    - to:
        - ipBlock: {cidr: 203.0.113.0/24}
        - namespaceSelector:
            matchLabels: {kubernetes.io/metadata.name: data}
          podSelector:
            matchLabels: {app: db}
      ports:
        - port: 5432
`, []NetworkPolicyInfo{{Kind: "NetworkPolicy", Namespace: "prod", Name: "worker-egress", Selector: "app=worker",
			CIDRs: []string{"203.0.113.0/24"}, Endpoints: []string{"namespaces kubernetes.io/metadata.name=data, pods app=db"}}}},
		{"network policy that denies all egress is an empty allowlist", `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: deny, namespace: prod}
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
`, []NetworkPolicyInfo{{Kind: "NetworkPolicy", Namespace: "prod", Name: "deny"}}},
		{"network policy rule without destinations is open", `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: dns, namespace: prod}
spec:
  podSelector: {}
  egress:
    - ports:
        - port: 53
    - to:
        - ipBlock: {cidr: 0.0.0.0/0}
`, []NetworkPolicyInfo{{Kind: "NetworkPolicy", Namespace: "prod", Name: "dns", Open: true}}},
		{"network policy for ingress only", `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: in, namespace: prod}
spec:
  podSelector: {}
  policyTypes: [Ingress]
`, nil},
		{"another object", `
apiVersion: v1
kind: Service
metadata: {name: api, namespace: prod}
spec:
  egress: []
`, nil},
	}
	for _, c := range cases {
		if got := EgressPolicies(decode(t, c.doc)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestDiscoverNetworkPolicies(t *testing.T) {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "worker-egress", Namespace: "prod"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.0/24"}}}}},
		}}
	other := np.DeepCopy()
	other.Namespace = "kube-system"
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		np, other,
	}
	cilium := &unstructured.Unstructured{Object: decode(t, paymentsPolicy)}
	c := newTestClients(objs, nil, newInventoryDynamic(cilium))
	inv, err := Discover(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []NetworkPolicyInfo{
		{Kind: "CiliumNetworkPolicy", Namespace: "prod", Name: "api-egress", Selector: "app=api",
			Hosts: []string{"api.payments.example"}, Patterns: []string{"*.mail.example"},
			CIDRs: []string{"10.20.0.0/16", "192.0.2.0/24"}, Endpoints: []string{"pods app=redis"}},
		{Kind: "NetworkPolicy", Namespace: "prod", Name: "worker-egress", Selector: "app=worker", CIDRs: []string{"203.0.113.0/24"}},
	}
	if !reflect.DeepEqual(inv.NetworkPolicies, want) {
		t.Errorf("network policies:\n got %+v\nwant %+v (kube-system must be left out)", inv.NetworkPolicies, want)
	}
	if len(inv.Warnings) != 0 {
		t.Errorf("warnings = %v", inv.Warnings)
	}
}

// A cluster that does not let the policies be listed, or that has no
// Cilium, is still discovered: the first is a warning, the second nothing.
func TestDiscoverWithoutPolicyAccess(t *testing.T) {
	objs := []runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}, apiDeployment("ghcr.io/bookstore/api:b7e9f21", 1)}
	dyn := newInventoryDynamic()
	dyn.PrependReactor("list", "ciliumnetworkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "cilium.io", Resource: "ciliumnetworkpolicies"}, "", nil)
	})
	dyn.PrependReactor("list", "ciliumclusterwidenetworkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "cilium.io", Resource: "ciliumclusterwidenetworkpolicies"}, "")
	})
	c := newTestClients(objs, nil, dyn)
	c.Core.(interface {
		PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
	}).PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", nil)
	})
	inv, err := Discover(context.Background(), c, nil)
	if err != nil {
		t.Fatalf("discovery must go on without the policies: %v", err)
	}
	if len(inv.Workloads) != 1 || len(inv.NetworkPolicies) != 0 {
		t.Errorf("inventory = %+v", inv)
	}
	joined := strings.Join(inv.Warnings, "\n")
	if len(inv.Warnings) != 2 || !strings.Contains(joined, "network policies") || !strings.Contains(joined, "ciliumnetworkpolicies") {
		t.Errorf("warnings = %v, want one per list that was forbidden and none for the missing kind", inv.Warnings)
	}
}
