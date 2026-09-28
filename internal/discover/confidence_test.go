package discover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

func TestRate(t *testing.T) {
	code := Evidence{Source: "code", File: "apps/api/pay.py", Line: 3, Note: "calls api.stripe.com"}
	dotenv := Evidence{Source: "dotenv", File: ".env.example", Line: 1, Note: "DATABASE_URL"}
	manifest := Evidence{Source: "manifest", File: "deploy/api.yaml", Line: 1, Note: "Deployment api"}
	helm := Evidence{Source: "helm", File: "charts/electric/values.yaml", Line: 1, Note: "values for electric"}
	terraform := Evidence{Source: "terraform", File: "infra/main.tf", Line: 9, Note: "hcloud_load_balancer.bookstore"}
	policy := Evidence{Source: "manifest", File: "deploy/policies.yaml", Line: 12, Note: "allows api.stripe.com", Policy: true}
	cluster := Evidence{Source: "cluster", Note: "Deployment shop/api"}
	inferred := Evidence{Source: "inference", Note: "the only Postgres is the sync source"}
	cases := []struct {
		name     string
		evidence []Evidence
		want     Confidence
	}{
		{"nothing", nil, Confidence{Level: Low, Basis: BasisRepository}},
		{"concluded", []Evidence{inferred}, Confidence{Level: Low, Basis: BasisRepository, Citation: inferred.String()}},
		{"code only", []Evidence{code}, Confidence{Level: Low, Basis: BasisRepository, Citation: code.String()}},
		{"code and a .env file", []Evidence{dotenv, code}, Confidence{Level: Low, Basis: BasisRepository, Citation: dotenv.String()}},
		{"a manifest after code", []Evidence{code, manifest}, Confidence{Level: Medium, Basis: BasisRepository, Citation: manifest.String()}},
		{"helm values", []Evidence{helm}, Confidence{Level: Medium, Basis: BasisRepository, Citation: helm.String()}},
		{"terraform", []Evidence{inferred, terraform}, Confidence{Level: Medium, Basis: BasisRepository, Citation: terraform.String()}},
		{"a policy before a manifest", []Evidence{code, manifest, policy}, Confidence{Level: Medium, Basis: BasisRepository, Citation: policy.String()}},
		{"the cluster before everything", []Evidence{code, policy, manifest, cluster}, Confidence{Level: High, Basis: BasisCluster, Citation: cluster.String()}},
	}
	for _, c := range cases {
		before := append([]Evidence(nil), c.evidence...)
		if got := Rate(c.evidence); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
		for i := range before {
			if before[i] != c.evidence[i] {
				t.Errorf("%s: Rate changed the order of its input", c.name)
			}
		}
	}
}

func TestProposalIsRated(t *testing.T) {
	f, p := scanFixture(t)
	for _, c := range p.Topology.Components {
		if _, ok := p.Confidence[c.ID]; !ok {
			t.Errorf("component %s has no confidence", c.ID)
		}
	}
	for _, e := range p.Topology.Edges {
		if _, ok := p.Confidence[e.ID()]; !ok {
			t.Errorf("edge %s has no confidence", e.ID())
		}
	}
	want := map[string]string{
		"api": Medium, "bookstore-lb": Medium, "api->bookstore-db": Medium, // a manifest, Terraform
		"stripe": Low, "api->stripe": Low, "api->exports-queue": Low, // code only
	}
	for id, level := range want {
		if c := p.Confidence[id]; c.Level != level || c.Basis != BasisRepository || c.Citation == "" {
			t.Errorf("confidence of %s = %+v, want %s from the repository", id, c, level)
		}
	}

	// The same system seen running: what the cluster shows is high, what
	// only the repository has stays where it was.
	AddCluster(f, &k8s.Inventory{
		Context: "bookstore", Namespaces: []string{"bookstore"},
		Workloads: []k8s.WorkloadInfo{{Namespace: "bookstore", Kind: "Deployment", Name: "api", Selector: "app=api", Images: []string{"ghcr.io/bookstore/api:1.2.3"},
			Labels: map[string]string{"app": "api"}, Env: []k8s.EnvRef{{Container: "api", Name: "REDIS_URL", Value: "redis://redis.bookstore:6379"}}}},
	})
	p = Propose(f, ProposeOptions{Name: "bookstore"})
	for _, id := range []string{"api", "api->redis"} {
		if c := p.Confidence[id]; c.Level != High || c.Basis != BasisCluster || !strings.HasPrefix(c.Citation, "cluster: ") {
			t.Errorf("confidence of %s = %+v, want high from the cluster", id, c)
		}
	}
	if c := p.Confidence["worker"]; c.Level != Medium {
		t.Errorf("confidence of worker = %+v: the cluster did not show it", c)
	}
	if c := p.Confidence["api->stripe"]; c.Level != Low {
		t.Errorf("confidence of api->stripe = %+v: only the code mentions it", c)
	}
	// The topology itself carries none of it.
	b, _ := yamlOf(t, p)
	if strings.Contains(b, "confidence") {
		t.Errorf("confidence leaked into the topology:\n%s", b)
	}
}

