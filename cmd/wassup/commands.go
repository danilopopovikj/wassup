package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danilopopovikj/wassup/internal/app"
	"github.com/danilopopovikj/wassup/internal/demo"
	"github.com/danilopopovikj/wassup/internal/explain"
	"github.com/danilopopovikj/wassup/internal/history"
	"github.com/danilopopovikj/wassup/internal/layout"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/fixture"
	"github.com/danilopopovikj/wassup/internal/render"
	"github.com/danilopopovikj/wassup/internal/scenario"
	"github.com/danilopopovikj/wassup/prompts"
	"github.com/danilopopovikj/wassup/skill"
)

// Kubernetes-backed helpers are wired in kube.go; when the build has no
// Kubernetes support they stay nil and the commands say so.
var (
	kubeLogs     explain.LogSource
	kubeEvents   explain.EventSource
	kubeDiscover func(ctx context.Context, kubeconfig, kcontext string, namespaces []string) (any, error)
)

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// loadConfig loads and validates; on validation errors the machine readable
// list is printed and the error returned.
func loadConfig() (*model.Config, error) {
	dir, err := findDir()
	if err != nil {
		return nil, err
	}
	cfg, err := model.Load(dir)
	if err != nil {
		if ve, ok := err.(*model.ValidationError); ok {
			if flags.jsonOut {
				_ = printJSON(map[string]any{"ok": false, "issues": ve.Issues})
			} else {
				for _, is := range ve.Issues {
					fmt.Fprintln(os.Stderr, "  "+is.String())
				}
			}
		}
		return cfg, err
	}
	return cfg, nil
}

func initCmd() *cobra.Command {
	var printPrompt bool
	c := &cobra.Command{
		Use:   "init",
		Short: "create .wassup/ with the setup prompt, or print the prompt",
		RunE: func(cmd *cobra.Command, args []string) error {
			if printPrompt {
				fmt.Print(prompts.Setup)
				return nil
			}
			dir := flags.dir
			if dir == "" {
				dir = model.DirName
			} else if filepath.Base(dir) != model.DirName {
				dir = filepath.Join(dir, model.DirName)
			}
			if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "prompts", "setup.md"), []byte(prompts.Setup), 0o644); err != nil {
				return err
			}
			gi := filepath.Join(dir, ".gitignore")
			if _, err := os.Stat(gi); os.IsNotExist(err) {
				_ = os.WriteFile(gi, []byte("state/\n"), 0o644)
			}
			topo := filepath.Join(dir, "topology.yaml")
			if _, err := os.Stat(topo); os.IsNotExist(err) {
				_ = os.WriteFile(topo, []byte("version: 1\nname: my-system\ncomponents: []\nedges: []\n"), 0o644)
			}
			fmt.Printf("created %s\n\nPaste this into Claude Code:\n\n%s", dir, prompts.Setup)
			render.CopyText(prompts.Setup)
			return nil
		},
	}
	c.Flags().BoolVar(&printPrompt, "print-prompt", false, "print prompts/setup.md and exit")
	return c
}

