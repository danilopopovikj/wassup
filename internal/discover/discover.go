// Package discover reads the repository and the cluster for evidence of how
// data flows, and proposes a topology and bindings from it. It is
// deterministic and needs no AI: Claude Code reviews what it proposes, it
// does not have to invent it. `wassup sync` diffs a fresh proposal against
// the committed .wassup/ so new services and flows show up as they appear.
package discover

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danilopopovikj/wassup/internal/discover/redact"
	"github.com/danilopopovikj/wassup/internal/model"
)

// Evidence points at where something was seen.
type Evidence struct {
	Source string `json:"source"` // terraform, manifest, helm, code, cluster
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Note   string `json:"note"`
	// Policy marks what a network policy says: an allowlist is written by
	// whoever runs the system, so it is cited before a host seen in code.
	Policy bool `json:"policy,omitempty"`
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
	ID     string       `json:"id"`
	Type   string       `json:"type"`
	Label  string       `json:"label,omitempty"`
	Group  string       `json:"group,omitempty"`
	Engine string       `json:"engine,omitempty"`
	Roles  *model.Roles `json:"roles,omitempty"`
	// RunsOn lists the nodes the pods were seen on in the live cluster. It
	// is empty when the cluster was not read.
	RunsOn    []string `json:"runs_on,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Selector  string   `json:"selector,omitempty"`
	Image     string   `json:"image,omitempty"`
	Name      string   `json:"name,omitempty"` // the source's own name (terraform name, k8s name)
	Addresses []string `json:"addresses,omitempty"`
	// Env maps the name of an environment variable to what may be kept of
	// it: the host it points at ("postgresql://db-rw.bookstore:5432"), the
	// object it comes from ("secret:api/token", "configmap:app/REDIS_HOST"),
	// or nothing. It never holds a password, a token or a whole URL; see the
	// redact package.
	Env map[string]string `json:"env,omitempty"`
	// Labels are the labels of a workload or of a node.
	Labels map[string]string `json:"labels,omitempty"`
	// Objects lists the cluster objects one component stands for, as
	// "Kind namespace/name": the Ingress objects behind the ingress, the
	// workloads of one product.
	Objects  []string          `json:"objects,omitempty"`
	Extra    map[string]string `json:"extra,omitempty"`
	Evidence []Evidence        `json:"evidence"`
}

// Link is a data flow the scanner believes exists.
type Link struct {
	From     string     `json:"from"`
	To       string     `json:"to"` // component id, or an unresolved host when Unresolved is set
	Kind     string     `json:"kind"`
	Label    string     `json:"label,omitempty"`
	Host     string     `json:"host,omitempty"`
	Port     string     `json:"port,omitempty"`
	Evidence []Evidence `json:"evidence"`
	// Internal marks a host that is the name of a Service in the cluster
	// (an Ingress backend): it is never an external dependency.
	Internal bool `json:"internal,omitempty"`
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
	// Environments are the environments the repository has; Environment is
	// the one that was read, or empty when none was chosen.
	Environments []Environment `json:"environments,omitempty"`
	Environment  string        `json:"environment,omitempty"`
	// Policies are the network policies that say what pods may reach, from
	// the repository and from the cluster.
	Policies []EgressPolicy `json:"policies,omitempty"`

	// merged maps the id of a candidate that was folded into another one to
	// the id that stands for it, so that evidence found later (the live
	// cluster after the repository) lands on the same component.
	merged map[string]string
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
	// What git ignores, lockfiles, tests, nested checkouts and local
	// Kustomize overlays are left out as well; see collectFiles.
	Skip []string
	// Namespaces limits cluster evidence to these namespaces (empty = all seen).
	Namespaces []string
	// MaxFiles caps the walk.
	MaxFiles int
	// Environment names the one environment to read (see Environments):
	// its files and what belongs to no environment. Empty reads every
	// environment but the local overlays, as one system.
	Environment string
}

var defaultSkip = []string{".git", "node_modules", "vendor", ".wassup", "dist", "build", ".terraform", ".next", "__pycache__", ".venv", "venv", "target", ".cache", ".claude"}

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
	envs := environments(root, skip)
	env, err := chooseEnvironment(envs, opts.Environment)
	if err != nil {
		return nil, err
	}
	f := &Findings{Root: root, Environments: envs, Environment: env}
	acc := newAccumulator(f)
	files, err := collectFiles(root, skip, opts.MaxFiles, env, acc.note)
	if err != nil {
		return nil, err
	}
	noteEnvironments(envs, env, acc.note)
	f.FilesRead = len(files)
	// Kustomizations first: they say which namespace the manifests next to
	// them land in, and generate ConfigMaps the workloads load.
	for _, p := range files {
		if isKustomization(filepath.Base(p)) {
			rel, _ := filepath.Rel(root, p)
			scanKustomization(acc, p, rel)
		}
	}
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		ext := strings.ToLower(filepath.Ext(p))
		base := filepath.Base(p)
		switch {
		case isKustomization(base):
		case ext == ".tf":
			scanTerraform(acc, p, rel)
		case ext == ".tfvars":
			scanTerraformVars(acc, p)
		case ext == ".yaml" || ext == ".yml":
			if base == "values.yaml" || strings.HasSuffix(base, ".values.yaml") || strings.HasPrefix(base, "values-") || strings.HasPrefix(base, "values.") {
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

	// tfValues holds what terraform variables and locals resolve to
	// ("var.bucket", "local.prefix"), for the few expressions discovery
	// resolves. It is never written out.
	tfValues map[string]string
	// tfOverride holds the values of the tfvars files Terraform loads by
	// itself; they win over the defaults.
	tfOverride map[string]string
	// configMaps holds the data of the ConfigMaps in the repository by
	// "namespace/name", for the workloads that load their environment from
	// them. It is never written out: only what redact lets through reaches
	// a candidate.
	configMaps map[string]map[string]string
	// kustomizeNS maps a directory to the namespace a kustomization puts
	// its resources in.
	kustomizeNS map[string]string
	// kustomizeOwn names the overlay a directory got its namespace from,
	// when it did not set one itself.
	kustomizeOwn map[string]string
	// workloads are the pod templates read so far. Their environment is
	// resolved at the end, when every ConfigMap is known.
	workloads []pendingWorkload
	// services wait for the workloads their selectors pick.
	services []pendingService
}

// newAccumulator starts from findings that may already hold candidates.
func newAccumulator(f *Findings) *accumulator {
	a := &accumulator{root: f.Root, f: f, byID: map[string]int{}, tfValues: map[string]string{}, tfOverride: map[string]string{},
		configMaps: map[string]map[string]string{}, kustomizeNS: map[string]string{}}
	a.reindex()
	return a
}

// reindex rebuilds the id lookup after candidates moved.
func (a *accumulator) reindex() {
	a.byID = map[string]int{}
	for i, c := range a.f.Candidates {
		a.byID[c.ID] = i
	}
}

// canonical returns the id that stands for id: itself, unless the candidate
// was folded into another.
func (f *Findings) canonical(id string) string {
	for n := 0; n < 8; n++ {
		next, ok := f.merged[id]
		if !ok || next == id {
			break
		}
		id = next
	}
	return id
}

// safe makes a candidate fit to be written: a valid id, and no credential
// in the texts. Environment values are reduced where they are read (see
// envValue); here the free texts pass through redact.Text once more, so a
// scanner that forgets cannot leak.
func safe(c *Candidate) {
	c.ID = model.Slugify(c.ID)
	for k, v := range c.Extra {
		c.Extra[k] = redact.Text(v)
	}
	for i := range c.Evidence {
		c.Evidence[i].Note = redact.Text(c.Evidence[i].Note)
	}
}

func (a *accumulator) add(c Candidate) *Candidate {
	if c.ID == "" {
		c.ID = c.Label
	}
	safe(&c)
	if c.ID == "" {
		return nil
	}
	if to := a.f.canonical(c.ID); to != c.ID {
		// The candidate was folded into another: only what describes the
		// component as a whole is taken, not where this part runs or how
		// it is configured.
		if i, ok := a.byID[to]; ok {
			cur := &a.f.Candidates[i]
			cur.Addresses = uniq(append(cur.Addresses, c.Addresses...))
			cur.Objects = uniq(append(cur.Objects, c.Objects...))
			if c.Kind != "" {
				cur.Objects = uniq(append(cur.Objects, objectRef(c.Kind, c.Namespace, c.Name)))
			}
			cur.Evidence = append(cur.Evidence, c.Evidence...)
			return cur
		}
		c.ID = to
	}
	if i, ok := a.byID[c.ID]; ok {
		cur := &a.f.Candidates[i]
		mergeCandidate(cur, c)
		return cur
	}
	a.byID[c.ID] = len(a.f.Candidates)
	a.f.Candidates = append(a.f.Candidates, c)
	return &a.f.Candidates[len(a.f.Candidates)-1]
}

// mergeCandidate adds what c knows to cur. What cur already says stays.
func mergeCandidate(cur *Candidate, c Candidate) {
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
	if cur.Name == "" {
		cur.Name = c.Name
	}
	if cur.Engine == "" {
		cur.Engine = c.Engine
	}
	if cur.Roles == nil {
		cur.Roles = c.Roles
	}
	cur.Addresses = uniq(append(cur.Addresses, c.Addresses...))
	cur.RunsOn = uniq(append(cur.RunsOn, c.RunsOn...))
	cur.Objects = uniq(append(cur.Objects, c.Objects...))
	cur.Env = mergeMap(cur.Env, c.Env)
	cur.Labels = mergeMap(cur.Labels, c.Labels)
	cur.Extra = mergeMap(cur.Extra, c.Extra)
	cur.Evidence = append(cur.Evidence, c.Evidence...)
}

// mergeMap adds the keys of src that dst does not have. A key that is
// present without a value gives way to one that has a value.
func mergeMap(dst, src map[string]string) map[string]string {
	if dst == nil && src != nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		if cur, ok := dst[k]; !ok || cur == "" {
			dst[k] = v
		}
	}
	return dst
}

// link records a flow. A host that names nothing (a placeholder, a
// developer's machine, a tunnel, a documentation domain) is not a flow of
// the system and is left out.
func (a *accumulator) link(l Link) {
	if l.Host != "" {
		l.Host = strings.ToLower(l.Host)
		if !usableHost(l.Host) {
			return
		}
	}
	if l.From != "" && l.From != "repo" {
		l.From = a.f.canonical(model.Slugify(l.From))
	}
	if l.To != "" {
		l.To = a.f.canonical(model.Slugify(l.To))
	}
	for i := range l.Evidence {
		l.Evidence[i].Note = redact.Text(l.Evidence[i].Note)
	}
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

// note adds a line for the reviewer, once.
func (a *accumulator) note(format string, args ...any) {
	n := redact.Text(fmt.Sprintf(format, args...))
	for _, have := range a.f.Notes {
		if have == n {
			return
		}
	}
	a.f.Notes = append(a.f.Notes, n)
}

func (a *accumulator) provider(p string) {
	for _, x := range a.f.Providers {
		if x == p {
			return
		}
	}
	a.f.Providers = append(a.f.Providers, p)
}

// finish emits terraform resources (so references resolve across files),
// resolves the environment of the workloads against the ConfigMaps, folds
// the workloads of one product into one candidate, adds what the network
// policies allow and resolves hosts in links to candidate ids.
func (a *accumulator) finish() {
	emitTerraform(a)
	a.resolveWorkloads()
	a.collapseProducts()
	a.mergeCodeOnly()
	a.settleIngress()
	a.applyPolicies()
	resolveLinks(a.f)
	a.reindex()
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
