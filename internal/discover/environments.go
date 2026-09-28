package discover

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// A repository usually describes the same system several times: once per
// environment. Read together, a local overlay, staging and production give
// a diagram with three of everything, and the local one brings hosts that
// exist on nobody's cluster. Discovery therefore lists the environments it
// can see and drafts one of them.

// Environment is one environment a repository deploys to, with the files
// and directories that describe it and nothing else.
type Environment struct {
	Name string `json:"name"`
	// Paths are the directories and files of the environment, relative to
	// the root of the scan, with forward slashes.
	Paths []string `json:"paths"`
	// Local is set for an environment meant for a developer's machine.
	Local bool `json:"local,omitempty"`
}

// environmentDirs are the directories whose children are environments: the
// overlays of Kustomize and the environment directories of Terraform.
var environmentDirs = map[string]bool{"overlays": true, "environments": true, "envs": true}

// environmentNames are the names a directory with a kustomization of its
// own is taken for an environment by (deploy/production next to
// deploy/base), besides the local ones.
var environmentNames = map[string]bool{"production": true, "prod": true, "live": true, "staging": true, "stage": true, "stg": true,
	"qa": true, "uat": true, "preprod": true, "preview": true, "sandbox": true}

// environmentAliases are the short forms of a name. values-prod.yaml and
// overlays/production are one environment, not two.
var environmentAliases = map[string]string{"prod": "production", "stage": "staging", "stg": "staging", "dev": "development"}

// notEnvironments are the words that follow "values-" or ".env." in a file
// that is a sample or shared by every environment.
var notEnvironments = map[string]bool{"example": true, "sample": true, "template": true, "tmpl": true, "dist": true, "default": true,
	"defaults": true, "schema": true, "base": true, "common": true, "shared": true, "global": true}

// environmentName is the name an environment goes by, whatever the
// spelling: lowercase, the short forms written out.
func environmentName(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	if long, ok := environmentAliases[name]; ok {
		return long
	}
	return name
}

// localEnvironment reports whether a name, as it is written in the
// repository, is one of a developer's machine.
func localEnvironment(raw string) bool {
	raw = strings.ToLower(raw)
	return localOverlays[raw] || localOverlays[environmentName(raw)]
}

// environmentOfDir returns the environment a directory describes: a child
// of overlays/, environments/ or envs/, or a directory named like an
// environment that holds a kustomization.
func environmentOfDir(dir string) (string, bool) {
	base := strings.ToLower(filepath.Base(dir))
	switch {
	case environmentDirs[strings.ToLower(filepath.Base(filepath.Dir(dir)))]:
	case (environmentNames[base] || localOverlays[base]) && kustomizationIn(dir) != "":
	default:
		return "", false
	}
	return environmentName(base), true
}

// environmentOfFile returns the environment a file belongs to by its name:
// values-<name>.yaml and values.<name>.yaml of a chart, .env.<name>.
func environmentOfFile(base string) (string, bool) {
	lower := strings.ToLower(base)
	name := ""
	switch ext := filepath.Ext(lower); {
	case strings.HasPrefix(lower, ".env."):
		// .env.production.local is production, on somebody's machine
		name, _, _ = strings.Cut(strings.TrimPrefix(lower, ".env."), ".")
	case ext == ".yaml" || ext == ".yml":
		stem := strings.TrimSuffix(lower, ext)
		for _, prefix := range []string{"values-", "values."} {
			if rest, ok := strings.CutPrefix(stem, prefix); ok {
				name = rest
				break
			}
		}
	}
	if name == "" || notEnvironments[name] {
		return "", false
	}
	return environmentName(name), true
}

// Environments lists the environments of the repository under root, by
// name: Kustomize overlays (overlays/<name>), the values files of a chart
// (values-<name>.yaml, values.<name>.yaml), Terraform environment
// directories (environments/<name>, envs/<name>) and .env.<name> files. It
// follows the rules of the scan, so what git ignores is not listed. It
// returns nothing when root cannot be read.
func Environments(root string) []Environment {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	skip := map[string]bool{}
	for _, s := range defaultSkip {
		skip[s] = true
	}
	return environments(abs, skip)
}

// environments is Environments for a scan that has its own skip list.
func environments(root string, skip map[string]bool) []Environment {
	byName := map[string]*Environment{}
	add := func(name, raw, rel string) {
		e := byName[name]
		if e == nil {
			e = &Environment{Name: name}
			byName[name] = e
		}
		e.Paths = append(e.Paths, rel)
		e.Local = e.Local || localEnvironment(raw)
	}
	_ = walkTree(root, skip, func(string, ...any) {}, func(p, rel string) bool {
		if name, ok := environmentOfDir(p); ok {
			add(name, filepath.Base(p), rel+"/")
			return false // what is below belongs to it
		}
		return true
	}, func(p, rel string) bool {
		base := filepath.Base(p)
		if name, ok := environmentOfFile(base); ok {
			raw := name
			if strings.HasPrefix(strings.ToLower(base), ".env.") && strings.Contains(strings.ToLower(base), ".local") {
				raw = "local"
			}
			add(name, raw, rel)
		}
		return true
	})
	out := make([]Environment, 0, len(byName))
	for _, e := range byName {
		sort.Strings(e.Paths)
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// environmentNamesOf lists the names of environments, for a message.
func environmentNamesOf(envs []Environment) string {
	names := make([]string, 0, len(envs))
	for _, e := range envs {
		names = append(names, e.Name)
	}
	return strings.Join(names, ", ")
}

// chooseEnvironment checks the environment a scan was asked for against
// the ones the repository has and returns its name as the repository
// spells it. An unknown one is an error that lists the known ones: drafting
// everything instead would be the noise the choice was meant to remove.
func chooseEnvironment(envs []Environment, want string) (string, error) {
	if strings.TrimSpace(want) == "" {
		return "", nil
	}
	name := environmentName(want)
	for _, e := range envs {
		if e.Name == name {
			return name, nil
		}
	}
	if len(envs) == 0 {
		return "", fmt.Errorf("unknown environment %q: this repository has no environments (no overlays, no values-<name>.yaml, no environments/<name>, no .env.<name>)", want)
	}
	return "", fmt.Errorf("unknown environment %q: this repository has %s", want, environmentNamesOf(envs))
}

// noteEnvironments says what the choice of an environment left out, one
// line per environment, or, when none was chosen, which ones there are to
// choose from.
func noteEnvironments(envs []Environment, chosen string, note func(format string, args ...any)) {
	if chosen == "" {
		if len(envs) > 1 {
			note("this repository has the environments %s, read together except the local overlays; name one with --environment to draft only that one", environmentNamesOf(envs))
		}
		return
	}
	for _, e := range envs {
		if e.Name != chosen {
			note("left out the environment %s (%s): only %s is drafted", e.Name, strings.Join(e.Paths, ", "), chosen)
		}
	}
}