func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "validate every config file against its schema and cross-check ids",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			// Probe kinds and specs.
			var issues []model.Problem
			check := func(file, id string, specs []model.ProbeSpec) {
				for i, s := range specs {
					kind := s.Kind()
					if !probe.Known(kind) {
						issues = append(issues, model.Problem{File: file, Path: fmt.Sprintf("/%s/%d", id, i), Message: "unknown probe " + kind})
						continue
					}
					p, _ := probe.New(kind)
					if err := p.Validate(s.Plain()); err != nil {
						issues = append(issues, model.Problem{File: file, Path: fmt.Sprintf("/%s/%d", id, i), Message: kind + ": " + err.Error()})
					}
				}
			}
			for id, specs := range cfg.Bindings.Components {
				check("bindings.yaml", "components/"+id, specs)
			}
			for id, specs := range cfg.Bindings.Edges {
				check("bindings.yaml", "edges/"+id, specs)
			}
			// Fit by facet: a probe declares the facets it fills, a type the
			// facets it accepts. Probes that declare nothing fall back to the
			// catalog's advisory probe list.
			var warnings []string
			fits := func(typeFacets, probeFacets []string) bool {
				for _, a := range typeFacets {
					for _, b := range probeFacets {
						if a == b {
							return true
						}
					}
				}
				return false
			}
			for id, specs := range cfg.Bindings.Components {
				comp, ok := cfg.Topology.Component(id)
				if !ok {
					continue
				}
				spec := model.Catalog[comp.Type]
				for _, s := range specs {
					acc, _ := probe.AccessFor(s.Kind())
					if len(acc.Facets) > 0 {
						if len(spec.Facets) > 0 && !fits(spec.Facets, acc.Facets) {
							warnings = append(warnings, fmt.Sprintf("%s: probe %s fills %s, but a %s shows %s", id, s.Kind(), strings.Join(acc.Facets, "/"), comp.Type, strings.Join(spec.Facets, "/")))
						}
						continue
					}
					allowed := len(spec.Probes) == 0
					for _, p := range spec.Probes {
						if p == s.Kind() {
							allowed = true
						}
					}
					if !allowed {
						warnings = append(warnings, fmt.Sprintf("%s: probe %s is unusual for type %s", id, s.Kind(), comp.Type))
					}
				}
			}
			for id, specs := range cfg.Bindings.Edges {
				for _, s := range specs {
					acc, _ := probe.AccessFor(s.Kind())
					if len(acc.Facets) > 0 && !fits(model.EdgeFacets, acc.Facets) {
						warnings = append(warnings, fmt.Sprintf("%s: probe %s fills %s, which is not an edge facet (%s)", id, s.Kind(), strings.Join(acc.Facets, "/"), strings.Join(model.EdgeFacets, "/")))
					}
				}
			}
			unbound := 0
			for _, c := range cfg.Topology.AllComponents() {
				if len(cfg.Bindings.Components[c.ID]) == 0 && !cfg.Topology.HasRoles(c.ID) {
					unbound++
					warnings = append(warnings, fmt.Sprintf("%s has no probe and will draw as unbound", c.ID))
				}
			}
			sort.Strings(warnings)
			if len(issues) > 0 {
				if flags.jsonOut {
					_ = printJSON(map[string]any{"ok": false, "issues": issues, "warnings": warnings})
				} else {
					for _, is := range issues {
						fmt.Fprintln(os.Stderr, "  "+is.String())
					}
				}
				return &model.ValidationError{Issues: issues}
			}
			if flags.jsonOut {
				return printJSON(map[string]any{"ok": true, "components": len(cfg.Topology.AllComponents()), "edges": len(cfg.Topology.Edges), "unbound": unbound, "warnings": warnings})
			}
			fmt.Printf("ok: %d components, %d edges, %d groups", len(cfg.Topology.AllComponents()), len(cfg.Topology.Edges), len(cfg.Topology.AllGroups()))
			if unbound > 0 {
				fmt.Printf(", %d unbound", unbound)
			}
			fmt.Println()
			for _, w := range warnings {
				fmt.Println("  warning: " + w)
			}
			return nil
		},
	}
}

