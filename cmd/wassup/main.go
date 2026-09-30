// Command wassup shows one live architecture diagram of a Kubernetes-hosted
// system and answers three questions at a glance: where is it stuck, since
// when, what changed.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/danilopopovikj/wassup/internal/app"
	"github.com/danilopopovikj/wassup/internal/model"
	_ "github.com/danilopopovikj/wassup/internal/probe/all"
	"github.com/danilopopovikj/wassup/internal/render"
	"github.com/danilopopovikj/wassup/prompts"
)

// Version is set by goreleaser and the Makefile. A binary built by
// `go install ...@version` has no such flag; version reads the module
// version the toolchain recorded instead.
var Version = "dev"

// version returns the version to print.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	return versionFrom(Version, info)
}

// versionFrom picks the version set at link time, else the module version,
// else the commit the binary was built from.
func versionFrom(linked string, info *debug.BuildInfo) string {
	if linked != "" && linked != "dev" {
		return linked
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		return "dev-" + rev + dirty
	}
	return "dev"
}

var flags struct {
	dir        string
	kubeconfig string
	kcontext   string
	envFile    string
	noColor    bool
	jsonOut    bool
	split      int
	// clusterChosen is set when the kubeconfig or the context was named for
	// wassup, by a flag or in the settings, and not left to the default of
	// the machine.
	clusterChosen bool
}

func main() {
	root := &cobra.Command{
		Use:   "wassup",
		Short: "one live architecture diagram: where is it stuck, since when, what changed",
		Long: `wassup is a terminal app that shows one live architecture diagram of a
Kubernetes-hosted system. Every node and edge is always in exactly one of six
states: flowing, idle, waiting, processing, blocked, failing.

Claude Code writes the configuration in .wassup/, reads the snapshot and
draws on the diagram; wassup itself needs no AI to run.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runTUI,
		// Every command starts with the settings of this machine, so
		// nothing has to be exported before wassup runs.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return loadSettings() },
	}
	pf := root.PersistentFlags()
	pf.StringVar(&flags.dir, "dir", "", "path to the .wassup directory (default: found upward from the cwd)")
	pf.StringVar(&flags.kubeconfig, "kubeconfig", "", "kubeconfig path for k8s probes (default: $WASSUP_KUBECONFIG, $KUBECONFIG or ~/.kube/config)")
	pf.StringVar(&flags.kcontext, "context", "", "kubeconfig context (default: $WASSUP_CONTEXT or the current context)")
	pf.StringVar(&flags.envFile, "env-file", "", "a file of NAME=value lines to read before .wassup/local.env")
	pf.BoolVar(&flags.jsonOut, "json", false, "machine readable output")
	root.Flags().BoolVar(&flags.noColor, "no-color", false, "glyphs only, no colors")
	root.Flags().IntVar(&flags.split, "split", 0, "graph width in percent when the panel is open")

	root.AddCommand(
		initCmd(), discoverCmd(), validateCmd(), probeCmd(), snapshotCmd(), explainCmd(),
		logsCmd(), eventsCmd(), annotateCmd(), clearAnnotationsCmd(), exportCmd(), remapCmd(),
		replayCmd(), skillCmd(), demoCmd(), probesCmd(), syncCmd(), versionCmd(), accessCmd(), measureCmd(), updateCmd(),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "wassup:", err)
		if _, ok := err.(*model.ValidationError); ok {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// findDir resolves the .wassup directory from --dir or by walking upward.
func findDir() (string, error) {
	if flags.dir != "" {
		abs, err := filepath.Abs(flags.dir)
		if err != nil {
			return "", err
		}
		if filepath.Base(abs) != model.DirName {
			cand := filepath.Join(abs, model.DirName)
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				return cand, nil
			}
		}
		return abs, nil
	}
	return model.FindDir(".")
}

func runTUI(cmd *cobra.Command, args []string) error {
	dir, err := findDir()
	if err != nil {
		// First run: print the setup prompt and copy it to the clipboard.
		fmt.Println("No .wassup/ directory found. Paste this into Claude Code to set one up:")
		fmt.Println()
		fmt.Println(prompts.Setup)
		render.CopyText(prompts.Setup)
		fmt.Fprintln(os.Stderr, "(the prompt was copied to the clipboard; `wassup init` creates .wassup/ with it)")
		printPathHint()
		return nil
	}
	rt, err := app.New(app.Options{Dir: dir, Kubeconfig: flags.kubeconfig, Context: flags.kcontext, Logf: logf(dir)})
	if err != nil {
		return err
	}
	return runProgram(rt, render.Options{NoColor: flags.noColor, Split: flags.split})
}

func runProgram(rt *app.Runtime, opts render.Options) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		return err
	}
	defer rt.Stop()
	m := render.New(rt, opts)
	p := tea.NewProgram(m, tea.WithContext(ctx))
	_, err := p.Run()
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}

// logf appends runtime diagnostics to state/wassup.log.
func logf(dir string) func(string, ...any) {
	return func(format string, args ...any) {
		p := filepath.Join(dir, "state", "wassup.log")
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, format+"\n", args...)
	}
}

// printPathHint tells a person who ran wassup by its full path how to run
// it by its name.
func printPathHint() {
	if hint := pathHint(); hint != "" {
		fmt.Fprintln(os.Stderr, "\nnote: "+hint)
	}
}
