package discover

import (
	"strings"
	"testing"

	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

const shopPolicies = `
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: api-egress
  namespace: shop
spec:
  endpointSelector:
    matchLabels:
      app: api
  egress:
    - toFQDNs:
        - matchName: api.payments.example
        - matchName: api.stripe.com
        - matchPattern: "*.mail.example"
        - matchPattern: "*"
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: worker-egress
  namespace: shop
spec:
  endpointSelector:
    matchLabels:
      app: worker
  egress:
    - toFQDNs:
        - matchName: hooks.chat.example
    - toEndpoints:
        - matchLabels:
            app: cache
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: everyone
  namespace: shop
spec:
  endpointSelector: {}
  egress:
    - toFQDNs:
        - matchName: status.metrics.example
`

// shop is a repository with two workloads, the network policies that say
// what each may reach, and code that calls outside services.
func shop(t *testing.T, extra map[string]string) string {
	files := map[string]string{
		"deploy/api.yaml":      deployment("api", "shop", "ghcr.io/bookstore/api:1.0.0", "MAIL_URL", "https://smtp.mail.example"),
		"deploy/worker.yaml":   deployment("worker", "shop", "ghcr.io/bookstore/worker:1.0.0"),
		"deploy/policies.yaml": shopPolicies,
		"apps/api/pay.py":      "CHARGES = \"https://api.stripe.com/v1/charges\"\n",
		"apps/worker/geo.py":   "# tiles\nTILES = \"https://api.mapbox.com/v4\"\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	return writeTree(t, files)
}

func cites(ev []Evidence, part string) bool {
	for _, e := range ev {
		if strings.Contains(e.String(), part) {
			return true
		}
	}
	return false
}

func TestPolicyHostsBecomeOutsideServices(t *testing.T) {
	f, err := Scan(Options{Root: shop(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	p := Propose(f, ProposeOptions{Name: "shop"})

	// A host a policy names is an outside service, reached by the workload
	// the policy names and by no other.
	for _, id := range []string{"payments", "chat", "metrics", "stripe"} {
		if !hasComp(p, id, "external") {
			t.Errorf("no external %s; components %+v", id, p.Topology.Components)
		}
	}
	for _, e := range [][2]string{{"api", "payments"}, {"worker", "chat"}, {"api", "stripe"}} {
		if !hasEdge(p, e[0], e[1], "external") {
			t.Errorf("no edge %s -> %s; edges %+v", e[0], e[1], p.Topology.Edges)
		}
	}
	for _, e := range [][2]string{{"worker", "payments"}, {"api", "chat"}, {"worker", "stripe"}} {
		if hasEdge(p, e[0], e[1], "") {
			t.Errorf("edge %s -> %s: the policy does not apply to %s", e[0], e[1], e[0])
		}
	}
	// Cited to the line the host is written on.
	if ev := p.Evidence["api->payments"]; len(ev) != 1 || !ev[0].Policy || ev[0].String() != "manifest deploy/policies.yaml:12: CiliumNetworkPolicy shop/api-egress allows the pods app=api to reach api.payments.example" {
		t.Errorf("evidence of api->payments = %+v", ev)
	}
	if c := p.Confidence["api->payments"]; c.Level != Medium || c.Basis != BasisRepository {
		t.Errorf("confidence of api->payments = %+v", c)
	}
	if c := p.Confidence["payments"]; c.Level != Medium {
		t.Errorf("confidence of payments = %+v", c)
	}

	// A policy for every pod of the namespace lists the service and draws
	// no flow: it does not say who calls.
	for _, e := range p.Topology.Edges {
		if e.To == "metrics" {
			t.Errorf("edge %s -> metrics drawn from a policy for the whole namespace", e.From)
		}
	}

	// The policy is cited before the code that calls the same host.
	ev := p.Evidence["api->stripe"]
	if len(ev) != 2 || !ev[0].Policy || ev[1].Source != "code" {
		t.Errorf("evidence of api->stripe = %+v, want the policy first, then the code", ev)
	}
	if c := p.Confidence["api->stripe"]; c.Level != Medium || !strings.Contains(c.Citation, "api-egress") || c.Caveat != "" {
		t.Errorf("confidence of api->stripe = %+v", c)
	}

	// A pattern is not drawn; it confirms the host that matches it.
	if !cites(p.Evidence["api->mail"], "allows the pods app=api to reach *.mail.example") {
		t.Errorf("evidence of api->mail = %+v", p.Evidence["api->mail"])
	}
	for _, c := range p.Topology.Components {
		if strings.Contains(c.ID, "*") || c.Label == "*" {
			t.Errorf("a pattern was drawn: %+v", c)
		}
	}
	notes := strings.Join(p.Notes, "\n")
	for _, want := range []string{
		"CiliumNetworkPolicy shop/api-egress (deploy/policies.yaml:1) allows the pods app=api to reach names like *; names like *.mail.example: not drawn",
		"CiliumNetworkPolicy shop/worker-egress (deploy/policies.yaml:16) allows the pods app=worker to reach pods app=cache: not drawn",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("no note %q in:\n%s", want, notes)
		}
	}
}

func TestHostNoPolicyAllows(t *testing.T) {
	f, err := Scan(Options{Root: shop(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	p := Propose(f, ProposeOptions{Name: "shop"})
	c := p.Confidence["worker->mapbox"]
	if c.Level != Low || c.Basis != BasisRepository || c.Caveat != "no network policy that applies to worker allows it" {
		t.Errorf("confidence of worker->mapbox = %+v", c)
	}
	want := "worker -> mapbox: only the code mentions api.mapbox.com and no network policy that applies to worker allows it"
	if !strings.Contains(strings.Join(p.Notes, "\n"), want) {
		t.Errorf("no note %q in:\n%s", want, strings.Join(p.Notes, "\n"))
	}
	// api may reach every name ("*"): nothing can be said against a call.
	if c := p.Confidence["api->stripe"]; c.Caveat != "" {
		t.Errorf("api->stripe = %+v", c)
	}
}

func TestPolicyThatCannotTell(t *testing.T) {
	cases := map[string]string{
		"an address range may hold the host": `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: ranges, namespace: shop}
spec:
  podSelector:
    matchLabels: {app: worker}
  policyTypes: [Egress]
  egress:
    - to:
        - ipBlock: {cidr: 203.0.113.0/24}
`,
		"the world is allowed": `
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata: {name: world, namespace: shop}
spec:
  endpointSelector:
    matchLabels: {app: worker}
  egress:
    - toEntities: [world]
`,
		"the pods are chosen by an expression": `
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata: {name: some, namespace: shop}
spec:
  endpointSelector:
    matchExpressions:
      - {key: app, operator: In, values: [api, worker]}
  egress:
    - toFQDNs:
        - matchName: api.ledger.example
`,
	}
	for name, doc := range cases {
		f, err := Scan(Options{Root: shop(t, map[string]string{"deploy/more.yaml": doc})})
		if err != nil {
			t.Fatal(err)
		}
		p := Propose(f, ProposeOptions{Name: "shop"})
		if c := p.Confidence["worker->mapbox"]; c.Level != Low || c.Caveat != "" {
			t.Errorf("%s: worker->mapbox = %+v, want low without a caveat", name, c)
		}
		if name == "the pods are chosen by an expression" {
			if !hasComp(p, "ledger", "external") {
				t.Errorf("%s: the host it names is an outside service all the same", name)
			}
			for _, e := range p.Topology.Edges {
				if e.To == "ledger" {
					t.Errorf("%s: edge %s -> ledger is a guess", name, e.From)
				}
			}
		}
	}
	// No policy at all: nothing to hold a call against.
	f, err := Scan(Options{Root: shop(t, map[string]string{"deploy/policies.yaml": ""})})
	if err != nil {
		t.Fatal(err)
	}
	if c := Propose(f, ProposeOptions{Name: "shop"}).Confidence["worker->mapbox"]; c.Level != Low || c.Caveat != "" {
		t.Errorf("without policies: worker->mapbox = %+v", c)
	}
}

func TestPatternMatches(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"*.mail.example", "smtp.mail.example", true},
		{"*.mail.example", "mail.example", false},
		{"*.mail.example", "a.b.mail.example", false},
		{"**.mail.example", "a.b.mail.example", true},
		{"api-*.payments.example", "api-eu.payments.example", true},
		{"api-*.payments.example", "api.payments.example", false},
		{"*", "anything.example", true},
		{"*.MAIL.example", "SMTP.mail.example", true},
		{"smtp.mail.example", "smtpxmail.example", false},
	}
	for _, c := range cases {
		if got := patternMatches(c.pattern, c.host); got != c.want {
			t.Errorf("patternMatches(%q, %q) = %v", c.pattern, c.host, got)
		}
	}
}

// The policies of the live cluster count for more than those of the
// repository, and what the cluster would not show is said.
func TestClusterPolicies(t *testing.T) {
	f, err := Scan(Options{Root: shop(t, map[string]string{"deploy/policies.yaml": ""})})
	if err != nil {
		t.Fatal(err)
	}
	inv := &k8s.Inventory{
		Context: "bookstore", Namespaces: []string{"shop"},
		Workloads: []k8s.WorkloadInfo{
			{Namespace: "shop", Kind: "Deployment", Name: "api", Selector: "app=api", Images: []string{"ghcr.io/bookstore/api:1.0.0"}, Labels: map[string]string{"app": "api"}},
			{Namespace: "shop", Kind: "Deployment", Name: "worker", Selector: "app=worker", Images: []string{"ghcr.io/bookstore/worker:1.0.0"}, Labels: map[string]string{"app": "worker"}},
		},
		NetworkPolicies: []k8s.NetworkPolicyInfo{
			{Kind: "CiliumNetworkPolicy", Namespace: "shop", Name: "api-egress", Selector: "app=api", Hosts: []string{"api.stripe.com", "api.payments.example"}},
			{Kind: "CiliumNetworkPolicy", Namespace: "shop", Name: "worker-egress", Selector: "app=worker", Hosts: []string{"hooks.chat.example"}},
		},
		Warnings: []string{"network policies: networkpolicies.networking.k8s.io is forbidden"},
	}
	AddCluster(f, inv)
	AddCluster(f, inv) // adding it twice must not cite twice
	p := Propose(f, ProposeOptions{Name: "shop"})

	ev := p.Evidence["api->stripe"]
	if len(ev) != 2 || ev[0].String() != "cluster: CiliumNetworkPolicy shop/api-egress allows the pods app=api to reach api.stripe.com" || ev[1].Source != "code" {
		t.Errorf("evidence of api->stripe = %+v", ev)
	}
	for _, id := range []string{"api->stripe", "stripe", "api->payments", "payments", "worker->chat"} {
		if c := p.Confidence[id]; c.Level != High || c.Basis != BasisCluster {
			t.Errorf("confidence of %s = %+v, want high from the cluster", id, c)
		}
	}
	if c := p.Confidence["worker->mapbox"]; c.Level != Low || c.Caveat == "" {
		t.Errorf("confidence of worker->mapbox = %+v", c)
	}
	if !strings.Contains(strings.Join(p.Notes, "\n"), "the cluster did not answer for network policies: networkpolicies.networking.k8s.io is forbidden") {
		t.Errorf("the warning of the inventory is not in the notes:\n%s", strings.Join(p.Notes, "\n"))
	}
}
