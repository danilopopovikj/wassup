package k8s

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// ClusterInfo says which cluster a kubeconfig and a context lead to, in the
// words a person checks before anything is read: a default context that
// belongs to another project maps the wrong system without an error.
type ClusterInfo struct {
	// Kubeconfig is the file that was used; "" is the service account of
	// the pod wassup runs in.
	Kubeconfig string `json:"kubeconfig,omitempty"`
	Context    string `json:"context,omitempty"`
	Server     string `json:"server,omitempty"`
	// Nodes is the number of nodes; it is only known when NodesErr is "".
	Nodes    int    `json:"nodes"`
	NodesErr string `json:"nodes_error,omitempty"`
}

// String is the cluster in one line.
func (c ClusterInfo) String() string {
	var b strings.Builder
	if c.Context != "" {
		fmt.Fprintf(&b, "context %s, ", c.Context)
	}
	fmt.Fprintf(&b, "server %s, ", orUnknown(c.Server))
	if c.NodesErr != "" {
		b.WriteString("nodes not read (" + c.NodesErr + ")")
	} else {
		fmt.Fprintf(&b, "%d nodes", c.Nodes)
	}
	if c.Kubeconfig != "" {
		b.WriteString(", kubeconfig " + c.Kubeconfig)
	} else {
		b.WriteString(", the service account of this pod")
	}
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "not known"
	}
	return s
}

// Describe resolves a kubeconfig and a context the way the probes do and
// counts the nodes of the cluster they lead to. The file and the context
// are read offline; an error is returned when they name no cluster. A
// cluster that does not answer is not an error: NodesErr says why.
func Describe(ctx context.Context, kubeconfig, kubeContext string) (ClusterInfo, error) {
	info := ClusterInfo{Kubeconfig: kubeconfigPath(kubeconfig), Context: kubeContext}
	if info.Kubeconfig != "" {
		rules := &clientcmd.ClientConfigLoadingRules{Precedence: filepath.SplitList(info.Kubeconfig)}
		raw, err := rules.Load()
		if err != nil {
			return info, fmt.Errorf("kubeconfig %s: %w", info.Kubeconfig, err)
		}
		if info.Context == "" {
			info.Context = raw.CurrentContext
		}
		kc, ok := raw.Contexts[info.Context]
		if !ok {
			return info, fmt.Errorf("kubeconfig %s has no context %q; it has %s", info.Kubeconfig, info.Context, strings.Join(contextNames(raw.Contexts), ", "))
		}
		if cl, ok := raw.Clusters[kc.Cluster]; ok {
			info.Server = cl.Server
		}
	}
	c, err := clientsFor(kubeconfig, kubeContext)
	if err != nil {
		return info, err
	}
	if info.Server == "" && c.REST != nil {
		info.Server = c.REST.Host
	}
	nodes, err := c.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		info.NodesErr = err.Error()
		return info, nil
	}
	info.Nodes = len(nodes.Items)
	return info, nil
}

// contextNames lists the contexts of a kubeconfig, sorted.
func contextNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

// KubeconfigFile is a kubeconfig found in a repository.
type KubeconfigFile struct {
	Path     string            `json:"path"`
	Current  string            `json:"current_context,omitempty"`
	Contexts []string          `json:"contexts"`
	Servers  map[string]string `json:"servers,omitempty"` // context -> server
}

// kubeconfigSkip are the directories a kubeconfig is not looked for in.
var kubeconfigSkip = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, ".next": true, "dist": true, "build": true, "target": true, ".cache": true}

const (
	// kubeconfigMaxSize is the largest file read as a kubeconfig; one with
	// embedded certificates for a handful of clusters stays well below it.
	kubeconfigMaxSize = 256 << 10
	// kubeconfigMaxFiles bounds the walk.
	kubeconfigMaxFiles = 50000
)

// FindKubeconfigs looks for kubeconfig files under root. It looks into what
// git ignores too: the kubeconfig of a cluster is usually an output of the
// infrastructure code, kept out of git and next to it. Only the names of
// contexts and the addresses of servers are returned, never a credential.
func FindKubeconfigs(root string) []KubeconfigFile {
	var out []KubeconfigFile
	seen := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && kubeconfigSkip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if seen++; seen > kubeconfigMaxFiles {
			return filepath.SkipAll
		}
		if !mayBeKubeconfig(d.Name()) {
			return nil
		}
		if fi, err := d.Info(); err != nil || !fi.Mode().IsRegular() || fi.Size() > kubeconfigMaxSize {
			return nil
		}
		if k, ok := readKubeconfig(path); ok {
			out = append(out, k)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// mayBeKubeconfig reports whether a file name is one a kubeconfig goes by:
// YAML, or a name with "kubeconfig" in it, or the plain "config" of a .kube
// directory.
func mayBeKubeconfig(name string) bool {
	lower := strings.ToLower(name)
	switch filepath.Ext(lower) {
	case ".yaml", ".yml", ".conf", ".kubeconfig":
		return true
	}
	return strings.Contains(lower, "kubeconfig") || strings.Contains(lower, "kube_config") || lower == "config"
}

// readKubeconfig parses a file as a kubeconfig. ok is false for everything
// that is not one with at least one context.
func readKubeconfig(path string) (KubeconfigFile, bool) {
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "clusters:") || !strings.Contains(string(b), "contexts:") {
		return KubeconfigFile{}, false
	}
	raw, err := clientcmd.Load(b)
	if err != nil || len(raw.Contexts) == 0 {
		return KubeconfigFile{}, false
	}
	k := KubeconfigFile{Path: path, Current: raw.CurrentContext, Contexts: contextNames(raw.Contexts), Servers: map[string]string{}}
	for name, c := range raw.Contexts {
		if cl, ok := raw.Clusters[c.Cluster]; ok {
			k.Servers[name] = cl.Server
		}
	}
	return k, true
}

// ForwardRules is what the account needs in a namespace so wassup can open
// a tunnel to a Service there (via: k8s.service/...). The create on
// pods/portforward is the one permission of wassup that is not a read: it
// opens a stream and changes nothing in the cluster.
func ForwardRules() []probe.Rule {
	return []probe.Rule{
		{Group: "", Resources: []string{"services"}, Verbs: []string{"get"}},
		{Group: "", Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		{Group: "", Resources: []string{"pods/portforward"}, Verbs: []string{"create"}},
	}
}

// ViaNamespace returns the namespace a `via` of this package reaches into,
// or "" when via names no tunnel into a cluster.
func ViaNamespace(via string) string {
	scheme, target, ok := probe.SplitVia(via)
	if !ok || (scheme != schemeService && scheme != schemePod) {
		return ""
	}
	tt, err := parseTunnelTarget(target)
	if err != nil {
		return ""
	}
	return tt.namespace
}
