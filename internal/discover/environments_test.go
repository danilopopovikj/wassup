package discover

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTree writes a small repository under a temporary directory. The
// trees of these tests are written here rather than kept under testdata,
// because the .gitignore of this repository would apply to them.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.TrimLeft(content, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// deployment is the manifest of a Deployment with one container.
func deployment(name, ns, image string, env ...string) string {
	out := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + name + "\n"
	if ns != "" {
		out += "  namespace: " + ns + "\n"
	}
	out += "spec:\n  selector:\n    matchLabels:\n      app: " + name + "\n  template:\n    metadata:\n      labels:\n        app: " + name + "\n" +
		"    spec:\n      containers:\n        - name: " + name + "\n          image: " + image + "\n"
	if len(env) > 0 {
		out += "          env:\n"
		for i := 0; i+1 < len(env); i += 2 {
			out += "            - name: " + env[i] + "\n              value: " + env[i+1] + "\n"
		}
	}
	return out
}

func overlay(ns string, resources ...string) string {
	out := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: " + ns + "\nresources:\n  - ../../base\n"
	for _, r := range resources {
		out += "  - " + r + "\n"
	}
	return out
}

// threeEnvironments is a repository with a local, a staging and a
// production environment, each with one workload of its own.
func threeEnvironments(t *testing.T) string {
	return writeTree(t, map[string]string{
		".gitignore":                                  ".env.production\n",
		".env.example":                                "DATABASE_URL=postgresql://db.bookstore:5432/app\n",
		".env.production":                             "DATABASE_URL=postgresql://db.bookstore:5432/app\n",
		".env.staging":                                "CACHE_URL=redis://cache.bookstore-staging:6379/0\n",
		"deploy/base/kustomization.yaml":              "resources:\n  - api.yaml\n",
		"deploy/base/api.yaml":                        deployment("api", "", "ghcr.io/bookstore/api:1.0.0"),
		"deploy/overlays/local/kustomization.yaml":    overlay("bookstore-local", "mailhog.yaml"),
		"deploy/overlays/local/mailhog.yaml":          deployment("mailhog", "", "ghcr.io/bookstore/mailhog:1.0.0"),
		"deploy/overlays/staging/kustomization.yaml":  overlay("bookstore-staging", "seed.yaml"),
		"deploy/overlays/staging/seed.yaml":           deployment("staging-seed", "", "ghcr.io/bookstore/seed:1.0.0"),
		"deploy/overlays/prod/kustomization.yaml":     overlay("bookstore", "pooler.yaml"),
		"deploy/overlays/prod/pooler.yaml":            deployment("pooler", "", "ghcr.io/bookstore/pooler:1.0.0"),
		"charts/reports/values.yaml":                  "image:\n  repository: ghcr.io/bookstore/reports\n",
		"charts/reports/values-production.yaml":       "env:\n  REPORTS_DB_HOST: reports-db.bookstore\n",
		"charts/reports/values.staging.yaml":          "env:\n  REPORTS_DB_HOST: reports-db.bookstore-staging\n",
		"charts/reports/values-common.yaml":           "env:\n  LOG_LEVEL: info\n",
		"infra/environments/production/main.tf":       "resource \"hcloud_load_balancer\" \"bookstore\" {\n  name = \"bookstore-lb\"\n}\n",
		"infra/environments/staging/main.tf":          "resource \"hcloud_load_balancer\" \"staging\" {\n  name = \"staging-lb\"\n}\n",
		"node_modules/pkg/overlays/qa/something.yaml": "a: b\n",
	})
}

