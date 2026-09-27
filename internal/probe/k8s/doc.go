// Package k8s implements the Kubernetes probes of wassup: workloads, nodes,
// cron jobs, persistent volume claims, ingresses and CloudNativePG
// clusters and instances. It also exports the client, log, event and
// discovery helpers the CLI uses.
//
// Every probe is read only. Probes bound to the same kubeconfig and context
// share one set of clients and one shared informer factory (see Clients),
// read the informer stores on every tick and additionally when the informer
// reports a change to the bound object.
package k8s
