package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/danilopopovikj/wassup/internal/measure"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
	"github.com/danilopopovikj/wassup/internal/probe/signoz"
)

// defaultRouterPort is where Traefik's Helm chart serves its metrics.
const defaultRouterPort = "9100"

func measureCmd() *cobra.Command {
	var signozURL string
	var lookback time.Duration
	var write bool
	c := &cobra.Command{
		Use:   "measure",
		Short: "find a counter for every edge that has none, check it against the live data, and write the bindings that count",
		Long: `measure reads what the live sources hold and says, for every edge of the
diagram without a binding, one of three things: the binding that counts it,
with the evidence that it does (the router counts that service; SigNoz
counted 412 calls from the API to that host this week); why nothing can
count it yet, and what would change that; or that its kind carries no rate.
It also lists what the sources saw that no box on the diagram stands for:
a host the API calls that has no box, a service that sends traces and is no
component.

The sources: the cluster (which Service each Ingress routes to and which
pods it selects), one metrics page of the router (the services and routes
Traefik counts), and SigNoz over --lookback (the calls, queries and requests
of every service that sends traces). SigNoz is the one a signoz.edge binding
names, or the one --signoz names with SIGNOZ_USER and SIGNOZ_PASSWORD, or
SIGNOZ_API_KEY, from .wassup/local.env.

--write adds the bindings it found to bindings.yaml and touches no binding
that is there. Run wassup probe afterwards.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			src := gatherSources(ctx, cfg, signozURL, lookback)
			rep := measure.Plan(cfg, src)
			written := 0
			if write {
				if written, err = writeFound(cfg, rep); err != nil {
					return err
				}
			}
			if flags.jsonOut {
				return printJSON(map[string]any{"report": rep, "counts": rep.Counts(), "written": written})
			}
			printMeasure(rep, write, written)
			return nil
		},
	}
	c.Flags().StringVar(&signozURL, "signoz", "", "the address of SigNoz, when no signoz.edge binding names it")
	c.Flags().DurationVar(&lookback, "lookback", 7*24*time.Hour, "how far back SigNoz's counts are read")
	c.Flags().BoolVar(&write, "write", false, "add the bindings found to bindings.yaml")
	return c
}

// gatherSources reads the cluster, the router and SigNoz. A source that
// cannot be read is left out with a note: the others still decide.
func gatherSources(ctx context.Context, cfg *model.Config, signozURL string, lookback time.Duration) measure.Sources {
	var src measure.Sources
	note := func(format string, a ...any) { src.Notes = append(src.Notes, fmt.Sprintf(format, a...)) }

	clients, err := k8s.NewClients(flags.kubeconfig, flags.kcontext)
	if err == nil {
		if _, err = announceCluster(ctx, os.Stderr); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			src.Inventory, err = k8s.Discover(cctx, clients, nil)
			cancel()
		}
	}
	if err != nil {
		note("the cluster was not read: %v", err)
	}

	if spec := routerSpec(cfg, src.Inventory); spec != nil && clients != nil {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		src.Router, err = readRouter(cctx, clients, spec)
		cancel()
		if err != nil {
			note("the router's counters were not read (%s in %s, port %s): %v; Traefik serves them when metrics.prometheus is on in its Helm values",
				spec.String("selector"), spec.String("namespace"), spec.String("port"), err)
		}
	} else if src.Inventory != nil {
		note("no router was found: no k8s.scrape binding reads traefik_ counters and no Traefik workload runs")
	}

	src.SigNoz = signozSpec(cfg, signozURL)
	if src.SigNoz == nil {
		note("SigNoz was not read: no signoz.edge binding names it; pass --signoz")
		return src
	}
	fmt.Fprintf(os.Stderr, "signoz: %s, the last %s\n", src.SigNoz.String("url"), lookback)
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	sv, err := signoz.Surveyed(cctx, src.SigNoz, lookback)
	cancel()
	if err != nil {
		note("SigNoz was not read: %v", err)
	} else {
		src.Survey = &sv
	}
	return src
}

// routerSpec is where the router's counters are: the k8s.scrape binding
// that reads Traefik already, else the Traefik workload the cluster runs,
// on the port its chart serves them.
func routerSpec(cfg *model.Config, inv *k8s.Inventory) model.ProbeSpec {
	for _, group := range []map[string][]model.ProbeSpec{cfg.Bindings.Components, cfg.Bindings.Edges} {
		ids := make([]string, 0, len(group))
		for id := range group {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			for _, s := range group[id] {
				if s.Kind() == "k8s.scrape" && strings.HasPrefix(s.String("metric"), "traefik_") {
					out := model.ProbeSpec{}
					for _, k := range []string{"namespace", "selector", "port", "path", "scheme", "kubeconfig", "context"} {
						if v, ok := s[k]; ok {
							out[k] = v
						}
					}
					return out
				}
			}
		}
	}
	if inv == nil {
		return nil
	}
	for _, w := range inv.Workloads {
		for _, img := range w.Images {
			if strings.Contains(img, "traefik") && w.Selector != "" {
				return model.ProbeSpec{"namespace": w.Namespace, "selector": w.Selector, "port": defaultRouterPort}
			}
		}
	}
	return nil
}

// readRouter reads the labels of the router's counters from one of its pods.
func readRouter(ctx context.Context, c *k8s.Clients, spec model.ProbeSpec) (*measure.Router, error) {
	path := spec.String("path")
	if path == "" {
		path = "/metrics"
	}
	port := spec.String("port")
	if port == "" {
		port = defaultRouterPort
	}
	values := func(metric, label string) (map[string]bool, error) {
		sets, err := k8s.MetricLabels(ctx, c, spec.String("namespace"), spec.String("selector"), port, path, metric)
		if err != nil {
			return nil, err
		}
		out := map[string]bool{}
		for _, l := range sets {
			if v := l[label]; v != "" {
				out[v] = true
			}
		}
		return out, nil
	}
	services, err := values(measure.RouterServiceMetric, "service")
	if err != nil {
		return nil, err
	}
	// Without router labels the page holds no such series: no error, the
	// hosts that share a service say so.
	routers, _ := values(measure.RouterRouterMetric, "router")
	return &measure.Router{Spec: spec, Services: services, Routers: routers}, nil
}

// signozSpec is the url and credentials of SigNoz: those of the first
// signoz.edge binding, else --signoz with the variables local.env holds.
func signozSpec(cfg *model.Config, url string) model.ProbeSpec {
	for _, group := range []map[string][]model.ProbeSpec{cfg.Bindings.Edges, cfg.Bindings.Components} {
		ids := make([]string, 0, len(group))
		for id := range group {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			for _, s := range group[id] {
				if s.Kind() == "signoz.edge" && (url == "" || s.String("url") == url) {
					out := model.ProbeSpec{}
					for _, k := range []string{"url", "user_env", "password_env", "token_env"} {
						if v, ok := s[k]; ok {
							out[k] = v
						}
					}
					return out
				}
			}
		}
	}
	if url == "" {
		return nil
	}
	if os.Getenv("SIGNOZ_API_KEY") != "" {
		return model.ProbeSpec{"url": url, "token_env": "SIGNOZ_API_KEY"}
	}
	return model.ProbeSpec{"url": url, "user_env": "SIGNOZ_USER", "password_env": "SIGNOZ_PASSWORD"}
}

// writeFound adds the bindings found to bindings.yaml, beside what is there.
func writeFound(cfg *model.Config, rep measure.Report) (int, error) {
	n := 0
	for _, e := range rep.Edges {
		if e.Status != measure.Found || len(cfg.Bindings.Edges[e.ID]) > 0 {
			continue
		}
		if cfg.Bindings.Edges == nil {
			cfg.Bindings.Edges = map[string][]model.ProbeSpec{}
		}
		cfg.Bindings.Edges[e.ID] = []model.ProbeSpec{e.Binding}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	if cfg.Bindings.Version == 0 {
		cfg.Bindings.Version = 1
	}
	return n, model.WriteYAML(filepath.Join(cfg.Dir, "bindings.yaml"), cfg.Bindings)
}

// printMeasure writes the report for a person: what was found, what cannot
// be counted and why, and what no box stands for.
func printMeasure(rep measure.Report, write bool, written int) {
	counts := rep.Counts()
	fmt.Printf("\n%d edges: %d counted already, %d found, %d cannot be counted yet, %d need no rate\n",
		len(rep.Edges), counts[measure.Counted], counts[measure.Found], counts[measure.Missing], counts[measure.Skipped])
	section := func(title string, st measure.Status, show func(measure.Edge)) {
		var list []measure.Edge
		for _, e := range rep.Edges {
			if e.Status == st {
				list = append(list, e)
			}
		}
		if len(list) == 0 {
			return
		}
		fmt.Printf("\n%s\n", title)
		for _, e := range list {
			show(e)
		}
	}
	foundTitle := "found (--write adds them to bindings.yaml):"
	if write {
		foundTitle = fmt.Sprintf("found, %d written to bindings.yaml:", written)
	}
	section(foundTitle, measure.Found, func(e measure.Edge) {
		fmt.Printf("  %-32s %s\n", e.ID, e.Evidence)
	})
	section("cannot be counted yet:", measure.Missing, func(e measure.Edge) {
		fmt.Printf("  %-32s %s\n", e.ID, e.Reason)
		if e.Fix != "" {
			fmt.Printf("  %-32s fix: %s\n", "", e.Fix)
		}
	})
	if len(rep.Unplaced) > 0 {
		fmt.Println("\nseen, and no box stands for it:")
		for _, u := range rep.Unplaced {
			fmt.Printf("  %s called %s %.0f times, the last %s: %s\n", u.Service, u.Address, u.Count, u.Last.UTC().Format("2006-01-02 15:04 UTC"), u.Suggest)
		}
	}
	if len(rep.Unknown) > 0 {
		fmt.Printf("\nservices that send traces and are no component: %s\n", strings.Join(rep.Unknown, ", "))
	}
	for _, n := range rep.Notes {
		fmt.Printf("\nnote: %s\n", n)
	}
	if write && written > 0 {
		fmt.Println("\nrun wassup probe to read them")
	}
}
