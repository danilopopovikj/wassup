// Package demo embeds the recorded scenarios so `wassup demo` runs with no
// cluster. The files are copies of testdata/scenarios written by gen.py.
package demo

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed all:data
var data embed.FS

// List returns the embedded scenario directory names, sorted.
func List() []string {
	entries, err := data.ReadDir("data")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Extract copies one scenario into a fresh temp directory and returns it.
// The selector may be a number ("2"), a prefix ("02") or a slug fragment.
func Extract(selector string) (string, error) {
	names := List()
	if len(names) == 0 {
		return "", fmt.Errorf("no embedded scenarios")
	}
	pick := ""
	sel := strings.TrimSpace(selector)
	if sel == "" {
		sel = "02"
	}
	if len(sel) == 1 {
		sel = "0" + sel
	}
	for _, n := range names {
		if strings.HasPrefix(n, sel) || strings.Contains(n, sel) {
			pick = n
			break
		}
	}
	if pick == "" {
		return "", fmt.Errorf("no scenario matches %q; have %s", selector, strings.Join(names, ", "))
	}
	dir, err := os.MkdirTemp("", "wassup-demo-")
	if err != nil {
		return "", err
	}
	src := "data/" + pick
	err = fs.WalkDir(data, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, src)
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := data.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
