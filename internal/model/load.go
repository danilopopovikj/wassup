package model

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed schema/*.json
var schemaFS embed.FS

// SchemaFS exposes the embedded JSON Schemas (for `skill install`).
func SchemaFS() embed.FS { return schemaFS }

// SchemaNames lists the embedded schemas.
var SchemaNames = []string{"topology", "bindings", "findings", "thresholds", "annotations", "layout"}

var (
	schemaOnce sync.Once
	schemas    map[string]*jsonschema.Schema
	schemaErr  error
)

func compiledSchemas() (map[string]*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		out := map[string]*jsonschema.Schema{}
		for _, name := range SchemaNames {
			b, err := schemaFS.ReadFile("schema/" + name + ".json")
			if err != nil {
				schemaErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				schemaErr = fmt.Errorf("schema %s: %w", name, err)
				return
			}
			url := "https://wassup.dev/schema/" + name + ".json"
			if err := c.AddResource(url, doc); err != nil {
				schemaErr = fmt.Errorf("schema %s: %w", name, err)
				return
			}
			s, err := c.Compile(url)
			if err != nil {
				schemaErr = fmt.Errorf("schema %s: %w", name, err)
				return
			}
			out[name] = s
		}
		schemas = out
	})
	return schemas, schemaErr
}

// Problem is one validation problem, machine readable.
type Problem struct {
	File    string `json:"file"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

func (i Problem) String() string {
	if i.Path != "" {
		return i.File + " " + i.Path + ": " + i.Message
	}
	return i.File + ": " + i.Message
}

// ValidationError carries every issue found.
type ValidationError struct {
	Issues []Problem
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	for i, is := range e.Issues {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(is.String())
	}
	return b.String()
}

// ValidateDoc validates a decoded YAML/JSON document against a named schema.
func ValidateDoc(schema, file string, doc any) []Problem {
	ss, err := compiledSchemas()
	if err != nil {
		return []Problem{{File: file, Message: err.Error()}}
	}
	s, ok := ss[schema]
	if !ok {
		return []Problem{{File: file, Message: "no schema " + schema}}
	}
	// Round trip through JSON so YAML integer/float/map types match what the
	// validator expects.
	jb, err := json.Marshal(doc)
	if err != nil {
		return []Problem{{File: file, Message: err.Error()}}
	}
	norm, err := jsonschema.UnmarshalJSON(bytes.NewReader(jb))
	if err != nil {
		return []Problem{{File: file, Message: err.Error()}}
	}
	if err := s.Validate(norm); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			var out []Problem
			for _, u := range flattenOutput(ve.BasicOutput()) {
				out = append(out, Problem{File: file, Path: u.InstanceLocation, Message: u.Error.String()})
			}
			if len(out) == 0 {
				out = append(out, Problem{File: file, Message: ve.Error()})
			}
			return out
		}
		return []Problem{{File: file, Message: err.Error()}}
	}
	return nil
}

func flattenOutput(u *jsonschema.OutputUnit) []jsonschema.OutputUnit {
	var out []jsonschema.OutputUnit
	if u == nil {
		return out
	}
	if u.Error != nil {
		out = append(out, *u)
	}
	for _, e := range u.Errors {
		out = append(out, flattenOutput(&e)...)
	}
	return out
}

// decodeYAML reads a YAML file into both a generic document (for the schema)
// and a typed struct.
func decodeYAML(path string, typed any) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := yaml.Unmarshal(b, &generic); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := yaml.Unmarshal(b, typed); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return generic, nil
}

func decodeJSON(path string, typed any) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(b, typed); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return generic, nil
}

// Config is every file under .wassup/ that the TUI and CLI read.
type Config struct {
	Dir         string
	Topology    Topology
	Bindings    Bindings
	Findings    Findings
	Thresholds  Thresholds
	Layout      Layout
	Annotations Annotations
	// Present records which optional files exist.
	Present map[string]bool
}

// FindDir walks up from start looking for a .wassup directory. It returns the
// directory path or an error.
func FindDir(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		cand := filepath.Join(dir, DirName)
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s directory found from %s upward", DirName, start)
		}
		dir = parent
	}
}

// Load reads and validates every file in dir. Missing optional files are
// fine; a missing topology.yaml is an error.
func Load(dir string) (*Config, error) {
	cfg := &Config{Dir: dir, Present: map[string]bool{}}
	var issues []Problem

	topoPath := filepath.Join(dir, "topology.yaml")
	if generic, err := decodeYAML(topoPath, &cfg.Topology); err != nil {
		return nil, err
	} else {
		cfg.Present["topology.yaml"] = true
		issues = append(issues, ValidateDoc("topology", "topology.yaml", generic)...)
	}

	optionalYAML := []struct {
		name, schema string
		into         any
	}{
		{"bindings.yaml", "bindings", &cfg.Bindings},
		{"findings.yaml", "findings", &cfg.Findings},
		{"thresholds.yaml", "thresholds", &cfg.Thresholds},
	}
	for _, f := range optionalYAML {
		p := filepath.Join(dir, f.name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		generic, err := decodeYAML(p, f.into)
		if err != nil {
			issues = append(issues, Problem{File: f.name, Message: err.Error()})
			continue
		}
		cfg.Present[f.name] = true
		issues = append(issues, ValidateDoc(f.schema, f.name, generic)...)
	}
	if p := filepath.Join(dir, "layout.json"); fileExists(p) {
		generic, err := decodeJSON(p, &cfg.Layout)
		if err != nil {
			issues = append(issues, Problem{File: "layout.json", Message: err.Error()})
		} else {
			cfg.Present["layout.json"] = true
			issues = append(issues, ValidateDoc("layout", "layout.json", generic)...)
		}
	}
	if p := filepath.Join(dir, "state", "annotations.json"); fileExists(p) {
		generic, err := decodeJSON(p, &cfg.Annotations)
		if err != nil {
			issues = append(issues, Problem{File: "state/annotations.json", Message: err.Error()})
		} else {
			cfg.Present["state/annotations.json"] = true
			issues = append(issues, ValidateDoc("annotations", "state/annotations.json", generic)...)
		}
	}
	if len(issues) == 0 {
		issues = append(issues, cfg.CrossCheck()...)
	}
	if cfg.Layout.Components == nil {
		cfg.Layout.Components = map[string]Placement{}
	}
	cfg.Layout.Version = Version
	if len(issues) > 0 {
		return cfg, &ValidationError{Issues: issues}
	}
	return cfg, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// CrossCheck verifies ids across files: unique ids, known groups, edge ends,
// runs_on targets, binding targets, finding components, catalog probes.
func (c *Config) CrossCheck() []Problem {
	var out []Problem
	t := &c.Topology
	add := func(file, path, msg string) { out = append(out, Problem{File: file, Path: path, Message: msg}) }

	ids := map[string]string{}
	for i, g := range t.Groups {
		if prev, dup := ids[g.ID]; dup {
			add("topology.yaml", fmt.Sprintf("/groups/%d/id", i), "duplicate id "+g.ID+" (also a "+prev+")")
		}
		ids[g.ID] = "group"
	}
	for i, g := range t.Groups {
		if g.Parent != "" && ids[g.Parent] != "group" {
			add("topology.yaml", fmt.Sprintf("/groups/%d/parent", i), "unknown parent group "+g.Parent)
		}
	}
	for i, comp := range t.AllComponents() {
		if prev, dup := ids[comp.ID]; dup {
			add("topology.yaml", fmt.Sprintf("/components/%d/id", i), "duplicate id "+comp.ID+" (also a "+prev+")")
		}
		ids[comp.ID] = "component"
	}
	for i, comp := range t.Components {
		if comp.Group != "" && ids[comp.Group] != "group" {
			add("topology.yaml", fmt.Sprintf("/components/%d/group", i), "unknown group "+comp.Group)
		}
		for _, n := range comp.RunsOn {
			nc, ok := t.Component(n)
			if !ok {
				add("topology.yaml", fmt.Sprintf("/components/%d/runs_on", i), "unknown node "+n)
			} else if nc.Type != "node" {
				add("topology.yaml", fmt.Sprintf("/components/%d/runs_on", i), n+" is a "+nc.Type+", runs_on must name nodes")
			}
		}
		if comp.Roles != nil && comp.Type != "db" {
			add("topology.yaml", fmt.Sprintf("/components/%d/roles", i), "roles are only allowed on db components")
		}
		if _, ok := Catalog[comp.Type]; !ok {
			add("topology.yaml", fmt.Sprintf("/components/%d/type", i), "unknown type "+comp.Type)
		}
	}
	edgeIDs := map[string]bool{}
	for i, e := range t.Edges {
		if ids[e.From] != "component" {
			add("topology.yaml", fmt.Sprintf("/edges/%d/from", i), "unknown component "+e.From)
		}
		if ids[e.To] != "component" {
			add("topology.yaml", fmt.Sprintf("/edges/%d/to", i), "unknown component "+e.To)
		}
		if edgeIDs[e.ID()] {
			add("topology.yaml", fmt.Sprintf("/edges/%d", i), "duplicate edge "+e.ID())
		}
		edgeIDs[e.ID()] = true
	}
	for id, specs := range c.Bindings.Components {
		if ids[id] != "component" {
			add("bindings.yaml", "/components/"+id, "no such component in topology.yaml")
		}
		for j, s := range specs {
			if s.Kind() == "" {
				add("bindings.yaml", fmt.Sprintf("/components/%s/%d", id, j), "probe kind missing")
			}
		}
	}
	for id, specs := range c.Bindings.Edges {
		if !edgeIDs[id] {
			add("bindings.yaml", "/edges/"+id, "no such edge in topology.yaml")
		}
		for j, s := range specs {
			if s.Kind() == "" {
				add("bindings.yaml", fmt.Sprintf("/edges/%s/%d", id, j), "probe kind missing")
			}
		}
	}
	seenF := map[string]bool{}
	for i, f := range c.Findings.Findings {
		if seenF[f.ID] {
			add("findings.yaml", fmt.Sprintf("/findings/%d/id", i), "duplicate finding id "+f.ID)
		}
		seenF[f.ID] = true
		if f.Component != "" && ids[f.Component] != "component" && !edgeIDs[f.Component] {
			add("findings.yaml", fmt.Sprintf("/findings/%d/component", i), "no such component "+f.Component)
		}
	}
	for id := range c.Thresholds.Components {
		if ids[id] != "component" && !edgeIDs[id] {
			add("thresholds.yaml", "/components/"+id, "no such component or edge")
		}
	}
	for id := range c.Layout.Components {
		if ids[id] == "" && !edgeIDs[id] {
			add("layout.json", "/components/"+id, "no such component (stale layout entry)")
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// WriteAtomic writes data to path via a temp file, fsync and rename so a
// reader never sees a partial file.
func WriteAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// WriteYAML marshals v and writes it atomically.
func WriteYAML(path string, v any) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return WriteAtomic(path, buf.Bytes())
}

// WriteJSON marshals v with indentation and writes it atomically.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(path, append(b, '\n'))
}

// SaveLayout writes layout.json.
func (c *Config) SaveLayout() error {
	c.Layout.Version = Version
	return WriteJSON(filepath.Join(c.Dir, "layout.json"), c.Layout)
}

// SaveAnnotations writes state/annotations.json.
func SaveAnnotations(dir string, a Annotations) error {
	a.Version = Version
	if a.Annotations == nil {
		a.Annotations = []Annotation{}
	}
	return WriteJSON(filepath.Join(dir, "state", "annotations.json"), a)
}

// LoadAnnotations reads state/annotations.json, returning an empty set if absent.
func LoadAnnotations(dir string) (Annotations, error) {
	var a Annotations
	p := filepath.Join(dir, "state", "annotations.json")
	if !fileExists(p) {
		return Annotations{Version: Version}, nil
	}
	_, err := decodeJSON(p, &a)
	return a, err
}

// LoadSnapshot reads state/snapshot.json.
func LoadSnapshot(dir string) (*Snapshot, error) {
	var s Snapshot
	b, err := os.ReadFile(filepath.Join(dir, "state", "snapshot.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
