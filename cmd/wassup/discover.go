package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danilopopovikj/wassup/internal/discover"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

// scanRepo runs the repository scanners and, unless --no-cluster, adds the
// live cluster's inventory as evidence.
func scanRepo(ctx context.Context, repo string, namespaces []string, noCluster bool) (*discover.Findings, *k8s.Inventory, error) {
	f, err := discover.Scan(discover.Options{Root: repo, Namespaces: namespaces})
	if err != nil {
		return nil, nil, err
	}
	var inv *k8s.Inventory
	if !noCluster {
		c, err := k8s.NewClients(flags.kubeconfig, flags.kcontext)
		if err != nil {
			f.Notes = append(f.Notes, "cluster not read: "+err.Error())
		} else {
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			inv, err = k8s.Discover(cctx, c, namespaces)
			cancel()
			if err != nil {
				f.Notes = append(f.Notes, "cluster not read: "+err.Error())
			} else {
				discover.AddCluster(f, inv)
			}
		}
	}
	return f, inv, nil
}

func discoverCmd() *cobra.Command {
	var namespaces []string
	var repo string
	var propose, write, noCluster bool
	var name, defaultNS string
	c := &cobra.Command{
		Use:   "discover",
		Short: "read the repository and the cluster for what exists and how data flows; --propose drafts topology and bindings",
		Long: `discover reads Terraform (*.tf), Kubernetes manifests, Helm values, .env
files and application code for components and the connections between them
(connection strings, service names, celery queues, Hatchet workflows, Electric
shapes, external APIs), and merges the live cluster's inventory (nodes,
workloads with their env and commands, services, ingresses, CNPG clusters,
cron jobs) when a kubeconfig is reachable.

Without --propose it prints the raw evidence. With --propose it prints a
draft topology.yaml and bindings.yaml built from that evidence; --write puts
them under .wassup/proposed/ together with evidence.json so Claude Code can
review every component and edge against the file and line it came from.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, inv, err := scanRepo(context.Background(), repo, namespaces, noCluster)
			if err != nil {
				return err
			}
			if !propose && !write {
				if flags.jsonOut {
					return printJSON(map[string]any{"repository": f, "kubernetes": inv})
				}
				printFindings(f)
				return nil
			}
			p := discover.Propose(f, discover.ProposeOptions{Name: name, Namespace: defaultNS})
			if write {
				dir := flags.dir
				if dir == "" {
					dir = model.DirName
				}
				out := filepath.Join(dir, "proposed")
				if err := os.MkdirAll(out, 0o755); err != nil {
					return err
				}
				if err := model.WriteYAML(filepath.Join(out, "topology.yaml"), p.Topology); err != nil {
					return err
				}
				if err := model.WriteYAML(filepath.Join(out, "bindings.yaml"), p.Bindings); err != nil {
					return err
				}
				if err := model.WriteJSON(filepath.Join(out, "evidence.json"), map[string]any{"evidence": p.Evidence, "unresolved": p.Unresolved, "notes": p.Notes, "findings": f}); err != nil {
					return err
				}
				if flags.jsonOut {
					return printJSON(map[string]any{"written": out, "components": len(p.Topology.Components), "edges": len(p.Topology.Edges), "unresolved": p.Unresolved, "notes": p.Notes})
				}
				fmt.Printf("wrote %s/{topology.yaml,bindings.yaml,evidence.json}: %d components, %d edges", out, len(p.Topology.Components), len(p.Topology.Edges))
				if len(p.Unresolved) > 0 {
					fmt.Printf(", %d unresolved hosts", len(p.Unresolved))
				}
				fmt.Println()
				printNotes(p)
				return nil
			}
			if flags.jsonOut {
				return printJSON(p)
			}
			fmt.Println("# topology.yaml (proposed)")
			b, _ := yamlBytes(p.Topology)
			fmt.Print(string(b))
			fmt.Println("# bindings.yaml (proposed)")
			b, _ = yamlBytes(p.Bindings)
			fmt.Print(string(b))
			printNotes(p)
			return nil
		},
	}
	c.Flags().StringSliceVarP(&namespaces, "namespace", "n", nil, "namespaces to inspect (default all but kube-system)")
	c.Flags().StringVar(&repo, "repo", ".", "repository root to scan")
	c.Flags().BoolVar(&propose, "propose", false, "print a proposed topology.yaml and bindings.yaml")
	c.Flags().BoolVar(&write, "write", false, "write the proposal under .wassup/proposed/")
	c.Flags().BoolVar(&noCluster, "no-cluster", false, "do not read the cluster, repository only")
	c.Flags().StringVar(&name, "name", "", "system name for the proposal (default: repository directory)")
	c.Flags().StringVar(&defaultNS, "default-namespace", "default", "namespace for probes when the manifest has none")
	return c
}

func printFindings(f *discover.Findings) {
	fmt.Printf("scanned %d files under %s\n", f.FilesRead, f.Root)
	if len(f.Providers) > 0 {
		fmt.Println("terraform providers:", strings.Join(f.Providers, ", "))
	}
	fmt.Printf("\n%d candidates:\n", len(f.Candidates))
	for _, c := range f.Candidates {
		fmt.Printf("  %-14s %-24s %s\n", c.Type, c.ID, c.Label)
		for i, e := range c.Evidence {
			if i >= 3 {
				fmt.Printf("      … %d more\n", len(c.Evidence)-3)
				break
			}
			fmt.Printf("      %s\n", e)
		}
	}
	fmt.Printf("\n%d links:\n", len(f.Links))
	for _, l := range f.Links {
		to := l.To
		if to == "" {
			to = "? " + l.Host
		}
		fmt.Printf("  %-20s → %-28s %s\n", l.From, to, l.Kind)
		if len(l.Evidence) > 0 {
			fmt.Printf("      %s\n", l.Evidence[0])
		}
	}
	if len(f.Notes) > 0 {
		fmt.Println("\nnotes:")
		for _, n := range f.Notes {
			fmt.Println("  " + n)
		}
	}
}

func printNotes(p *discover.Proposal) {
	if len(p.Unresolved) > 0 {
		fmt.Println("unresolved hosts (name the component they belong to, or add it):")
		for _, l := range p.Unresolved {
			fmt.Printf("  %s → %s (%s)\n", l.From, l.Host, l.Evidence[0])
		}
	}
	if len(p.Notes) > 0 {
		fmt.Println("notes:")
		for _, n := range p.Notes {
			fmt.Println("  " + n)
		}
	}
}

func yamlBytes(v any) ([]byte, error) {
	tmp, err := os.CreateTemp("", "wassup-*.yaml")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	if err := model.WriteYAML(name, v); err != nil {
		return nil, err
	}
	return os.ReadFile(name)
}

func syncCmd() *cobra.Command {
	var namespaces []string
	var repo string
	var apply, prune, check, noCluster bool
	c := &cobra.Command{
		Use:   "sync",
		Short: "re-discover and report what the repository and cluster have that .wassup/ does not (and the other way round)",
		Long: `sync runs discovery again and compares the proposal with the committed
topology.yaml and bindings.yaml. It lists added components and edges (with
the evidence), components no longer found, and components left unbound.

--apply merges the additions: new components, edges and bindings are
appended; existing labels, groups, notes and layout.json are never touched;
new components take auto positions. --prune also removes components the scan
no longer finds. --check exits 4 when there is drift, for CI.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			f, _, err := scanRepo(context.Background(), repo, namespaces, noCluster)
			if err != nil {
				return err
			}
			p := discover.Propose(f, discover.ProposeOptions{Name: cfg.Topology.Name})
			d := discover.Diff(cfg, p)
			if flags.jsonOut && !apply {
				return printJSON(map[string]any{"drift": !d.Empty(), "changes": d.Changes, "unresolved": p.Unresolved, "notes": p.Notes})
			}
			if d.Empty() {
				fmt.Println("in sync: the repository and the cluster show nothing .wassup/ does not have")
			}
			for _, ch := range d.Changes {
				sign := map[string]string{"added": "+", "removed": "-", "changed": "~"}[ch.Op]
				fmt.Printf("%s %-9s %-28s %s\n", sign, ch.What, ch.ID, ch.Detail)
				for i, e := range ch.Evidence {
					if i >= 2 {
						break
					}
					fmt.Printf("      %s\n", e)
				}
			}
			printNotes(p)
			if apply {
				added, addedEdges := discover.Apply(cfg, p, d, prune)
				if err := model.WriteYAML(filepath.Join(cfg.Dir, "topology.yaml"), cfg.Topology); err != nil {
					return err
				}
				if err := model.WriteYAML(filepath.Join(cfg.Dir, "bindings.yaml"), cfg.Bindings); err != nil {
					return err
				}
				if prune {
					if err := cfg.SaveLayout(); err != nil {
						return err
					}
				}
				if _, err := model.Load(cfg.Dir); err != nil {
					fmt.Fprintln(os.Stderr, "applied, but validation now fails; fix before running wassup:")
					return err
				}
				if flags.jsonOut {
					return printJSON(map[string]any{"applied": true, "components_added": added, "edges_added": addedEdges, "changes": d.Changes})
				}
				fmt.Printf("\napplied: %d components and %d edges added; run `wassup validate` and `wassup probe --once`\n", added, addedEdges)
				return nil
			}
			if check && !d.Empty() {
				os.Exit(4)
			}
			return nil
		},
	}
	c.Flags().StringSliceVarP(&namespaces, "namespace", "n", nil, "namespaces to inspect")
	c.Flags().StringVar(&repo, "repo", ".", "repository root to scan")
	c.Flags().BoolVar(&apply, "apply", false, "merge additions into topology.yaml and bindings.yaml")
	c.Flags().BoolVar(&prune, "prune", false, "with --apply, also remove components no longer found")
	c.Flags().BoolVar(&check, "check", false, "exit 4 when there is drift")
	c.Flags().BoolVar(&noCluster, "no-cluster", false, "do not read the cluster, repository only")
	return c
}

// unused guard for json import when jsonOut paths are compiled out
var _ = json.Marshal
