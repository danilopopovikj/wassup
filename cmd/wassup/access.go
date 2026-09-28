package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

// accessPlan is what `wassup access` prints: the smallest account for the
// probes that are bound.
type accessPlan struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Duration  string `json:"token_duration"`
	Logs      bool   `json:"logs"`
	// Rules are the reads, granted in the whole cluster: the probes watch
	// through one shared cache per resource, which lists every namespace.
	Rules []probe.Rule `json:"rules"`
	// ForwardNamespaces are the namespaces a binding reaches into with
	// via: k8s.service/...; the port-forward is granted there and nowhere
	// else.
	ForwardNamespaces []string `json:"forward_namespaces,omitempty"`
	// ScrapeNamespaces are the namespaces a k8s.scrape binding reads the
	// metrics pages of pods in; a GET through the API server to a pod is
	// granted there and nowhere else.
	ScrapeNamespaces []string `json:"scrape_namespaces,omitempty"`
	// LeftOut says what the probes could use and the account does not get.
	LeftOut  []string `json:"left_out,omitempty"`
	Manifest string   `json:"manifest"`
	Commands string   `json:"commands"`
}

// buildAccess collects the rules of the probes bound in b.
func buildAccess(b model.Bindings, name, namespace, duration string, logs bool) accessPlan {
	p := accessPlan{Name: name, Namespace: namespace, Duration: duration, Logs: logs}
	verbs := map[string]map[string]bool{} // group/resource -> verbs
	add := func(r probe.Rule) {
		for _, res := range r.Resources {
			key := r.Group + "/" + res
			if verbs[key] == nil {
				verbs[key] = map[string]bool{}
			}
			for _, v := range r.Verbs {
				verbs[key][v] = true
			}
		}
	}
	forward := map[string]bool{}
	scrape := map[string]bool{}
	leftOut := map[string]bool{}
	each := func(specs []model.ProbeSpec) {
		for _, s := range specs {
			acc, _ := probe.AccessFor(s.Kind())
			for _, r := range acc.RBAC {
				add(r)
			}
			if ns := k8s.ViaNamespace(s.String("via")); ns != "" {
				forward[ns] = true
			}
			if ns := k8s.ScrapeNamespace(s.Kind(), s); ns != "" {
				scrape[ns] = true
			}
			switch s.Kind() {
			case "k8s.node", "k8s.pvc":
				leftOut["nodes/proxy: the disk usage of nodes and volumes is read through it, and it reaches every endpoint of the kubelet; without it disk usage reads as not known"] = true
			case "k8s.ingress":
				if on, _ := s["read_tls_secret"].(bool); on {
					leftOut["secrets: a binding sets read_tls_secret; the certificate is then read from cert-manager or from a handshake with the host instead"] = true
				}
			}
		}
	}
	for _, specs := range b.Components {
		each(specs)
	}
	for _, specs := range b.Edges {
		each(specs)
	}
	if logs {
		add(probe.Rule{Group: "", Resources: []string{"pods/log"}, Verbs: []string{"get"}})
		add(probe.Rule{Group: "", Resources: []string{"pods", "events"}, Verbs: []string{"get", "list"}})
	} else if len(verbs) > 0 {
		leftOut["pods/log: `wassup logs` and the log lines in `wassup explain` need it; add it with --logs"] = true
	}
	p.Rules = mergeRules(verbs)
	p.ForwardNamespaces = sortedSet(forward)
	p.ScrapeNamespaces = sortedSet(scrape)
	p.LeftOut = sortedSet(leftOut)
	p.Manifest = accessManifest(p)
	p.Commands = accessCommands(p)
	return p
}

// mergeRules groups resources that share an API group and the same verbs
// into one rule, in a stable order.
func mergeRules(verbs map[string]map[string]bool) []probe.Rule {
	byRule := map[string][]string{} // group\x00verbs -> resources
	for key, vs := range verbs {
		group, res, _ := strings.Cut(key, "/")
		k := group + "\x00" + strings.Join(sortedSet(vs), ",")
		byRule[k] = append(byRule[k], res)
	}
	keys := make([]string, 0, len(byRule))
	for k := range byRule {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]probe.Rule, 0, len(keys))
	for _, k := range keys {
		group, vs, _ := strings.Cut(k, "\x00")
		res := byRule[k]
		sort.Strings(res)
		out = append(out, probe.Rule{Group: group, Resources: res, Verbs: strings.Split(vs, ",")})
	}
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// yamlList renders a list of words as a YAML flow sequence of strings.
func yamlList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = `"` + s + `"`
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func yamlRules(b *strings.Builder, rules []probe.Rule) {
	b.WriteString("rules:\n")
	for _, r := range rules {
		fmt.Fprintf(b, "  - apiGroups: %s\n    resources: %s\n    verbs: %s\n", yamlList([]string{r.Group}), yamlList(r.Resources), yamlList(r.Verbs))
	}
}

// namespaceRole renders a Role in one namespace and its binding to the
// account.
func namespaceRole(b *strings.Builder, p accessPlan, suffix, ns string, rules []probe.Rule) {
	fmt.Fprintf(b, "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: %s-%s\n  namespace: %s\n", p.Name, suffix, ns)
	yamlRules(b, rules)
	fmt.Fprintf(b, "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata:\n  name: %s-%s\n  namespace: %s\n", p.Name, suffix, ns)
	fmt.Fprintf(b, "roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: Role\n  name: %s-%s\n", p.Name, suffix)
	fmt.Fprintf(b, "subjects:\n  - kind: ServiceAccount\n    name: %s\n    namespace: %s\n", p.Name, p.Namespace)
}