func probeCmd() *cobra.Command {
	var once bool
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "probe",
		Short: "run every binding once and report bound or unbound per component",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			rt, err := app.New(app.Options{Dir: cfg.Dir, Kubeconfig: flags.kubeconfig, Context: flags.kcontext, ReadOnly: true})
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
			defer cancel()
			snap, err := rt.RunOnce(ctx, timeout)
			if err != nil {
				return err
			}
			type row struct {
				ID     string `json:"id"`
				Kind   string `json:"kind"`
				Bound  bool   `json:"bound"`
				State  string `json:"state"`
				Label  string `json:"label"`
				Probes string `json:"probes"`
			}
			var rows []row
			bound, total := 0, 0
			for _, comp := range cfg.Topology.AllComponents() {
				es := snap.Components[comp.ID]
				var kinds []string
				for _, s := range cfg.Bindings.Components[comp.ID] {
					kinds = append(kinds, s.Kind())
				}
				isBound := es.Marker != model.MarkerUnbound
				if isBound {
					bound++
				}
				total++
				rows = append(rows, row{ID: comp.ID, Kind: comp.Type, Bound: isBound, State: string(es.State), Label: es.Label, Probes: strings.Join(kinds, ",")})
			}
			for _, e := range cfg.Topology.Edges {
				es := snap.Edges[e.ID()]
				var kinds []string
				for _, s := range cfg.Bindings.Edges[e.ID()] {
					kinds = append(kinds, s.Kind())
				}
				rows = append(rows, row{ID: e.ID(), Kind: "edge", Bound: es.Marker != model.MarkerUnbound, State: string(es.State), Label: es.Label, Probes: strings.Join(kinds, ",")})
			}
			if flags.jsonOut {
				return printJSON(map[string]any{"bound": bound, "total": total, "elements": rows, "probes": rt.Instances(), "probe_health": rt.ProbeHealth()})
			}
			for _, r := range rows {
				mark := "bound  "
				if !r.Bound {
					mark = "UNBOUND"
				}
				fmt.Printf("%s  %-24s %-9s %-11s %s\n", mark, r.ID, r.Kind, r.State, r.Label)
			}
			fmt.Printf("\n%d of %d components bound\n", bound, total)
			for _, in := range rt.Instances() {
				if in.Health != string(probe.HealthOK) {
					msg := in.Message
					if in.Error != "" {
						msg = in.Error
					}
					fmt.Printf("  %s on %s: %s %s\n", in.Kind, in.Target, in.Health, msg)
				}
			}
			if bound < total {
				os.Exit(3)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&once, "once", true, "run once and exit (the only mode)")
	c.Flags().DurationVar(&timeout, "timeout", 20*time.Second, "how long to wait for every probe")
	return c
}

func snapshotCmd() *cobra.Command {
	var watch, until string
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "snapshot",
		Short: "print the current snapshot, or wait for an element to reach a state",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := findDir()
			if err != nil {
				return err
			}
			if watch == "" {
				snap, err := model.LoadSnapshot(dir)
				if err != nil {
					return fmt.Errorf("no snapshot yet (is wassup running?): %w", err)
				}
				if flags.jsonOut {
					return printJSON(snap)
				}
				printSnapshot(snap)
				return nil
			}
			ref, err := model.ParseRef(watch)
			if err != nil {
				return err
			}
			cfg, err := model.Load(dir)
			if err != nil {
				return err
			}
			ref, err = cfg.Topology.Resolve(ref)
			if err != nil {
				return err
			}
			deadline := time.Now().Add(timeout)
			for {
				snap, err := model.LoadSnapshot(dir)
				if err == nil {
					var es model.ElementState
					if ref.IsEdge() {
						es = snap.Edges[ref.ID]
					} else {
						es = snap.Components[ref.ID]
					}
					ok := false
					switch until {
					case "healthy":
						ok = (es.State == model.Flowing || es.State == model.Idle) && es.Marker == "" && es.Severity == model.Info
					default:
						ok = string(es.State) == until
					}
					fmt.Fprintf(os.Stderr, "%s %s: %s (%s)\n", snap.GeneratedAt.Format("15:04:05"), ref.ID, es.State, es.Label)
					if ok {
						if flags.jsonOut {
							return printJSON(map[string]any{"ok": true, "ref": ref.String(), "state": es})
						}
						fmt.Printf("%s reached %s: %s\n", ref.ID, until, es.Label)
						return nil
					}
				}
				if time.Now().After(deadline) {
					if flags.jsonOut {
						_ = printJSON(map[string]any{"ok": false, "ref": ref.String(), "wanted": until})
					}
					return fmt.Errorf("%s did not reach %s within %s", ref.ID, until, timeout)
				}
				time.Sleep(2 * time.Second)
			}
		},
	}
	c.Flags().StringVar(&watch, "watch", "", "element ref to watch")
	c.Flags().StringVar(&until, "until", "healthy", "state to wait for: healthy, flowing, idle, waiting, processing, blocked, failing")
	c.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "give up after")
	return c
}

