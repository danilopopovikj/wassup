package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// releaseBase is where the releases are. A test points it at a server of
// its own.
var releaseBase = "https://github.com/danilopopovikj/wassup"

// maxDownload bounds what update reads from the network: an archive is a
// few tens of megabytes, and nothing that is not one is kept in memory.
const maxDownload = 200 << 20

// updateResult is what `wassup update` did, or would do with --check.
type updateResult struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Updated bool   `json:"updated"`
	Path    string `json:"path,omitempty"`
}

// updater replaces a wassup binary with a release. It does what install.sh
// does, from inside the binary: the archive for this machine, checked
// against checksums.txt, put where the binary already is.
type updater struct {
	client *http.Client
	base   string
	goos   string
	goarch string
}

func newUpdater() *updater {
	return &updater{
		// Read only, as every client of wassup is: a release is two GETs.
		client: &http.Client{Timeout: 2 * time.Minute, Transport: probe.ReadOnly(nil)},
		base:   releaseBase,
		goos:   runtime.GOOS,
		goarch: runtime.GOARCH,
	}
}

// latest returns the tag of the latest release, such as v0.3.1. The release
// page redirects to its tag, which costs no API call and needs no token.
func (u *updater) latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	c := *u.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("the latest release could not be read: %w", err)
	}
	defer resp.Body.Close()
	tag := path.Base(resp.Header.Get("Location"))
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("no release was found at %s/releases; install from source with: go install github.com/danilopopovikj/wassup/cmd/wassup@latest", u.base)
	}
	return tag, nil
}

// get reads one file of a release.
func (u *updater) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("could not download %s: %w", url, err)
	}
	if len(b) > maxDownload {
		return nil, fmt.Errorf("%s is larger than a release archive; nothing was installed", url)
	}
	return b, nil
}

// binary downloads the release tag for this machine, checks it against
// checksums.txt and returns the wassup binary inside the archive.
func (u *updater) binary(ctx context.Context, tag string) ([]byte, error) {
	archive := fmt.Sprintf("wassup_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), u.goos, u.goarch)
	dl := u.base + "/releases/download/" + tag + "/"
	sums, err := u.get(ctx, dl+"checksums.txt")
	if err != nil {
		return nil, err
	}
	var want string
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == archive {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("release %s has no archive for %s/%s; install from source with: go install github.com/danilopopovikj/wassup/cmd/wassup@%s", tag, u.goos, u.goarch, tag)
	}
	data, err := u.get(ctx, dl+archive)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("the checksum of %s does not match checksums.txt; nothing was installed", archive)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", archive, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s holds no wassup binary", archive)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", archive, err)
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == "wassup" {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

// replace puts the new binary where the old one is. It is written beside
// it and renamed over it, so a wassup that is running keeps the file it
// has open and nobody ever starts half a binary.
func replace(exe string, bin []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".wassup-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s is in a directory you may not write to; install wassup again the way you installed it", exe)
		}
		return err
	}
	defer os.Remove(tmp.Name()) // nothing is left behind when a step fails
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), exe)
}

// sameVersion reports whether the running binary is the release tag. A
// release prints 0.3.1, its tag is v0.3.1, and `go install` records v0.3.1.
func sameVersion(current, tag string) bool {
	return strings.TrimPrefix(current, "v") == strings.TrimPrefix(tag, "v")
}

// run updates the binary at exe from current to want, or to the latest
// release when want is empty. With check it only says what it would do.
func (u *updater) run(ctx context.Context, current, want, exe string, check bool) (updateResult, error) {
	res := updateResult{Current: current, Latest: want}
	if want == "" {
		tag, err := u.latest(ctx)
		if err != nil {
			return res, err
		}
		res.Latest = tag
	} else if !strings.HasPrefix(want, "v") {
		res.Latest = "v" + want
	}
	if check || sameVersion(current, res.Latest) {
		return res, nil
	}
	bin, err := u.binary(ctx, res.Latest)
	if err != nil {
		return res, err
	}
	if err := replace(exe, bin); err != nil {
		return res, err
	}
	res.Updated, res.Path = true, exe
	return res, nil
}

func updateCmd() *cobra.Command {
	var check bool
	var want string
	c := &cobra.Command{
		Use:   "update",
		Short: "replace this binary with the latest release",
		Long: "Downloads the release for this machine, checks it against checksums.txt\n" +
			"and puts it where this binary is. --check only says whether there is a\n" +
			"newer release.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if p, err := filepath.EvalSymlinks(exe); err == nil {
				exe = p
			}
			res, err := newUpdater().run(cmd.Context(), version(), want, exe, check)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return printJSON(res)
			}
			switch {
			case res.Updated:
				fmt.Printf("updated %s from %s to %s\n", res.Path, res.Current, res.Latest)
				// The skill in a repository is a copy made by the binary
				// that installed it; the new binary has the new one.
				if _, err := os.Stat(filepath.Join(".claude", "skills", "wassup")); err == nil {
					fmt.Println("the skill in .claude/skills/wassup is a copy: run `wassup skill install` to refresh it")
				}
			case sameVersion(res.Current, res.Latest):
				fmt.Printf("wassup %s is the latest release\n", res.Current)
			default:
				fmt.Printf("wassup %s is installed, %s is the latest release: run `wassup update`\n", res.Current, res.Latest)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "say whether there is a newer release and change nothing")
	c.Flags().StringVar(&want, "version", "", "the release to install (default: the latest)")
	return c
}
