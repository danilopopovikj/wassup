package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

// exitNeedsAnswer is the exit code of a command that stopped for a question
// it could not ask: nothing was read, and the answer goes in a flag.
const exitNeedsAnswer = 5

// describeTimeout bounds the look at the cluster before it is read.
const describeTimeout = 10 * time.Second

// needsAnswer is the error of a command that stopped for a question.
type needsAnswer struct {
	// Question is what has to be decided, Answers the flags that decide it.
	Question string   `json:"question"`
	Answers  []string `json:"answers"`
	// Cluster and Kubeconfigs are set when the question is which cluster.
	Cluster     *k8s.ClusterInfo     `json:"cluster,omitempty"`
	Kubeconfigs []k8s.KubeconfigFile `json:"kubeconfigs_in_repository,omitempty"`
	// Environments is set when the question is which environment.
	Environments []string `json:"environments,omitempty"`
}

func (n *needsAnswer) Error() string { return n.Question }

// interactive reports whether there is a person at the terminal to ask.
func interactive() bool {
	if flags.jsonOut {
		return false
	}
	for _, f := range []*os.File{os.Stdin, os.Stderr} {
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return true
}

// clusterChoice is a kubeconfig and a context offered to the user.
type clusterChoice struct {
	Kubeconfig, Context, Server string
}

// clusterChoices lists the contexts of the kubeconfigs found in the
// repository. The paths are relative to the working directory when they are
// below it, so an answer can be passed to --kubeconfig as it is printed.
func clusterChoices(found []k8s.KubeconfigFile) []clusterChoice {
	cwd, _ := os.Getwd()
	var out []clusterChoice
	for _, k := range found {
		path := k.Path
		if rel, err := filepath.Rel(cwd, k.Path); cwd != "" && err == nil && !strings.HasPrefix(rel, "..") {
			path = rel
		}
		for _, c := range k.Contexts {
			out = append(out, clusterChoice{Kubeconfig: path, Context: c, Server: k.Servers[c]})
		}
	}
	return out
}

// announceCluster prints the cluster that is about to be read. It returns
// the cluster, or an error when the kubeconfig and the context lead to none.
func announceCluster(ctx context.Context, w io.Writer) (k8s.ClusterInfo, error) {
	dctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	info, err := k8s.Describe(dctx, flags.kubeconfig, flags.kcontext)
	if err != nil {
		return info, err
	}
	if !flags.jsonOut {
		fmt.Fprintln(w, "cluster: "+info.String())
	}
	return info, nil
}

// confirmCluster makes sure the cluster that is about to be read is the one
// the user means. When the kubeconfig or the context was named for wassup it
// only prints the cluster. When they are the defaults of the machine it
// asks, and offers the kubeconfigs of the repository: the default context is
// the cluster somebody worked on last, which may belong to another project.
// Without a person to ask it stops with exitNeedsAnswer and reads nothing.
func confirmCluster(ctx context.Context, repo string, yes bool, in io.Reader, w io.Writer) error {
	info, err := announceCluster(ctx, w)
	if err != nil {
		return err
	}
	if flags.clusterChosen || yes {
		return nil
	}
	root, err := filepath.Abs(repo)
	if err != nil {
		root = repo
	}
	found := k8s.FindKubeconfigs(root)
	choices := clusterChoices(found)
	if !interactive() {
		q := &needsAnswer{
			Question: "the kubeconfig and the context are the defaults of this machine; confirm that this is the cluster to read",
			Answers:  []string{"--context " + orPlaceholder(info.Context, "<name>") + " (this cluster)", "--yes (this cluster)", "--no-cluster (the repository only)"},
			Cluster:  &info, Kubeconfigs: found,
		}
		for _, c := range choices {
			q.Answers = append(q.Answers, fmt.Sprintf("--kubeconfig %s --context %s (%s)", c.Kubeconfig, c.Context, c.Server))
		}
		return q
	}
	return askCluster(info, choices, in, w)
}

func orPlaceholder(s, placeholder string) string {
	if s == "" {
		return placeholder
	}
	return s
}

// askCluster asks at the terminal and sets the kubeconfig and the context
// to the answer.
func askCluster(info k8s.ClusterInfo, choices []clusterChoice, in io.Reader, w io.Writer) error {
	if len(choices) > 0 {
		fmt.Fprintln(w, "\nkubeconfigs in this repository:")
		for i, c := range choices {
			fmt.Fprintf(w, "  %d  %s, context %s, server %s\n", i+1, c.Kubeconfig, c.Context, orPlaceholder(c.Server, "not known"))
		}
		fmt.Fprintf(w, "\nRead the cluster above (y), one of these (1-%d), or none (n)? ", len(choices))
	} else {
		fmt.Fprint(w, "\nRead this cluster (y), or none (n)? ")
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("no answer: nothing was read")
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	switch answer {
	case "y", "yes":
		return nil
	case "n", "no", "":
		return fmt.Errorf("the cluster was not read; name it with --kubeconfig and --context, or scan the repository alone with --no-cluster")
	}
	n, err := strconv.Atoi(answer)
	if err != nil || n < 1 || n > len(choices) {
		return fmt.Errorf("%q is not an answer; nothing was read", answer)
	}
	flags.kubeconfig, flags.kcontext, flags.clusterChosen = choices[n-1].Kubeconfig, choices[n-1].Context, true
	_, err = announceCluster(context.Background(), w)
	return err
}

// exitOnQuestion prints a question that could not be asked, with the flags
// that answer it, and ends the run with exitNeedsAnswer.
func exitOnQuestion(err error) {
	q, ok := err.(*needsAnswer)
	if !ok {
		return
	}
	if flags.jsonOut {
		_ = printJSON(map[string]any{"ok": false, "needs_answer": q})
	} else {
		fmt.Fprintln(os.Stderr, "wassup: "+q.Question+". Nothing was read. Run it again with one of:")
		for _, a := range q.Answers {
			fmt.Fprintln(os.Stderr, "  "+a)
		}
	}
	os.Exit(exitNeedsAnswer)
}