func printSnapshot(s *model.Snapshot) {
	fmt.Printf("%s tick %d at %s\n", s.Name, s.Tick, s.GeneratedAt.Format(time.RFC3339))
	ids := make([]string, 0, len(s.Components))
	for id := range s.Components {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		es := s.Components[id]
		fmt.Printf("  %s %-22s %-11s %s\n", es.State.Glyph(), id, es.Severity, es.Label)
	}
	ids = ids[:0]
	for id := range s.Edges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		es := s.Edges[id]
		fmt.Printf("  %s %-30s %-11s %s\n", es.State.Glyph(), id, es.Severity, es.Label)
	}
	for _, is := range s.Issues {
		fmt.Printf("issue %s, cause %s:\n", is.ID, is.Cause)
		for _, l := range is.Story {
			fmt.Println("    " + l)
		}
	}
}

// explainSource builds the explain input from the files on disk.
func explainSource(cfg *model.Config) explain.Source {
	store := history.NewStore(cfg.Dir)
	snap, _ := model.LoadSnapshot(cfg.Dir)
	frames, _ := store.LoadFrames(time.Now().Add(-24 * time.Hour))
	evs, _ := store.LoadEvents()
	return explain.Source{Cfg: cfg, Snapshot: snap, Frames: frames, Events: evs, Now: time.Now(), Logs: kubeLogs, K8sEvent: kubeEvents}
}

func explainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain <ref> [@time]",
		Short: "everything known about an element, in under 200 lines",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			refText := args[0]
			if len(args) == 2 {
				refText += strings.TrimPrefix(args[1], " ")
				if !strings.HasPrefix(args[1], "@") {
					refText = args[0] + "@" + args[1]
				}
			}
			ref, err := model.ParseRef(refText)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := explain.Explain(ctx, explainSource(cfg), ref)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return printJSON(res)
			}
			for _, l := range res.Lines {
				fmt.Println(l)
			}
			return nil
		},
	}
}

func logsCmd() *cobra.Command {
	var since time.Duration
	var tail int
	c := &cobra.Command{
		Use:   "logs <ref>",
		Short: "container logs for a workload ref, merged across pods, with pod prefix",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ref, err := model.ParseRef(args[0])
			if err != nil {
				return err
			}
			ref, err = cfg.Topology.Resolve(ref)
			if err != nil {
				return err
			}
			comp, ok := cfg.Topology.Component(ref.ID)
			if !ok {
				return fmt.Errorf("%s is not a component", ref.ID)
			}
			if kubeLogs == nil {
				return fmt.Errorf("this build has no Kubernetes support")
			}
			var spec model.ProbeSpec
			for _, s := range cfg.Bindings.Components[comp.ID] {
				if strings.HasPrefix(s.Kind(), "k8s.") || strings.HasPrefix(s.Kind(), "cnpg.") {
					spec = s
					break
				}
			}
			if spec == nil {
				return fmt.Errorf("%s has no k8s.* binding to read logs through", comp.ID)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			lines, err := kubeLogs(ctx, comp, spec, since, tail)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return printJSON(map[string]any{"ref": ref.String(), "lines": lines})
			}
			for _, l := range lines {
				fmt.Println(l)
			}
			return nil
		},
	}
	c.Flags().DurationVar(&since, "since", 15*time.Minute, "how far back")
	c.Flags().IntVar(&tail, "tail", 200, "max lines per pod")
	return c
}