func TestEnvironments(t *testing.T) {
	got := Environments(threeEnvironments(t))
	want := []Environment{
		{Name: "local", Local: true, Paths: []string{"deploy/overlays/local/"}},
		{Name: "production", Paths: []string{"charts/reports/values-production.yaml", "deploy/overlays/prod/", "infra/environments/production/"}},
		{Name: "staging", Paths: []string{".env.staging", "charts/reports/values.staging.yaml", "deploy/overlays/staging/", "infra/environments/staging/"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("environments:\n got %+v\nwant %+v", got, want)
	}
	if again := Environments(threeEnvironments(t)); !reflect.DeepEqual(again, got) {
		t.Errorf("the order changes between runs: %+v", again)
	}
	if got := Environments(writeTree(t, map[string]string{"deploy/api.yaml": deployment("api", "shop", "ghcr.io/bookstore/api:1.0.0")})); len(got) != 0 {
		t.Errorf("a repository without environments lists %+v", got)
	}
}

func TestEnvironmentOf(t *testing.T) {
	files := map[string]string{
		"values-prod.yaml": "production", "values.staging.yml": "staging", "values-QA.yaml": "qa",
		".env.production": "production", ".env.production.local": "production", ".env.dev": "development",
		"values.yaml": "", "values-example.yaml": "", "electric.values.yaml": "", ".env": "", ".env.example": "", ".env.sample": "",
		"values-common.yaml": "", "main.tf": "",
	}
	for base, want := range files {
		if got, _ := environmentOfFile(base); got != want {
			t.Errorf("environmentOfFile(%q) = %q, want %q", base, got, want)
		}
	}
}

func ids(f *Findings) map[string]bool {
	out := map[string]bool{}
	for _, c := range f.Candidates {
		out[c.ID] = true
	}
	return out
}

func TestScanReadsOneEnvironment(t *testing.T) {
	root := threeEnvironments(t)
	cases := []struct {
		env         string
		has, hasNot []string
		notes       []string
	}{
		// None chosen: what it was before, every environment but the local
		// overlay, and a note that there is a choice.
		{"", []string{"api", "pooler", "staging-seed", "bookstore-lb", "staging-lb"}, []string{"mailhog"},
			[]string{"this repository has the environments local, production, staging", "skipped deploy/overlays/local"}},
		{"production", []string{"api", "pooler", "bookstore-lb", "reports"}, []string{"mailhog", "staging-seed", "staging-lb"},
			[]string{"left out the environment local (deploy/overlays/local/)", "left out the environment staging (.env.staging, charts/reports/values.staging.yaml, deploy/overlays/staging/, infra/environments/staging/)"}},
		{"prod", []string{"api", "pooler"}, []string{"mailhog", "staging-seed"}, []string{"only production is drafted"}},
		{"staging", []string{"api", "staging-seed", "staging-lb"}, []string{"mailhog", "pooler", "bookstore-lb"}, nil},
		// Asked for by name, the local overlay is read.
		{"local", []string{"api", "mailhog"}, []string{"pooler", "staging-seed"}, nil},
	}
	for _, c := range cases {
		f, err := Scan(Options{Root: root, Environment: c.env})
		if err != nil {
			t.Fatalf("environment %q: %v", c.env, err)
		}
		have := ids(f)
		for _, id := range c.has {
			if !have[id] {
				t.Errorf("environment %q: %s is missing; have %v", c.env, id, have)
			}
		}
		for _, id := range c.hasNot {
			if have[id] {
				t.Errorf("environment %q: %s belongs to another environment", c.env, id)
			}
		}
		notes := strings.Join(f.Notes, "\n")
		for _, n := range c.notes {
			if !strings.Contains(notes, n) {
				t.Errorf("environment %q: no note with %q in:\n%s", c.env, n, notes)
			}
		}
		if c.env != "" {
			if n := strings.Count(notes, "left out the environment"); n != 2 {
				t.Errorf("environment %q: %d notes for 2 environments left out:\n%s", c.env, n, notes)
			}
			if f.Environment != environmentName(c.env) || len(f.Environments) != 3 {
				t.Errorf("environment %q: findings say %q of %+v", c.env, f.Environment, f.Environments)
			}
		}
	}
	// The namespace is the one of the chosen overlay, without a conflict.
	f, _ := Scan(Options{Root: root, Environment: "staging"})
	if c := f.candidate("api"); c == nil || c.Namespace != "bookstore-staging" {
		t.Errorf("api in staging = %+v", c)
	}
}

func TestScanUnknownEnvironment(t *testing.T) {
	_, err := Scan(Options{Root: threeEnvironments(t), Environment: "qa"})
	if err == nil || !strings.Contains(err.Error(), `"qa"`) || !strings.Contains(err.Error(), "local, production, staging") {
		t.Errorf("error = %v, want one that lists the known environments", err)
	}
	_, err = Scan(Options{Root: writeTree(t, map[string]string{"deploy/api.yaml": deployment("api", "shop", "ghcr.io/bookstore/api:1.0.0")}), Environment: "production"})
	if err == nil || !strings.Contains(err.Error(), "no environments") {
		t.Errorf("error = %v, want one that says there are no environments", err)
	}
}
