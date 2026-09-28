package discover

import (
	"path/filepath"
	"strings"
	"testing"
)

const apiManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: NAME
  namespace: bookstore
spec:
  selector:
    matchLabels:
      app: NAME
  template:
    metadata:
      labels:
        app: NAME
    spec:
      containers:
        - name: app
          image: ghcr.io/bookstore/NAME:1
`

func manifest(name string) string { return strings.ReplaceAll(apiManifest, "NAME", name) }

// TestScanSkipsWhatIsNotTheSystem builds the repository the bugs came
// from: agent worktrees, a lockfile, a test, a local .env, a local overlay.
func TestScanSkipsWhatIsNotTheSystem(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, root, ".git/info/exclude", "scratch/\n")
	writeFile(t, root, ".gitignore", "# local settings\n.env\n/generated/\n*.rendered.yaml\n!keep.rendered.yaml\n")
	writeFile(t, root, "deploy/api.yaml", manifest("api"))

	// three worktrees of the same repository, and a checkout somewhere else
	for _, w := range []string{"a", "b", "c"} {
		writeFile(t, root, ".claude/worktrees/"+w+"/.git", "gitdir: ../../../.git/worktrees/"+w+"\n")
		writeFile(t, root, ".claude/worktrees/"+w+"/deploy/api.yaml", manifest("api"))
		writeFile(t, root, ".claude/worktrees/"+w+"/deploy/ghost.yaml", manifest("ghost-"+w))
	}
	writeFile(t, root, "third-party/charts/.git", "gitdir: ../../.git/modules/charts\n")
	writeFile(t, root, "third-party/charts/deploy.yaml", manifest("submodule-app"))

	// a lockfile: "queue" is a package, 6.0.2 its version
	writeFile(t, root, "apps/web/package-lock.json", `{"packages": {"node_modules/queue": {"version": "6.0.2"}}, "dependencies": {"queue": "6.0.2"}}`)
	writeFile(t, root, "apps/web/package.json", `{"dependencies": {"queue": "6.0.2"}}`)
	writeFile(t, root, "apps/web/yarn.lock", "queue@6.0.2:\n  resolved \"https://api.lockfile-host.com/queue\"\n")
	writeFile(t, root, "go.sum", "example.com/mod v1.0.0 h1:https://api.gosum-host.com/\n")

	// tests
	writeFile(t, root, "apps/api/tests/test_client.py", "BASE = \"https://api.example.com/v1\"\nOTHER = \"https://api.only-in-tests.com/v1\"\n")
	writeFile(t, root, "apps/api/app/client_test.py", "URL = \"https://api.only-in-tests.com/v1\"\n")
	writeFile(t, root, "apps/web/src/client.spec.ts", "const url = \"https://api.only-in-tests.com/v1\"\n")
	writeFile(t, root, "apps/api/conftest.py", "URL = \"https://api.only-in-tests.com/v1\"\n")
	writeFile(t, root, "cmd/api/main_test.go", "package main\n// https://api.only-in-tests.com/v1\n")
	writeFile(t, root, "apps/api/testdata/deploy.yaml", manifest("from-testdata"))
	// a sample domain in real code is still not a dependency
	writeFile(t, root, "apps/api/app/client.py", "DOCS = \"https://api.example.com/v1\"\nPAY = \"https://api.stripe.com/v1\"\n")

	// local settings: ignored by git, and a tunnel in a file that is not
	writeFile(t, root, ".env", "WEBHOOK_URL=https://api.ignored-env-host.com/hook\n")
	writeFile(t, root, ".env.staging", "WEBHOOK_URL=https://bookstore-dev.ngrok-free.app/hook\nTUNNEL=https://abc.loca.lt\nOTHER=https://x.localhost.run\nDB=postgresql://app:pw@localhost:5432/app\n")

	// what git ignores
	writeFile(t, root, "generated/deploy.yaml", manifest("generated-app"))
	writeFile(t, root, "scratch/deploy.yaml", manifest("scratch-app"))
	writeFile(t, root, "deploy/api.rendered.yaml", manifest("rendered-app"))
	writeFile(t, root, "deploy/keep.rendered.yaml", manifest("kept-app"))
	writeFile(t, root, "deploy/nested/.gitignore", "secret-*.yaml\n")
	writeFile(t, root, "deploy/nested/secret-app.yaml", manifest("nested-ignored"))
	writeFile(t, root, "deploy/nested/worker.yaml", manifest("worker"))
	writeFile(t, root, "deploy/secret-app.yaml", manifest("not-ignored-here"))

	// overlays
	for _, o := range []string{"local", "dev", "development"} {
		writeFile(t, root, "deploy/overlays/"+o+"/kustomization.yaml", "namespace: bookstore-"+o+"\nresources:\n  - app.yaml\n")
		writeFile(t, root, "deploy/overlays/"+o+"/app.yaml", manifest("only-"+o))
	}
	writeFile(t, root, "deploy/overlays/production/kustomization.yaml", "namespace: bookstore\nresources:\n  - app.yaml\n")
	writeFile(t, root, "deploy/overlays/production/app.yaml", manifest("only-production"))
	writeFile(t, root, "envs/dev/kustomization.yaml", "resources:\n  - app.yaml\n")
	writeFile(t, root, "envs/dev/app.yaml", manifest("only-envs-dev"))
	// a directory called dev that is not an overlay is read
	writeFile(t, root, "tools/dev/deploy.yaml", manifest("dev-tools"))

	f, err := Scan(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	p := Propose(f, ProposeOptions{Name: "bookstore"})
	have := map[string]Candidate{}
	for _, c := range f.Candidates {
		have[c.ID] = c
	}
	for _, id := range []string{"api", "worker", "kept-app", "not-ignored-here", "only-production", "dev-tools", "stripe"} {
		if _, ok := have[id]; !ok {
			t.Errorf("%s should have been found", id)
		}
	}
	for _, id := range []string{"ghost-a", "ghost-b", "ghost-c", "submodule-app", "from-testdata", "generated-app", "scratch-app", "rendered-app", "nested-ignored",
		"only-local", "only-dev", "only-development", "only-envs-dev",
		"6-0-2-queue", "example", "only-in-tests", "lockfile-host", "gosum-host", "ignored-env-host", "ngrok-free", "loca", "localhost"} {
		if c, ok := have[id]; ok {
			t.Errorf("%s should not be a candidate: %v", id, c.Evidence)
		}
	}
	for _, c := range f.Candidates {
		if c.Type == "queue" {
			t.Errorf("a version in a dependency list became the queue %q: %v", c.ID, c.Evidence)
		}
		for _, a := range c.Addresses {
			if isDevHost(a) || isReservedHost(a) {
				t.Errorf("%s answers to %s", c.ID, a)
			}
		}
	}
	// every finding once: the worktrees hold the same manifest three times
	if n := len(have["api"].Evidence); n != 1 {
		t.Errorf("api has %d pieces of evidence, want the one manifest: %v", n, have["api"].Evidence)
	}
	for _, l := range append(append([]Link{}, f.Links...), p.Unresolved...) {
		if l.Host != "" && !usableHost(l.Host) {
			t.Errorf("a link to %s", l.Host)
		}
	}
	// what was skipped is said
	for _, dir := range []string{"deploy/overlays/local", "deploy/overlays/dev", "deploy/overlays/development", "envs/dev", "third-party/charts"} {
		if !notesContain(f.Notes, "skipped "+dir+":") {
			t.Errorf("the notes should say that %s was skipped: %v", dir, f.Notes)
		}
	}
	if notesContain(f.Notes, "tools/dev") || notesContain(f.Notes, "overlays/production") {
		t.Errorf("only local overlays are skipped: %v", f.Notes)
	}
}

func TestSkipByName(t *testing.T) {
	for _, n := range []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "poetry.lock", "uv.lock", "Pipfile.lock", "Cargo.lock", "go.sum", "composer.lock", "Gemfile.lock", "npm-shrinkwrap.json", "flake.lock", ".terraform.lock.hcl"} {
		if !isLockfile(n) {
			t.Errorf("%s is a lockfile", n)
		}
	}
	for _, n := range []string{"package.json", "values.yaml", "lock.go", "main.tf", "Chart.yaml"} {
		if isLockfile(n) {
			t.Errorf("%s is not a lockfile", n)
		}
	}
	for _, n := range []string{"api_test.go", "api.test.ts", "api.spec.ts", "api.test.tsx", "api.spec.js", "test_api.py", "api_test.py", "conftest.py", "api_spec.rb", "ApiTest.java"} {
		if !isTestFile(n) {
			t.Errorf("%s is a test", n)
		}
	}
	for _, n := range []string{"api.go", "api.ts", "testing.py", "contest.py", "latest.yaml", "attest.go", "spec.ts"} {
		if isTestFile(n) {
			t.Errorf("%s is not a test", n)
		}
	}
	for _, d := range []string{"tests", "test", "__tests__", "testdata", "e2e", "fixtures"} {
		if !testDirs[d] {
			t.Errorf("%s holds tests", d)
		}
	}
}

func TestIgnorePatterns(t *testing.T) {
	root := filepath.FromSlash("/repo")
	rules := `
