package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danilopopovikj/wassup/internal/discover"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

// scanOptions say what a scan reads.
type scanOptions struct {
	Repo       string
	Namespaces []string
	NoCluster  bool
	// Confirm asks whether the cluster is the right one before it is read,
	// unless it was named for wassup or Yes is set.
	Confirm, Yes bool
	// Environment is the one environment to read; "" reads all but the
	// local ones. Draft is set for a scan a draft is built from, which is
	// of one environment: when the repository has several and none was
	// named, the scan asks.
	Environment string
	Draft       bool
}

// scanRepo runs the repository scanners and, unless --no-cluster, adds the
// live cluster's inventory as evidence. The cluster is named before it is
// read.
func scanRepo(ctx context.Context, o scanOptions) (*discover.Findings, *k8s.Inventory, error) {
	repo, namespaces, noCluster := o.Repo, o.Namespaces, o.NoCluster
	f, err := discover.Scan(discover.Options{Root: repo, Namespaces: namespaces, Environment: o.Environment})
	if err != nil {
		return nil, nil, err
	}
	if o.Draft && o.Environment == "" {
		env, err := chooseEnvironment(f.Environments, os.Stdin, os.Stderr)
		if err != nil {
			return nil, nil, err
		}
		if env != "" {
			if f, err = discover.Scan(discover.Options{Root: repo, Namespaces: namespaces, Environment: env}); err != nil {
				return nil, nil, err
			}
		}
	}
	var inv *k8s.Inventory
	if !noCluster {
		switch err := nameCluster(ctx, o); err.(type) {
		case nil:
		case *needsAnswer:
			return nil, nil, err
		default:
			// No cluster to read is not the end of a scan: the repository
			// alone is evidence too, and the note says what is missing.
			f.Notes = append(f.Notes, "cluster not read: "+err.Error())
			return f, nil, nil
		}
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

// chooseEnvironment returns the environment to draft when the repository
// has several that are not for a developer's machine, by asking. A draft of
// all of them at once shows every component as many times as there are
// environments. With one or none there is nothing to choose and it returns
// "". Without a person to ask it stops with exitNeedsAnswer.
func chooseEnvironment(envs []discover.Environment, in io.Reader, w io.Writer) (string, error) {
	var names []string
	for _, e := range envs {
		if !e.Local {
			names = append(names, e.Name)
		}
	}
	if len(names) < 2 {
		return "", nil
	}
	if !interactive() {
		q := &needsAnswer{Question: "the repository has " + strconv.Itoa(len(names)) + " environments and a draft is of one; name it", Environments: names}
		for _, n := range names {
			q.Answers = append(q.Answers, "--environment "+n)
		}
		return "", q
	}
	fmt.Fprintln(w, "environments in this repository:")
	for _, e := range envs {
		if !e.Local {
			fmt.Fprintf(w, "  %s  (%s)\n", e.Name, strings.Join(e.Paths, ", "))
		}
	}
	fmt.Fprintf(w, "\nWhich one is the draft for (%s)? ", strings.Join(names, ", "))
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no answer: nothing was drafted")
	}
	answer := strings.TrimSpace(line)
	for _, n := range names {
		if strings.EqualFold(answer, n) {
			return n, nil
		}
	}
	return "", fmt.Errorf("%q is not one of %s; nothing was drafted", answer, strings.Join(names, ", "))
}

// nameCluster prints the cluster a scan is about to read and, for a scan
// that confirms, asks whether it is the right one.
func nameCluster(ctx context.Context, o scanOptions) error {
	if o.Confirm {
		return confirmCluster(ctx, o.Repo, o.Yes, os.Stdin, os.Stderr)
	}
	_, err := announceCluster(ctx, os.Stderr)
	return err
}

func discoverCmd() *cobra.Command {
	var namespaces []string
	var repo string
	var propose, write, noCluster, yes bool
	var name, defaultNS, environment string
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
them under .wassup/proposed/ together with review.md and evidence.json, and
makes sure .wassup/.gitignore ignores proposed/, state/ and local.env.
review.md is what to review: one line and one citation per component and
edge, with how sure the scan is and whether the cluster proves it or the
repository hints at it. evidence.json is the whole record behind it.

Before the cluster is read, discover prints which one it is: the kubeconfig,
the context, the server and the number of nodes. When the kubeconfig and the
context are the defaults of the machine it asks, and offers the kubeconfigs
it finds in the repository. A draft is of one environment: when the
repository has several, discover asks which one. Without a terminal to ask
at, it exits 5, reads nothing, and lists the flags that answer.

Of an environment variable discover keeps the name and the host it points
at, never the value: no password, token or key, and no user, path or query
of a URL, whether it comes from a manifest, a ConfigMap, a pod spec or a
command line. Secrets are never read.

It leaves out what git ignores (.gitignore, .git/info/exclude), lockfiles,
tests, nested checkouts (worktrees, submodules) and Kustomize overlays for
local development; the notes say which directories were skipped.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, inv, err := scanRepo(context.Background(), scanOptions{Repo: repo, Namespaces: namespaces, NoCluster: noCluster, Confirm: true, Yes: yes,
				Environment: environment, Draft: propose || write})
			if err != nil {
				exitOnQuestion(err)
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
				// WriteProposal also makes sure .wassup/.gitignore keeps
				// proposed/ and state/ out of git.
				if err := discover.WriteProposal(dir, p, f); err != nil {
					return err
				}
				out := filepath.Join(dir, discover.ProposedDir)
				if flags.jsonOut {
					return printJSON(map[string]any{"written": out, "review": filepath.Join(out, discover.ReviewFile), "environment": f.Environment,
						"components": len(p.Topology.Components), "edges": len(p.Topology.Edges), "confidence": p.Confidence, "unresolved": p.Unresolved, "notes": p.Notes})
				}
				fmt.Printf("wrote %s/{topology.yaml,bindings.yaml,review.md,evidence.json}: %d components, %d edges", out, len(p.Topology.Components), len(p.Topology.Edges))
				if len(p.Unresolved) > 0 {
					fmt.Printf(", %d unresolved hosts", len(p.Unresolved))
				}
				fmt.Println()
				printNotes(p)
				fmt.Printf("\nreview %s first: one line and one citation per component and edge, the least certain at the end\n", filepath.Join(out, discover.ReviewFile))
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
	c.Flags().BoolVar(&yes, "yes", false, "read the cluster of the default kubeconfig and context without asking")
	c.Flags().StringVar(&environment, "environment", "", "the environment to read (an overlay, a values file, a Terraform directory); the others are left out")
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
			f, _, err := scanRepo(context.Background(), scanOptions{Repo: repo, Namespaces: namespaces, NoCluster: noCluster})
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
