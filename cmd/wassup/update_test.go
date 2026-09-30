package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// releaseServer serves one release the way GitHub does: /releases/latest
// redirects to the tag, and the tag has an archive and checksums.txt.
// sumOf says which checksum checksums.txt lists for the archive.
func releaseServer(t *testing.T, tag string, bin []byte, sumOf func(archive []byte) string) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string][]byte{"README.md": []byte("read me"), "wassup": bin} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	archive := "wassup_" + strings.TrimPrefix(tag, "v") + "_linux_arm64.tar.gz"
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0000  wassup_other.tar.gz\n" + sumOf(buf.Bytes()) + "  " + archive + "\n"))
	})
	mux.HandleFunc("/releases/download/"+tag+"/"+archive, func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func testUpdater(srv *httptest.Server, goarch string) *updater {
	return &updater{client: &http.Client{Transport: probe.ReadOnly(nil)}, base: srv.URL, goos: "linux", goarch: goarch}
}

// installed writes a stand-in for the binary that is installed.
func installed(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "wassup")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestUpdateReplacesTheBinaryWithTheLatestRelease(t *testing.T) {
	srv := releaseServer(t, "v0.2.0", []byte("new"), sha)
	exe := installed(t)
	res, err := testUpdater(srv, "arm64").run(context.Background(), "0.1.0", "", exe, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Latest != "v0.2.0" || res.Path != exe {
		t.Fatalf("result %+v", res)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "new" {
		t.Fatalf("the binary holds %q, want the release", got)
	}
	if st, _ := os.Stat(exe); st.Mode().Perm() != 0o755 {
		t.Errorf("the binary has mode %v, want one that runs", st.Mode().Perm())
	}
	left, _ := os.ReadDir(filepath.Dir(exe))
	if len(left) != 1 {
		t.Errorf("%d files beside the binary, want none left behind", len(left)-1)
	}
}

// What is installed already is left alone, and so is everything with
// --check.
func TestUpdateChangesNothingWhenThereIsNothingToDo(t *testing.T) {
	srv := releaseServer(t, "v0.2.0", []byte("new"), sha)
	for name, tc := range map[string]struct {
		current string
		check   bool
	}{
		"the latest, as a release prints it":  {"0.2.0", false},
		"the latest, as go install names it":  {"v0.2.0", false},
		"an older one with --check":           {"0.1.0", true},
		"a build from source, with --check":   {"dev-0123456789ab", true},
		"a version between two, with --check": {"v0.1.1-0.20260930101010-0123456789ab", true},
	} {
		exe := installed(t)
		res, err := testUpdater(srv, "arm64").run(context.Background(), tc.current, "", exe, tc.check)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Updated || res.Latest != "v0.2.0" {
			t.Errorf("%s: result %+v", name, res)
		}
		if got, _ := os.ReadFile(exe); string(got) != "old" {
			t.Errorf("%s: the binary was replaced", name)
		}
	}
}

// A download that is not what checksums.txt says, a machine the release
// was not built for and a release that does not exist install nothing.
func TestUpdateInstallsNothingItCannotCheck(t *testing.T) {
	ctx := context.Background()
	bad := releaseServer(t, "v0.2.0", []byte("new"), func([]byte) string { return sha([]byte("something else")) })
	good := releaseServer(t, "v0.2.0", []byte("new"), sha)
	none := httptest.NewServer(http.NotFoundHandler())
	defer none.Close()
	for name, tc := range map[string]struct {
		u    *updater
		want string
		says string
	}{
		"a wrong checksum":    {testUpdater(bad, "arm64"), "", "does not match checksums.txt"},
		"another machine":     {testUpdater(good, "riscv64"), "", "no archive for linux/riscv64"},
		"no release at all":   {testUpdater(none, "arm64"), "", "no release was found"},
		"a version not there": {testUpdater(good, "arm64"), "0.9.0", "could not download"},
	} {
		exe := installed(t)
		res, err := tc.u.run(ctx, "0.1.0", tc.want, exe, false)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: err %v, want one that says %q", name, err, tc.says)
		}
		if res.Updated {
			t.Errorf("%s: reported an update", name)
		}
		if got, _ := os.ReadFile(exe); string(got) != "old" {
			t.Errorf("%s: the binary was replaced", name)
		}
	}
}

// --version installs the release that was asked for, with or without the v.
func TestUpdateToAVersion(t *testing.T) {
	srv := releaseServer(t, "v0.2.0", []byte("new"), sha)
	exe := installed(t)
	res, err := testUpdater(srv, "arm64").run(context.Background(), "0.3.0", "0.2.0", exe, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); !res.Updated || res.Latest != "v0.2.0" || string(got) != "new" {
		t.Fatalf("result %+v, binary %q", res, got)
	}
}