# a comment, then a blank line

*.log
!important.log
/build/
docs/*.pdf
**/cache/
logs/**
src/**/generated/*.go
?.tmp
\#notes
name with space 
[ab].txt
vendor
`
	s := &ignoreSet{}
	for _, line := range strings.Split(rules, "\n") {
		if r, ok := parseIgnoreLine(line, root); ok {
			s.rules = append(s.rules, r)
		}
	}
	if len(s.rules) != 12 {
		t.Fatalf("%d rules read, want 12", len(s.rules))
	}
	cases := []struct {
		path string
		dir  bool
		want bool
	}{
		{"app.log", false, true},
		{"deep/down/app.log", false, true},
		{"important.log", false, false}, // negated
		{"deep/important.log", false, false},
		{"build", true, true},
		{"build", false, false},    // the pattern names a directory
		{"src/build", true, false}, // anchored at the top
		{"docs/a.pdf", false, true},
		{"docs/sub/a.pdf", false, false},   // a star stops at a slash
		{"other/docs/a.pdf", false, false}, // a slash in the middle anchors
		{"cache", true, true},
		{"a/b/cache", true, true},
		{"a/b/cache", false, false},
		{"logs/x", false, true},
		{"logs/x/y/z", false, true},
		{"src/generated/a.go", false, true}, // ** is also no directory at all
		{"src/a/b/generated/a.go", false, true},
		{"src/a/b/generated/z.txt", false, false},
		{"a.tmp", false, true},
		{"ab.tmp", false, false},
		{"#notes", false, true},
		{"name with space", false, true},
		{"a.txt", false, true},
		{"c.txt", false, false},
		{"vendor", true, true},
		{"x/vendor", false, true},
		{"vendors", true, false},
	}
	for _, c := range cases {
		if got := s.ignored(filepath.Join(root, filepath.FromSlash(c.path)), c.dir); got != c.want {
			t.Errorf("ignored(%q, dir=%v) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
	// a nested file rules its own directory only
	nested, _ := parseIgnoreLine("*.yaml", filepath.Join(root, "deploy"))
	s.rules = append(s.rules, nested)
	if !s.ignored(filepath.Join(root, "deploy", "a.yaml"), false) || s.ignored(filepath.Join(root, "other", "a.yaml"), false) {
		t.Error("the rules of deploy/.gitignore apply below deploy/ and nowhere else")
	}
}
