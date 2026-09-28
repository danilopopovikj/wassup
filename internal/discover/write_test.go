package discover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danilopopovikj/wassup/internal/model"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestEnsureGitignore(t *testing.T) {
	// missing, in a directory that does not exist yet
	dir := filepath.Join(t.TempDir(), model.DirName)
	if err := EnsureGitignore(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".gitignore")
	if got := strings.Join(readLines(t, path), "|"); got != "state/|proposed/|local.env" {
		t.Errorf("a new .gitignore = %q", got)
	}
	// again: nothing changes
	before, _ := os.ReadFile(path)
	if err := EnsureGitignore(dir); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("a second call changed the file: %q", after)
	}

	// one of the two is missing, the rest is the user's
	own := "# ours\n*.bak\n\nstate/\nlocal.env\nnotes.md"
	if err := os.WriteFile(path, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureGitignore(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != own+"\nproposed/\n" {
		t.Errorf("the file should keep what it had and gain proposed/: %q", b)
	}

	// other spellings of the same directory count
	for _, have := range []string{"/state/\n/proposed/\n/local.env\n", "state\nproposed\nlocal.env\n", "  state/  \r\nproposed/\r\nlocal.env\r\n"} {
		if err := os.WriteFile(path, []byte(have), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := EnsureGitignore(dir); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(path); string(b) != have {
			t.Errorf("%q already ignores all of them, got %q", have, b)
		}
	}
	// a line that only looks like it does not
	if err := os.WriteFile(path, []byte("state/annotations.json\n#proposed/\nlocal.env.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureGitignore(dir); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(readLines(t, path), "|"); got != "state/annotations.json|#proposed/|local.env.example|state/|proposed/|local.env" {
		t.Errorf("the three lines should be appended: %q", got)
	}
}

func TestWriteProposal(t *testing.T) {
	f, p := scanFixture(t)
	dir := filepath.Join(t.TempDir(), model.DirName)
	if err := WriteProposal(dir, p, f); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"topology.yaml", "bindings.yaml", "evidence.json"} {
		if fi, err := os.Stat(filepath.Join(dir, ProposedDir, name)); err != nil || fi.Size() == 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := strings.Join(readLines(t, filepath.Join(dir, ".gitignore")), "|"); got != "state/|proposed/|local.env" {
		t.Errorf("writing a proposal should make git ignore it: %q", got)
	}
	// what was written passes the schemas, the id pattern among them
	cfg, err := model.Load(filepath.Join(dir, ProposedDir))
	if err != nil {
		t.Fatalf("the written proposal does not load: %v", err)
	}
	if len(cfg.Topology.Components) != len(p.Topology.Components) {
		t.Errorf("%d components written, %d proposed", len(cfg.Topology.Components), len(p.Topology.Components))
	}
	// and so does the one of a repository with Ingress paths, placeholders
	// and a live cluster in it
	pf := scanProduction(t)
	AddCluster(pf, liveInventory())
	other := filepath.Join(t.TempDir(), model.DirName)
	if err := WriteProposal(other, Propose(pf, ProposeOptions{Name: "bookstore", Namespace: "default"}), pf); err != nil {
		t.Fatal(err)
	}
	if _, err := model.Load(filepath.Join(other, ProposedDir)); err != nil {
		t.Errorf("the written proposal does not load: %v", err)
	}
	// nothing else lands in .wassup/
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != ".gitignore proposed" {
		t.Errorf("discover must not touch the rest of .wassup/: %v", names)
	}
}
