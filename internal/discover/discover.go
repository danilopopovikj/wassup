// Package discover reads the repository and the cluster for evidence of how
// data flows, and proposes a topology and bindings from it. It is
// deterministic and needs no AI: Claude Code reviews what it proposes, it
// does not have to invent it. `wassup sync` diffs a fresh proposal against
// the committed .wassup/ so new services and flows show up as they appear.
package discover

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danilopopovikj/wassup/internal/model"
)

// Evidence points at where something was seen.
type Evidence struct {
	Source string `json:"source"` // terraform, manifest, helm, code, cluster
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Note   string `json:"note"`
}

func (e Evidence) String() string {
	loc := e.File
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", e.File, e.Line)
	}
	if loc == "" {
		return e.Source + ": " + e.Note
	}
	return e.Source + " " + loc + ": " + e.Note
}

// Candidate is a component the scanner believes exists.
type Candidate struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Label     string            `json:"label,omitempty"`
	Group     string            `json:"group,omitempty"`
	Engine    string            `json:"engine,omitempty"`
	Roles     *model.Roles      `json:"roles,omitempty"`
	RunsOn    []string          `json:"runs_on,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Selector  string            `json:"selector,omitempty"`
	Image     string            `json:"image,omitempty"`
	Name      string            `json:"name,omitempty"` // the source's own name (terraform name, k8s name)
	Addresses []string          `json:"addresses,omitempty"`
	Env       map[string]string `json:"env,omitempty"` // env var name -> value or ref
	Extra     map[string]string `json:"extra,omitempty"`
	Evidence  []Evidence        `json:"evidence"`
}

// Link is a data flow the scanner believes exists.
type Link struct {
	From     string     `json:"from"`
	To       string     `json:"to"` // component id, or an unresolved host when Unresolved is set
	Kind     string     `json:"kind"`
	Label    string     `json:"label,omitempty"`
	Host     string     `json:"host,omitempty"`
	Evidence []Evidence `json:"evidence"`
	// Reverse means the data flows from the host to From (a replication
	// source read by a sync engine): the ends swap once the host resolves.
	Reverse bool `json:"reverse,omitempty"`
}

// Findings is everything the scanners saw.
type Findings struct {
	Root       string       `json:"root"`
	Candidates []Candidate  `json:"candidates"`
	Links      []Link       `json:"links"`
	Providers  []string     `json:"providers,omitempty"` // terraform providers seen
	Cluster    *ClusterHint `json:"cluster,omitempty"`
	Notes      []string     `json:"notes,omitempty"`
	FilesRead  int          `json:"files_read"`
}

// ClusterHint carries what the live cluster contributed.
type ClusterHint struct {
	Context    string   `json:"context,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// Options steer a scan.
type Options struct {
	// Root is the repository root.
	Root string
	// Skip lists directory names never entered (defaults added: .git, node_modules, vendor, .wassup, dist, build).
	Skip []string
	// Namespaces limits cluster evidence to these namespaces (empty = all seen).
	Namespaces []string
	// MaxFiles caps the walk.
	MaxFiles int
}

var defaultSkip = []string{".git", "node_modules", "vendor", ".wassup", "dist", "build", ".terraform", ".next", "__pycache__", ".venv", "venv", "target", ".cache"}

// Scan walks the repository with every scanner.
func Scan(opts Options) (*Findings, error) {
	if opts.Root == "" {
		opts.Root = "."
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 20000
	}
	skip := map[string]bool{}
	for _, s := range append(append([]string{}, defaultSkip...), opts.Skip...) {
		skip[s] = true
	}
	f := &Findings{Root: root}
	var files []string
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= opts.MaxFiles {
			return filepath.SkipAll
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	f.FilesRead = len(files)
	acc := &accumulator{root: root, f: f, byID: map[string]int{}}
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		ext := strings.ToLower(filepath.Ext(p))
		base := filepath.Base(p)
		switch {
		case ext == ".tf":
			scanTerraform(acc, p, rel)
		case ext == ".yaml" || ext == ".yml":
			if base == "values.yaml" || strings.HasSuffix(base, ".values.yaml") || strings.HasPrefix(base, "values-") {
				scanHelmValues(acc, p, rel)
			} else if !strings.Contains(rel, string(filepath.Separator)+"templates"+string(filepath.Separator)) {
				scanManifest(acc, p, rel)
			}
		case codeExt[ext]:
			scanCode(acc, p, rel)
		case base == ".env" || base == ".env.example" || strings.HasPrefix(base, ".env."):
			scanDotenv(acc, p, rel)
		}
	}
	acc.finish()
	return f, nil
}

