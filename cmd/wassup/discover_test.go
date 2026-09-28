package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danilopopovikj/wassup/internal/model"
)

// productionRepo is the fixture of the discover package: a repository with
// credentials written in plain in its manifests, ConfigMaps and arguments.
const productionRepo = "../../internal/discover/testdata/production"

// productionSecrets are those credentials.
var productionSecrets = []string{"Pg-Pa55-7f3a", "Ch-Pa55-c0ffee", "Ch-Dsn-Pa55-9d41", "R3dis-Pa55-e2c1", "wk-live-5b1f77", "Adm1n", "44e0"}

// runDiscover runs `wassup discover` with the arguments and returns what
// it printed. The cluster is never read.
func runDiscover(t *testing.T, dir string, jsonOut bool, args ...string) string {
	t.Helper()
	saved := flags
	flags.dir, flags.jsonOut = dir, jsonOut
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	c := discoverCmd()
	c.SetArgs(append([]string{"--no-cluster", "--repo", productionRepo, "--name", "bookstore"}, args...))
	runErr := c.Execute()
	w.Close()
	os.Stdout = stdout
	flags = saved
	out := <-done
	if runErr != nil {
		t.Fatalf("discover %v: %v", args, runErr)
	}
	return out
}

func assertClean(t *testing.T, what, text string) {
	t.Helper()
	if text == "" {
		t.Errorf("%s is empty", what)
	}
	for _, s := range productionSecrets {
		if strings.Contains(text, s) {
			t.Errorf("%s holds the credential %q", what, s)
		}
	}
}

func TestDiscoverWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), model.DirName)
	out := runDiscover(t, dir, false, "--propose", "--write")
	assertClean(t, "the output of --write", out)
	for _, name := range []string{"topology.yaml", "bindings.yaml", "evidence.json"} {
		if _, err := os.Stat(filepath.Join(dir, "proposed", name)); err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
	}
	// every file that was written, whatever it is called
	entries, err := os.ReadDir(filepath.Join(dir, "proposed"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, "proposed", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		assertClean(t, e.Name(), string(b))
	}
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("--write should create .wassup/.gitignore: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if strings.Join(lines, "|") != "state/|proposed/|local.env" {
		t.Errorf(".gitignore = %q", b)
	}
	if _, err := model.Load(filepath.Join(dir, "proposed")); err != nil {
		t.Errorf("the written proposal does not validate: %v", err)
	}
	// an existing .gitignore keeps what it has
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("state/\nnotes.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Written    string `json:"written"`
		Components int    `json:"components"`
		Edges      int    `json:"edges"`
	}
	jsonOut := runDiscover(t, dir, true, "--write")
	assertClean(t, "the output of --write --json", jsonOut)
	if err := json.Unmarshal([]byte(jsonOut), &res); err != nil || res.Components == 0 || res.Edges == 0 || res.Written != filepath.Join(dir, "proposed") {
		t.Errorf("--write --json = %s (%v)", jsonOut, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(b) != "state/\nnotes.md\nproposed/\nlocal.env\n" {
		t.Errorf(".gitignore = %q", b)
	}
}

func TestDiscoverPrintsNoCredential(t *testing.T) {
	dir := filepath.Join(t.TempDir(), model.DirName)
	for name, args := range map[string][]string{"--json": nil, "--propose --json": {"--propose"}} {
		out := runDiscover(t, dir, true, args...)
		assertClean(t, name, out)
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Errorf("%s does not print JSON: %v", name, err)
		}
	}
	assertClean(t, "the findings as text", runDiscover(t, dir, false))
	assertClean(t, "the proposal as text", runDiscover(t, dir, false, "--propose"))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("without --write nothing is written: %v", err)
	}
}