func eventsCmd() *cobra.Command {
	var since time.Duration
	c := &cobra.Command{
		Use:   "events [ref]",
		Short: "change markers and Kubernetes events touching an element",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			id := ""
			var comp model.Component
			if len(args) == 1 {
				ref, err := model.ParseRef(args[0])
				if err != nil {
					return err
				}
				ref, err = cfg.Topology.Resolve(ref)
				if err != nil {
					return err
				}
				id = ref.ID
				comp, _ = cfg.Topology.Component(id)
			}
			store := history.NewStore(cfg.Dir)
			all, _ := store.LoadEvents()
			evs := explain.EventsFor(all, id, since, time.Now())
			var k8s []string
			if id != "" && kubeEvents != nil && comp.ID != "" {
				for _, s := range cfg.Bindings.Components[comp.ID] {
					if strings.HasPrefix(s.Kind(), "k8s.") {
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						k8s, _ = kubeEvents(ctx, comp, s, since)
						cancel()
						break
					}
				}
			}
			if flags.jsonOut {
				return printJSON(map[string]any{"ref": id, "markers": evs, "kubernetes": k8s})
			}
			for _, e := range evs {
				line := fmt.Sprintf("%s %s %-12s %s", e.At.Format("01-02 15:04"), e.Letter(), e.Target, e.Summary)
				if e.Author != "" {
					line += " by " + e.Author
				}
				fmt.Println(line)
			}
			if len(k8s) > 0 {
				fmt.Println("\nkubernetes events:")
				for _, l := range k8s {
					fmt.Println("  " + l)
				}
			}
			if len(evs) == 0 && len(k8s) == 0 {
				fmt.Println("no events in the last", since)
			}
			return nil
		},
	}
	c.Flags().DurationVar(&since, "since", 2*time.Hour, "how far back")
	return c
}

func annotateCmd() *cobra.Command {
	var path, note, confidence, id, source string
	var ttl time.Duration
	c := &cobra.Command{
		Use:   "annotate --path <ref,...> --note <text>",
		Short: "draw a highlighted path with a note on the diagram",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if path == "" || note == "" {
				return fmt.Errorf("--path and --note are required")
			}
			var refs []string
			for _, p := range strings.Split(path, ",") {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				r, err := model.ParseRef(p)
				if err != nil {
					return err
				}
				r, err = cfg.Topology.Resolve(r)
				if err != nil {
					return err
				}
				refs = append(refs, r.String())
			}
			if len(refs) == 0 {
				return fmt.Errorf("--path names no element")
			}
			switch confidence {
			case "low", "medium", "high":
			default:
				return fmt.Errorf("--confidence must be low, medium or high")
			}
			a, err := model.LoadAnnotations(cfg.Dir)
			if err != nil {
				return err
			}
			now := time.Now()
			if id == "" {
				id = fmt.Sprintf("a%s", now.Format("150405"))
			}
			an := model.Annotation{ID: id, Path: refs, Note: note, Confidence: confidence, Source: source, CreatedAt: now}
			if ttl > 0 {
				an.ExpiresAt = now.Add(ttl)
			}
			kept := a.Live(now)
			replaced := false
			for i := range kept {
				if kept[i].ID == id {
					kept[i] = an
					replaced = true
				}
			}
			if !replaced {
				kept = append(kept, an)
			}
			a.Annotations = kept
			if err := model.SaveAnnotations(cfg.Dir, a); err != nil {
				return err
			}
			if flags.jsonOut {
				return printJSON(an)
			}
			fmt.Printf("annotation %s drawn on %d element(s)\n", id, len(refs))
			return nil
		},
	}
	c.Flags().StringVar(&path, "path", "", "comma separated refs or ids to highlight, in order")
	c.Flags().StringVar(&note, "note", "", "the note shown in the callout")
	c.Flags().StringVar(&confidence, "confidence", "medium", "low, medium or high")
	c.Flags().StringVar(&id, "id", "", "annotation id (replaces an existing one)")
	c.Flags().StringVar(&source, "source", "claude-code", "who drew it")
	c.Flags().DurationVar(&ttl, "ttl", 2*time.Hour, "expiry (0 = never)")
	return c
}

func clearAnnotationsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear-annotations",
		Short: "remove every annotation",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := findDir()
			if err != nil {
				return err
			}
			if err := model.SaveAnnotations(dir, model.Annotations{Version: model.Version}); err != nil {
				return err
			}
			if flags.jsonOut {
				return printJSON(map[string]any{"ok": true})
			}
			fmt.Println("annotations cleared")
			return nil
		},
	}
}

func exportCmd() *cobra.Command {
	var png, svg, lens bool
	c := &cobra.Command{
		Use:   "export",
		Short: "render the current diagram to svg, png and txt files and print their paths",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			snap, _ := model.LoadSnapshot(cfg.Dir)
			g := layout.Compute(&cfg.Topology, cfg.Layout, layout.DefaultOptions())
			opts := render.DrawOptions{Topology: &cfg.Topology, Snapshot: snap}
			var story []string
			if lens && snap != nil && len(snap.Issues) > 0 {
				opts.Lens = &snap.Issues[0]
				story = snap.Issues[0].Story
			}
			if a, err := model.LoadAnnotations(cfg.Dir); err == nil {
				opts.Annotations = a.Live(time.Now())
			}
			canvas := render.Draw(g, opts)
			dir, err := history.NewStore(cfg.Dir).ExportDir()
			if err != nil {
				return err
			}
			res := render.Export(dir, canvas, story, time.Now())
			if !png {
				res.PNG = ""
			}
			if !svg && png && res.PNG != "" {
				res.SVG = ""
			}
			if flags.jsonOut {
				return printJSON(res)
			}
			for _, p := range []string{res.SVG, res.PNG, res.TXT} {
				if p != "" {
					fmt.Println(p)
				}
			}
			if res.Err != "" {
				fmt.Fprintln(os.Stderr, "note:", res.Err)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&png, "png", true, "write a png (needs resvg or rsvg-convert)")
	c.Flags().BoolVar(&svg, "svg", true, "write an svg")
	c.Flags().BoolVar(&lens, "lens", true, "draw the issue lens when there is an issue")
	return c
}

func remapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remap <old-id> <new-id>",
		Short: "rename an id across topology, bindings, findings, thresholds, layout and annotations",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := findDir()
			if err != nil {
				return err
			}
			touched, err := model.Remap(dir, args[0], args[1])
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return printJSON(map[string]any{"old": args[0], "new": args[1], "files": touched})
			}
			fmt.Printf("renamed %s to %s in %s\n", args[0], args[1], strings.Join(touched, ", "))
			return nil
		},
	}
}

func replayCmd() *cobra.Command {
	var speed float64
	var check bool
	c := &cobra.Command{
		Use:   "replay <fixture-dir>",
		Short: "run the TUI against recorded observations; --check verifies expected.yaml offline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReplay(args[0], speed, check)
		},
	}
	c.Flags().Float64Var(&speed, "speed", 1, "clock multiplier")
	c.Flags().BoolVar(&check, "check", false, "play offline and compare with expected.yaml instead of drawing")
	return c
}

func runReplay(dir string, speed float64, check bool) error {
	if check {
		run, err := scenario.Load(dir)
		if err != nil {
			return err
		}
		snap := run.Play()
		problems := run.Check()
		if flags.jsonOut {
			return printJSON(map[string]any{"ok": len(problems) == 0, "problems": problems, "issues": snap.Issues})
		}
		printSnapshot(snap)
		if len(problems) > 0 {
			fmt.Println()
			for _, p := range problems {
				fmt.Println("  mismatch: " + p)
			}
			return fmt.Errorf("%d mismatch(es)", len(problems))
		}
		fmt.Println("\nok: matches expected.yaml")
		return nil
	}
	fx, err := fixture.Load(dir)
	if err != nil {
		return err
	}
	rt, err := app.New(app.Options{Dir: dir, Replay: fx, Speed: speed, ReadOnly: true})
	if err != nil {
		return err
	}
	return runProgram(rt, render.Options{NoColor: flags.noColor, Split: flags.split})
}