// yamlOf writes the proposal and returns its topology.yaml.
func yamlOf(t *testing.T, p *Proposal) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := WriteProposal(dir, p, &Findings{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ProposedDir, "topology.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b), dir
}

func TestReview(t *testing.T) {
	f, err := Scan(Options{Root: shop(t, map[string]string{
		"deploy/api.yaml": deployment("api", "shop", "ghcr.io/bookstore/api:1.0.0", "MAIL_URL", "https://smtp.mail.example", "LEDGER_HOST", "ledger.shop"),
	})})
	if err != nil {
		t.Fatal(err)
	}
	AddCluster(f, &k8s.Inventory{Context: "bookstore", Namespaces: []string{"shop"},
		Workloads: []k8s.WorkloadInfo{{Namespace: "shop", Kind: "Deployment", Name: "api", Selector: "app=api", Images: []string{"ghcr.io/bookstore/api:1.0.0"}, Labels: map[string]string{"app": "api"}}}})
	p := Propose(f, ProposeOptions{Name: "shop"})
	got := p.Review()

	for _, want := range []string{
		"# Review of the draft",
		"shop: 8 components, 5 edges.",
		"- api (workload), high, cluster: Deployment shop/api",
		"- worker (workload), medium, manifest deploy/worker.yaml:1: Deployment worker",
		"- api -> payments (external), medium, manifest deploy/policies.yaml:12: CiliumNetworkPolicy shop/api-egress allows the pods app=api to reach api.payments.example",
		"- mapbox (external), low, code apps/worker/geo.py:2: calls api.mapbox.com",
		"- worker -> mapbox (external), low, code apps/worker/geo.py:2: calls api.mapbox.com (no network policy that applies to worker allows it)",
		"## unresolved hosts",
		"- api -> ledger.shop, manifest deploy/api.yaml:1: LEDGER_HOST in Deployment api",
		"## notes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the review lacks %q:\n%s", want, got)
		}
	}
	// What is surest comes first, the hosts and the notes last.
	order := []string{"## high", "## medium", "## low", "## unresolved hosts", "## notes"}
	last := -1
	for _, h := range order {
		i := strings.Index(got, "\n"+h+"\n")
		if i < 0 || i < last {
			t.Errorf("%q is missing or out of order:\n%s", h, got)
		}
		last = i
	}
	// One line per component and per edge, each with one citation.
	lines := 0
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "- ") && (strings.Contains(l, ", high, ") || strings.Contains(l, ", medium, ") || strings.Contains(l, ", low, ")) {
			lines++
		}
	}
	if want := len(p.Topology.Components) + len(p.Topology.Edges); lines != want {
		t.Errorf("%d lines for %d components and edges:\n%s", lines, want, got)
	}
}

func TestWriteProposalWritesTheReview(t *testing.T) {
	f, err := Scan(Options{Root: shop(t, map[string]string{
		// a password in a manifest must reach neither file
		"deploy/worker.yaml": deployment("worker", "shop", "ghcr.io/bookstore/worker:1.0.0", "DATABASE_URL", "postgresql://app:S3cr3t-Pa55@db.shop:5432/app"),
	})})
	if err != nil {
		t.Fatal(err)
	}
	p := Propose(f, ProposeOptions{Name: "shop"})
	dir := t.TempDir()
	if err := WriteProposal(dir, p, f); err != nil {
		t.Fatal(err)
	}
	review, err := os.ReadFile(filepath.Join(dir, ProposedDir, ReviewFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(review) != p.Review() {
		t.Errorf("review.md is not what Review returns:\n%s", review)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ProposedDir, "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Confidence map[string]Confidence `json:"confidence"`
		Findings   struct {
			Policies []EgressPolicy `json:"policies"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if c := ev.Confidence["api->payments"]; c.Level != Medium || c.Basis != BasisRepository {
		t.Errorf("evidence.json: confidence of api->payments = %+v", c)
	}
	if len(ev.Findings.Policies) != 3 || ev.Findings.Policies[0].Name != "api-egress" || ev.Findings.Policies[0].File != "deploy/policies.yaml" {
		t.Errorf("evidence.json: policies = %+v", ev.Findings.Policies)
	}
	for name, content := range map[string]string{ReviewFile: string(review), "evidence.json": string(raw)} {
		if strings.Contains(content, "S3cr3t") || strings.Contains(content, "app:") && strings.Contains(content, "@db.shop") {
			t.Errorf("%s holds a credential:\n%s", name, content)
		}
	}
	// A proposal that was built by hand, without confidence, is rated from
	// its evidence when it is reviewed.
	bare := &Proposal{Topology: p.Topology, Evidence: p.Evidence}
	if !strings.Contains(bare.Review(), "- api -> payments (external), medium, ") {
		t.Errorf("review of a proposal without confidence:\n%s", bare.Review())
	}
}