// accessManifest renders the account, the role of reads and its binding,
// and one role per namespace for the port-forward and for the metrics
// pages of pods.
func accessManifest(p accessPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: %s\n  namespace: %s\n", p.Name, p.Namespace)
	if len(p.Rules) > 0 {
		fmt.Fprintf(&b, "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: %s-read\n", p.Name)
		yamlRules(&b, p.Rules)
		fmt.Fprintf(&b, "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: %s-read\n", p.Name)
		fmt.Fprintf(&b, "roleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: %s-read\n", p.Name)
		fmt.Fprintf(&b, "subjects:\n  - kind: ServiceAccount\n    name: %s\n    namespace: %s\n", p.Name, p.Namespace)
	}
	for _, ns := range p.ForwardNamespaces {
		namespaceRole(&b, p, "forward", ns, k8s.ForwardRules())
	}
	for _, ns := range p.ScrapeNamespaces {
		namespaceRole(&b, p, "scrape", ns, k8s.ScrapeRules())
	}
	return b.String()
}

// accessCommands renders the commands that create the account and write a
// kubeconfig with a token that expires. They are printed, never run:
// wassup does not change a cluster, the person who owns it does.
func accessCommands(p accessPlan) string {
	kc := "$HOME/.kube/" + p.Name + "-readonly.yaml"
	again := "wassup access --manifest --name " + p.Name + " --namespace " + p.Namespace
	if p.Logs {
		again += " --logs"
	}
	lines := []string{
		"# Run these with the kubeconfig you have today. wassup runs none of them.",
		again + " > " + p.Name + "-access.yaml",
		"kubectl apply -f " + p.Name + "-access.yaml",
		"",
		"# A kubeconfig of its own, with a token that expires after " + p.Duration + ".",
		"KC=\"" + kc + "\"",
		"SERVER=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')",
		"kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > \"$KC.ca\"",
		"kubectl --kubeconfig \"$KC\" config set-cluster " + p.Name + " --server \"$SERVER\" --certificate-authority \"$KC.ca\" --embed-certs",
		"kubectl --kubeconfig \"$KC\" config set-credentials " + p.Name + " --token \"$(kubectl -n " + p.Namespace + " create token " + p.Name + " --duration " + p.Duration + ")\"",
		"kubectl --kubeconfig \"$KC\" config set-context " + p.Name + " --cluster " + p.Name + " --user " + p.Name,
		"kubectl --kubeconfig \"$KC\" config use-context " + p.Name,
		"rm \"$KC.ca\" && chmod 600 \"$KC\"",
		"",
		"# Tell wassup to use it. When the token has expired, run the set-credentials line again.",
		"echo \"WASSUP_KUBECONFIG=$KC\" >> .wassup/local.env",
	}
	return strings.Join(lines, "\n") + "\n"
}

func accessCmd() *cobra.Command {
	var name, namespace, duration string
	var logs, manifest bool
	c := &cobra.Command{
		Use:   "access",
		Short: "print the smallest read-only account for the probes in bindings.yaml, and how to get a kubeconfig for it",
		Long: `access reads bindings.yaml and prints what the probes bound there need in
the cluster, and nothing more: a ServiceAccount, a ClusterRole that can get,
list and watch the resources those probes read, and, for bindings that reach
a Service inside the cluster (via: k8s.service/...), a Role for the
port-forward in that namespace only, and for bindings that read the metrics
pages of pods (k8s.scrape), a Role for a GET to the pods of that namespace
only. Secrets, nodes/proxy and exec are never part of it.

It then prints the commands that create the account and write a kubeconfig
with a token that expires. wassup runs none of them and contacts nothing.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			p := buildAccess(cfg.Bindings, name, namespace, duration, logs)
			switch {
			case flags.jsonOut:
				return printJSON(p)
			case manifest:
				fmt.Print(p.Manifest)
				return nil
			}
			if len(p.Rules) == 0 && len(p.ForwardNamespaces) == 0 && len(p.ScrapeNamespaces) == 0 {
				fmt.Println("no binding reads the cluster: no account is needed")
				return nil
			}
			fmt.Printf("# %s-access.yaml: what the bound probes read, and nothing else\n%s\n", p.Name, p.Manifest)
			if len(p.LeftOut) > 0 {
				fmt.Println("# Left out on purpose:")
				for _, l := range p.LeftOut {
					fmt.Println("#   " + l)
				}
				fmt.Println()
			}
			fmt.Print(p.Commands)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "wassup", "name of the account and its roles")
	c.Flags().StringVar(&namespace, "namespace", "default", "namespace of the ServiceAccount")
	c.Flags().StringVar(&duration, "duration", "8h", "how long the token is valid")
	c.Flags().BoolVar(&logs, "logs", false, "also let the account read pod logs, for `wassup logs` and `wassup explain`")
	c.Flags().BoolVar(&manifest, "manifest", false, "print the manifest only")
	return c
}