// accumulator merges evidence about the same thing from several files.
type accumulator struct {
	root string
	f    *Findings
	byID map[string]int
	tf   []*tfResource // terraform resources, emitted together at the end
}

func (a *accumulator) add(c Candidate) *Candidate {
	if c.ID == "" {
		c.ID = model.Slugify(c.Label)
	}
	if c.ID == "" {
		return nil
	}
	if i, ok := a.byID[c.ID]; ok {
		cur := &a.f.Candidates[i]
		if cur.Type == "custom" || cur.Type == "" {
			cur.Type = c.Type
		}
		if cur.Label == "" {
			cur.Label = c.Label
		}
		if cur.Namespace == "" {
			cur.Namespace = c.Namespace
		}
		if cur.Selector == "" {
			cur.Selector = c.Selector
		}
		if cur.Image == "" {
			cur.Image = c.Image
		}
		if cur.Kind == "" {
			cur.Kind = c.Kind
		}
		if cur.Engine == "" {
			cur.Engine = c.Engine
		}
		if cur.Roles == nil {
			cur.Roles = c.Roles
		}
		cur.Addresses = uniq(append(cur.Addresses, c.Addresses...))
		cur.RunsOn = uniq(append(cur.RunsOn, c.RunsOn...))
		if cur.Env == nil && c.Env != nil {
			cur.Env = map[string]string{}
		}
		for k, v := range c.Env {
			if _, ok := cur.Env[k]; !ok {
				cur.Env[k] = v
			}
		}
		if cur.Extra == nil && c.Extra != nil {
			cur.Extra = map[string]string{}
		}
		for k, v := range c.Extra {
			if _, ok := cur.Extra[k]; !ok {
				cur.Extra[k] = v
			}
		}
		cur.Evidence = append(cur.Evidence, c.Evidence...)
		return cur
	}
	a.byID[c.ID] = len(a.f.Candidates)
	a.f.Candidates = append(a.f.Candidates, c)
	return &a.f.Candidates[len(a.f.Candidates)-1]
}

func (a *accumulator) link(l Link) {
	for i := range a.f.Links {
		cur := &a.f.Links[i]
		if cur.From == l.From && cur.To == l.To && cur.Host == l.Host {
			if cur.Kind == "" {
				cur.Kind = l.Kind
			}
			cur.Evidence = append(cur.Evidence, l.Evidence...)
			return
		}
	}
	a.f.Links = append(a.f.Links, l)
}

func (a *accumulator) note(format string, args ...any) {
	a.f.Notes = append(a.f.Notes, fmt.Sprintf(format, args...))
}

func (a *accumulator) provider(p string) {
	for _, x := range a.f.Providers {
		if x == p {
			return
		}
	}
	a.f.Providers = append(a.f.Providers, p)
}

// finish emits terraform resources (so references resolve across files)
// and resolves hosts in links to candidate ids.
func (a *accumulator) finish() {
	emitTerraform(a)
	resolveLinks(a.f)
	sort.SliceStable(a.f.Candidates, func(i, j int) bool { return a.f.Candidates[i].ID < a.f.Candidates[j].ID })
	sort.SliceStable(a.f.Links, func(i, j int) bool {
		if a.f.Links[i].From != a.f.Links[j].From {
			return a.f.Links[i].From < a.f.Links[j].From
		}
		return a.f.Links[i].To < a.f.Links[j].To
	})
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Candidate lookup by id.
func (f *Findings) candidate(id string) *Candidate {
	for i := range f.Candidates {
		if f.Candidates[i].ID == id {
			return &f.Candidates[i]
		}
	}
	return nil
}