func demoCmd() *cobra.Command {
	var speed float64
	var list bool
	c := &cobra.Command{
		Use:   "demo [scenario]",
		Short: "replay a bundled scenario with no cluster (default: the connection pool one)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if list {
				for _, n := range demo.List() {
					fmt.Println(n)
				}
				return nil
			}
			sel := ""
			if len(args) == 1 {
				sel = args[0]
			}
			dir, err := demo.Extract(sel)
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			return runReplay(dir, speed, false)
		},
	}
	c.Flags().Float64Var(&speed, "speed", 1, "clock multiplier")
	c.Flags().BoolVar(&list, "list", false, "list bundled scenarios")
	return c
}

func skillCmd() *cobra.Command {
	var target string
	install := &cobra.Command{
		Use:   "install",
		Short: "copy the skill into the current project (.claude/skills/wassup/)",
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := filepath.Join(target, "wassup")
			n := 0
			err := fs.WalkDir(skill.FS, "wassup", func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel := strings.TrimPrefix(p, "wassup")
				out := filepath.Join(dest, rel)
				if d.IsDir() {
					return os.MkdirAll(out, 0o755)
				}
				b, err := skill.FS.ReadFile(p)
				if err != nil {
					return err
				}
				n++
				return os.WriteFile(out, b, 0o644)
			})
			if err != nil {
				return err
			}
			// The JSON Schemas come from the binary too, so they never drift.
			schemaDir := filepath.Join(dest, "reference", "schema")
			if err := os.MkdirAll(schemaDir, 0o755); err != nil {
				return err
			}
			for _, name := range model.SchemaNames {
				b, err := model.SchemaFS().ReadFile("schema/" + name + ".json")
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(schemaDir, name+".json"), b, 0o644); err != nil {
					return err
				}
				n++
			}
			if flags.jsonOut {
				return printJSON(map[string]any{"installed": dest, "files": n})
			}
			fmt.Printf("installed the wassup skill to %s (%d files)\n", dest, n)
			return nil
		},
	}
	install.Flags().StringVar(&target, "dir", filepath.Join(".claude", "skills"), "skills directory")
	c := &cobra.Command{Use: "skill", Short: "manage the Claude Code skill"}
	c.AddCommand(install)
	return c
}

func probesCmd() *cobra.Command {
	var markdown bool
	c := &cobra.Command{
		Use:   "probes",
		Short: "list the probe kinds this build ships and what they need",
		RunE: func(cmd *cobra.Command, args []string) error {
			all := probe.AllAccess()
			if flags.jsonOut {
				return printJSON(all)
			}
			if markdown {
				fmt.Println("| Probe | Facets | Source | Delivers | Spec fields | Needs | Status |")
				fmt.Println("| --- | --- | --- | --- | --- | --- | --- |")
				for _, a := range all {
					status := "shipped"
					if !a.Implemented {
						status = "spec only, not implemented yet"
					}
					esc := func(s string) string { return strings.ReplaceAll(s, "|", "\\|") }
					fmt.Printf("| `%s` | %s | %s | %s | %s | %s | %s |\n", a.Kind, strings.Join(a.Facets, ", "), esc(a.Source), esc(a.Delivers), "`"+strings.Join(a.SpecFields, "`, `")+"`", esc(a.Needs), status)
				}
				return nil
			}
			for _, a := range all {
				impl := ""
				if !a.Implemented {
					impl = " (not implemented in this build)"
				}
				fmt.Printf("%-18s %s%s\n", a.Kind, a.Delivers, impl)
				fmt.Printf("%-18s   source: %s\n", "", a.Source)
				fmt.Printf("%-18s   spec: %s\n", "", strings.Join(a.SpecFields, ", "))
				fmt.Printf("%-18s   needs: %s\n", "", a.Needs)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&markdown, "markdown", false, "print a markdown table (used for the skill reference)")
	return c
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print the version",
		Run: func(cmd *cobra.Command, args []string) {
			if flags.jsonOut {
				_ = printJSON(map[string]string{"version": Version})
				return
			}
			fmt.Println("wassup", Version)
		},
	}
}
