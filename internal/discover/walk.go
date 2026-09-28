package discover

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Which files are evidence. The scanners read text for patterns, so a file
// that does not describe the running system produces components that do not
// exist: a lockfile's version string, a host in a test, the settings of a
// local overlay, a second checkout of the same repository.

// testDirs are directories that hold tests and their data.
var testDirs = map[string]bool{"tests": true, "test": true, "__tests__": true, "testdata": true, "e2e": true, "fixtures": true,
	"__fixtures__": true, "__mocks__": true, "__snapshots__": true, "cypress": true, "playwright": true}

// lockfiles are named here when their name does not end in .lock,
// -lock.json or -lock.yaml.
var lockfiles = map[string]bool{"go.sum": true, "go.work.sum": true, "npm-shrinkwrap.json": true, "bun.lockb": true,
	".terraform.lock.hcl": true, "packages.lock.json": true}

// isLockfile reports whether a file pins dependency versions.
func isLockfile(base string) bool {
	if lockfiles[base] {
		return true
	}
	for _, suffix := range []string{".lock", "-lock.json", "-lock.yaml", "-lock.yml"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// isTestFile reports whether a file is a test by its name.
func isTestFile(base string) bool {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	switch {
	case base == "conftest.py":
		return true
	case strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec") || strings.HasSuffix(stem, ".e2e"):
		return true // api.test.ts, api.spec.tsx
	case strings.HasSuffix(stem, "_test") || strings.HasSuffix(stem, "_spec"):
		return true // api_test.go, api_test.py, api_spec.rb
	case ext == ".py" && strings.HasPrefix(base, "test_"):
		return true
	case (ext == ".java" || ext == ".kt" || ext == ".cs") && (strings.HasSuffix(stem, "Test") || strings.HasSuffix(stem, "Tests")):
		return true
	}
	return false
}

// localOverlays are the names of Kustomize overlays meant for a developer's
// machine.
var localOverlays = map[string]bool{"local": true, "dev": true, "development": true, "localhost": true, "local-dev": true}

// isLocalOverlay reports whether dir is a Kustomize overlay for local
// development: named like one, and either under overlays/ or holding a
// kustomization file.
func isLocalOverlay(dir string) bool {
	if !localOverlays[strings.ToLower(filepath.Base(dir))] {
		return false
	}
	if filepath.Base(filepath.Dir(dir)) == "overlays" {
		return true
	}
	return kustomizationIn(dir) != ""
}

// kustomizationIn returns the kustomization file of a directory, or "".
func kustomizationIn(dir string) string {
	for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if fi, err := os.Stat(filepath.Join(dir, n)); err == nil && !fi.IsDir() {
			return filepath.Join(dir, n)
		}
	}
	return ""
}

// isKustomization reports whether a file name is a kustomization file.
func isKustomization(base string) bool {
	return base == "kustomization.yaml" || base == "kustomization.yml" || base == "Kustomization"
}

// walkTree visits what belongs to the repository under root: it leaves out
// the skipped directories, tests, lockfiles, what git ignores and nested
// checkouts. It is the one set of rules for every walk, so that the list of
// environments and the scan never disagree on what the repository holds.
// dir decides whether a directory is entered; file returns false to end the
// walk.
func walkTree(root string, skip map[string]bool, note func(format string, args ...any), dir func(path, rel string) bool, file func(path, rel string) bool) error {
	ign := newIgnoreSet(root)
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p == root {
				return nil
			}
			name := d.Name()
			switch {
			case skip[name] || testDirs[name]:
				return filepath.SkipDir
			case ign.ignored(p, true):
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				note("skipped %s: a nested git checkout (worktree or submodule), not part of this repository", rel)
				return filepath.SkipDir
			}
			if !dir(p, rel) {
				return filepath.SkipDir
			}
			ign.load(filepath.Join(p, ".gitignore"), p)
			return nil
		}
		base := d.Name()
		if isLockfile(base) || isTestFile(base) || ign.ignored(p, false) {
			return nil
		}
		if !file(p, rel) {
			return filepath.SkipAll
		}
		return nil
	})
}

// collectFiles walks root and returns the files worth reading, sorted. It
// leaves out what walkTree leaves out and, of the environments the
// repository has, all but one: the one named by env, or, when env is empty,
// every one that is not a local overlay. Skipping a checkout or an overlay
// is said in the notes, since the user may have expected it to be read; the
// environments left out by name are noted by the caller.
func collectFiles(root string, skip map[string]bool, maxFiles int, env string, note func(format string, args ...any)) ([]string, error) {
	var files []string
	err := walkTree(root, skip, note, func(p, rel string) bool {
		if env != "" {
			name, ok := environmentOfDir(p)
			return !ok || name == env
		}
		if isLocalOverlay(p) {
			note("skipped %s: a Kustomize overlay for local development, its hosts and settings are not the running system", rel)
			return false
		}
		return true
	}, func(p, rel string) bool {
		if env != "" {
			if name, ok := environmentOfFile(filepath.Base(p)); ok && name != env {
				return true
			}
		}
		if len(files) >= maxFiles {
			return false
		}
		files = append(files, p)
		return true
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
