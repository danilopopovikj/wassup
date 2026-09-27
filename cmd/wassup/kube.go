package main

import (
	"context"
	"fmt"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

// The Kubernetes-backed helpers behind discover, logs, events and explain.
func init() {
	kubeLogs = func(ctx context.Context, comp model.Component, spec model.ProbeSpec, since time.Duration, tail int) ([]string, error) {
		c, err := k8s.NewClients(kubeconfigFor(spec), contextFor(spec))
		if err != nil {
			return nil, err
		}
		ns := spec.String("namespace")
		selector := spec.String("selector")
		if selector == "" {
			selector = selectorFor(spec, comp)
		}
		if ns == "" || selector == "" {
			return nil, fmt.Errorf("binding needs namespace and selector (or name) to read logs")
		}
		return k8s.PodLogs(ctx, c, ns, selector, since, int64(tail))
	}
	kubeEvents = func(ctx context.Context, comp model.Component, spec model.ProbeSpec, since time.Duration) ([]string, error) {
		c, err := k8s.NewClients(kubeconfigFor(spec), contextFor(spec))
		if err != nil {
			return nil, err
		}
		ns := spec.String("namespace")
		name := spec.String("name")
		if name == "" {
			name = spec.String("selector")
		}
		if name == "" {
			name = comp.ID
		}
		return k8s.Events(ctx, c, ns, name, since)
	}
	kubeDiscover = func(ctx context.Context, kubeconfig, kcontext string, namespaces []string) (any, error) {
		c, err := k8s.NewClients(kubeconfig, kcontext)
		if err != nil {
			return nil, err
		}
		return k8s.Discover(ctx, c, namespaces)
	}
}

func kubeconfigFor(spec model.ProbeSpec) string {
	if v := spec.String("kubeconfig"); v != "" {
		return v
	}
	return flags.kubeconfig
}

func contextFor(spec model.ProbeSpec) string {
	if v := spec.String("context"); v != "" {
		return v
	}
	return flags.kcontext
}

// selectorFor derives a label selector from a name-based binding.
func selectorFor(spec model.ProbeSpec, comp model.Component) string {
	if name := spec.String("name"); name != "" {
		return "app=" + name
	}
	return "app=" + comp.ID
}
