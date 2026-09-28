// Package k8s implements the Kubernetes probes of wassup: workloads, nodes,
// cron jobs, persistent volume claims, ingresses and CloudNativePG
// clusters and instances. It also exports the client, log, event and
// discovery helpers the CLI uses.
//
// Every probe is read only, and so is every client: the transport they
// share refuses a request that would change the cluster before it is sent
// (tuneConfig). Probes bound to the same kubeconfig and context
// share one set of clients and one shared informer factory (see Clients),
// read the informer stores on every tick and additionally when the informer
// reports a change to the bound object.
//
// The package also opens the tunnels of the `via` schemes k8s.service and
// k8s.pod: a port-forward from a free port on 127.0.0.1 to a pod, for probes
// of other packages whose server is only reachable from inside the cluster.
// A tunnel lives from the opener's return until its Close.
//
// The Kubernetes client's own log is dropped when the package is loaded, so
// nothing but wassup writes to the terminal; set WASSUP_DEBUG to keep it on
// stderr.
package k8s
