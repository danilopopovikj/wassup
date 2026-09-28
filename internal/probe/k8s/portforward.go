package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// The `via` schemes this package opens.
const (
	schemeService = "k8s.service"
	schemePod     = "k8s.pod"
)

const (
	// tunnelHost is where a tunnel listens: the loopback address, by number,
	// so the port is never offered to the network and never depends on how
	// the machine resolves "localhost".
	tunnelHost = "127.0.0.1"
	// tunnelCloseWait bounds the wait for a forwarder to end on Close. It
	// ends within milliseconds; the bound is for the API server that no
	// longer answers, which must not hold up leaving wassup.
	tunnelCloseWait = 2 * time.Second
)

func init() {
	t := &tunneler{}
	probe.RegisterTunnel(schemeService, t.openService)
	probe.RegisterTunnel(schemePod, t.openPod)
}

// tunneler opens port-forwards into a cluster for probes that speak to a
// port only reachable from inside it. The fields are the two places a test
// replaces: the cluster and the forwarding itself.
type tunneler struct {
	// clients returns the clients for a binding's spec; nil shares the ones
	// the probes use (same kubeconfig and context resolution).
	clients func(spec map[string]any) (*Clients, error)
	// forward opens the tunnel to a resolved pod and port; nil is the real
	// port-forward through the API server.
	forward func(ctx context.Context, c *Clients, to forwardTarget) (*probe.Tunnel, error)
}

// tunnelTarget is a parsed `<namespace>/<name>:<port>`. The port is a number
// or the name of a port.
type tunnelTarget struct {
	namespace string
	name      string
	port      string
}

// forwardTarget is where a tunnel ends: one port of one pod.
type forwardTarget struct {
	namespace string
	pod       string
	port      int
}

// String names the target in messages.
func (f forwardTarget) String() string {
	return fmt.Sprintf("pod %s/%s port %d", f.namespace, f.pod, f.port)
}

// parseTunnelTarget splits `<namespace>/<name>:<port>`.
func parseTunnelTarget(target string) (tunnelTarget, error) {
	bad := func(why string) (tunnelTarget, error) {
		return tunnelTarget{}, fmt.Errorf("tunnel target %q must be <namespace>/<name>:<port>: %s", target, why)
	}
	ns, rest, ok := strings.Cut(target, "/")
	if !ok || ns == "" {
		return bad("the namespace is missing")
	}
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return bad("the port is missing")
	}
	name, port := rest[:i], rest[i+1:]
	switch {
	case name == "":
		return bad("the name is missing")
	case strings.ContainsAny(ns+name, "/: \t"):
		return bad("the namespace and the name are single words")
	case port == "":
		return bad("the port is missing")
	case strings.ContainsAny(port, "/ \t"):
		return bad("the port is a number or the name of a port")
	}
	if isNumber(port) {
		if _, err := portNumber(port); err != nil {
			return bad(err.Error())
		}
	}
	return tunnelTarget{namespace: ns, name: name, port: port}, nil
}

// isNumber reports whether s is made of digits only.
func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// portNumber parses a port number and checks its range.
func portNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %s is not between 1 and 65535", s)
	}
	return n, nil
}

// resolveServiceForward picks the pod and the pod's port behind a port of a
// Service: the Service port by number or by name, its targetPort (a number,
// or a named container port looked up on the pod), and the first pod by name
// among those that match the selector, are ready and are not being deleted.
// The first by name keeps the choice stable from one tunnel to the next.
func resolveServiceForward(svc *corev1.Service, pods []corev1.Pod, port string) (forwardTarget, error) {
	ref := svc.Namespace + "/" + svc.Name
	if len(svc.Spec.Selector) == 0 {
		kind := ""
		if svc.Spec.Type == corev1.ServiceTypeExternalName {
			kind = " (it points outside the cluster)"
		}
		return forwardTarget{}, fmt.Errorf("service %s has no selector%s, so there is no pod to forward to", ref, kind)
	}
	sp, err := servicePort(svc, port)
	if err != nil {
		return forwardTarget{}, err
	}
	if sp.Protocol != "" && sp.Protocol != corev1.ProtocolTCP {
		return forwardTarget{}, fmt.Errorf("port %s of service %s is %s, only TCP can be forwarded", port, ref, sp.Protocol)
	}

	selector := labels.SelectorFromSet(svc.Spec.Selector)
	var matching, ready []*corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Namespace != svc.Namespace || !selector.Matches(labels.Set(p.Labels)) {
			continue
		}
		matching = append(matching, p)
		if p.DeletionTimestamp == nil && podReady(p) {
			ready = append(ready, p)
		}
	}
	if len(ready) == 0 {
		return forwardTarget{}, fmt.Errorf("service %s has no ready pod to forward to (%d match its selector)", ref, len(matching))
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })
	for _, p := range ready {
		if n, ok := targetPortOn(p, sp); ok {
			return forwardTarget{namespace: p.Namespace, pod: p.Name, port: n}, nil
		}
	}
	return forwardTarget{}, fmt.Errorf("no ready pod of service %s has a container port named %q, the target of its port %s", ref, sp.TargetPort.StrVal, port)
}

// servicePort finds a Service port by number or by name.
func servicePort(svc *corev1.Service, port string) (corev1.ServicePort, error) {
	var known []string
	for _, sp := range svc.Spec.Ports {
		if isNumber(port) && strconv.Itoa(int(sp.Port)) == port {
			return sp, nil
		}
		if !isNumber(port) && sp.Name == port {
			return sp, nil
		}
		k := strconv.Itoa(int(sp.Port))
		if sp.Name != "" {
			k += " (" + sp.Name + ")"
		}
		known = append(known, k)
	}
	if len(known) == 0 {
		return corev1.ServicePort{}, fmt.Errorf("service %s/%s has no ports", svc.Namespace, svc.Name)
	}
	return corev1.ServicePort{}, fmt.Errorf("service %s/%s has no port %s; it has %s", svc.Namespace, svc.Name, port, strings.Join(known, ", "))
}

// targetPortOn returns the pod port a Service port leads to. ok is false
// when the target is a named port this pod does not declare.
func targetPortOn(p *corev1.Pod, sp corev1.ServicePort) (int, bool) {
	switch {
	case sp.TargetPort.Type == intstr.String && sp.TargetPort.StrVal != "":
		return containerPort(p, sp.TargetPort.StrVal)
	case sp.TargetPort.IntValue() > 0:
		return sp.TargetPort.IntValue(), true
	}
	// An unset targetPort means the same number as the Service port.
	return int(sp.Port), true
}

// containerPort finds a TCP container port by name.
func containerPort(p *corev1.Pod, name string) (int, bool) {
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.Name == name && (cp.Protocol == "" || cp.Protocol == corev1.ProtocolTCP) {
				return int(cp.ContainerPort), true
			}
		}
	}
	return 0, false
}

// resolvePodPort turns the port of a pod target into a number: the number
// itself, or the container port of that name.
func resolvePodPort(p *corev1.Pod, port string) (int, error) {
	if isNumber(port) {
		return portNumber(port)
	}
	if p == nil {
		return 0, fmt.Errorf("port %q is a name and needs the pod to look it up", port)
	}
	if n, ok := containerPort(p, port); ok {
		return n, nil
	}
	return 0, fmt.Errorf("pod %s/%s has no container port named %q", p.Namespace, p.Name, port)
}

// connect returns the clients for the spec's kubeconfig and context, the
// ones the probes of the same cluster already share.
func (t *tunneler) connect(spec map[string]any) (*Clients, error) {
	var c *Clients
	var err error
	if t.clients != nil {
		c, err = t.clients(spec)
	} else {
		c, err = clientsFor(probe.Str(spec, "kubeconfig", ""), probe.Str(spec, "context", ""))
	}
	if err != nil {
		return nil, err
	}
	if c == nil || c.Core == nil {
		return nil, errors.New("no Kubernetes client to open the tunnel with")
	}
	return c, nil
}

// openService opens a tunnel to a port of a Service, by way of one of its
// ready pods: the API server forwards to pods, not to Services.
func (t *tunneler) openService(ctx context.Context, target string, spec map[string]any) (*probe.Tunnel, error) {
	tt, err := parseTunnelTarget(target)
	if err != nil {
		return nil, err
	}
	c, err := t.connect(spec)
	if err != nil {
		return nil, err
	}
	svc, err := c.Core.CoreV1().Services(tt.namespace).Get(ctx, tt.name, metav1.GetOptions{})
	if err != nil {
		return nil, readError("service", tt.namespace, tt.name, "get on services", err)
	}
	var pods []corev1.Pod
	if len(svc.Spec.Selector) > 0 {
		list, err := c.Core.CoreV1().Pods(tt.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String(),
		})
		if err != nil {
			return nil, readError("pods of service", tt.namespace, tt.name, "list on pods", err)
		}
		pods = list.Items
	}
	to, err := resolveServiceForward(svc, pods, tt.port)
	if err != nil {
		return nil, err
	}
	return t.forwardTo(ctx, c, to)
}

// openPod opens a tunnel to a port of a pod. The pod is read only when the
// port is given by name; a number needs no more access than the forward.
func (t *tunneler) openPod(ctx context.Context, target string, spec map[string]any) (*probe.Tunnel, error) {
	tt, err := parseTunnelTarget(target)
	if err != nil {
		return nil, err
	}
	c, err := t.connect(spec)
	if err != nil {
		return nil, err
	}
	var pod *corev1.Pod
	if !isNumber(tt.port) {
		pod, err = c.Core.CoreV1().Pods(tt.namespace).Get(ctx, tt.name, metav1.GetOptions{})
		if err != nil {
			return nil, readError("pod", tt.namespace, tt.name, "get on pods", err)
		}
	}
	n, err := resolvePodPort(pod, tt.port)
	if err != nil {
		return nil, err
	}
	return t.forwardTo(ctx, c, forwardTarget{namespace: tt.namespace, pod: tt.name, port: n})
}

// forwardTo opens the tunnel through the injected forward or the real one.
func (t *tunneler) forwardTo(ctx context.Context, c *Clients, to forwardTarget) (*probe.Tunnel, error) {
	if t.forward != nil {
		return t.forward(ctx, c, to)
	}
	return spdyForward(ctx, c, to)
}

// readError words a failed read for a person: what could not be read and,
// when access is the reason, the permission that is missing.
func readError(what, ns, name, permission string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s %s/%s not found", what, ns, name)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("not allowed to read %s %s/%s: the account needs %s in namespace %s (%w)", what, ns, name, permission, ns, err)
	}
	return fmt.Errorf("read %s %s/%s: %w", what, ns, name, err)
}

// forwardError words a failed port-forward for a person.
func forwardError(to forwardTarget, err error) error {
	msg := strings.ToLower(err.Error())
	switch {
	case apierrors.IsForbidden(err) || strings.Contains(msg, "forbidden"):
		return fmt.Errorf("not allowed to port-forward to %s: the account needs create on pods/portforward in namespace %s (%w)", to, to.namespace, err)
	case apierrors.IsNotFound(err) || strings.Contains(msg, "not found"):
		return fmt.Errorf("port-forward to %s: the pod was not found (%w)", to, err)
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return fmt.Errorf("port-forward to %s was not ready in time (%w)", to, err)
	}
	return fmt.Errorf("port-forward to %s: %w", to, err)
}

// forwarder is what a tunnel drives of the library's port forwarder.
type forwarder interface {
	// ForwardPorts runs the forward until its context ends or the
	// connection to the pod is lost.
	ForwardPorts() error
	// GetPorts returns the ports once the forwarder listens, with the local
	// port the system picked.
	GetPorts() ([]portforward.ForwardedPort, error)
}

// spdyForward forwards a free local port to a pod's port through the API
// server's portforward subresource.
func spdyForward(ctx context.Context, c *Clients, to forwardTarget) (*probe.Tunnel, error) {
	if c.REST == nil {
		return nil, fmt.Errorf("port-forward to %s: no API server configuration", to)
	}
	rc, ok := c.Core.CoreV1().RESTClient().(*rest.RESTClient)
	if !ok || rc == nil {
		return nil, fmt.Errorf("port-forward to %s: no REST client for the API server", to)
	}
	transport, upgrader, err := spdy.RoundTripperFor(c.REST)
	if err != nil {
		return nil, fmt.Errorf("port-forward to %s: %w", to, err)
	}
	url := rc.Post().Resource("pods").Namespace(to.namespace).Name(to.pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)

	t, err := openTunnel(ctx, tunnelCloseWait, func(live context.Context, ready chan struct{}) (forwarder, error) {
		// Local port 0 lets the system pick a free one. The forwarder prints
		// what it does; wassup owns the terminal, so that goes nowhere.
		return portforward.NewOnAddressesWithContext(live, dialer, []string{tunnelHost},
			[]string{fmt.Sprintf("0:%d", to.port)}, ready, io.Discard, io.Discard)
	})
	if err != nil {
		return nil, forwardError(to, err)
	}
	return t, nil
}

// openTunnel runs a forwarder and returns once it listens. ctx bounds the
// opening only: the caller's context is a short one per round, and a probe
// says goodbye to its server through the tunnel after its own context has
// ended, so the tunnel lives until Close. A forwarder that dies on its own
// (pod deleted, connection lost) is not revived here; connections to Addr
// then fail and the caller closes this tunnel and opens another.
func openTunnel(ctx context.Context, closeWait time.Duration, build func(live context.Context, ready chan struct{}) (forwarder, error)) (*probe.Tunnel, error) {
	live, stop := context.WithCancel(context.WithoutCancel(ctx))
	ready := make(chan struct{})
	fw, err := build(live, ready)
	if err != nil {
		stop()
		return nil, err
	}
	ended := make(chan error, 1)
	go func() { ended <- fw.ForwardPorts() }()

	select {
	case <-ready:
	case err := <-ended:
		stop()
		if err == nil {
			err = errors.New("the forwarder ended before it listened")
		}
		return nil, err
	case <-ctx.Done():
		// The forwarder may still be dialling; it sees the stop as soon as
		// it gets through and ends by itself.
		stop()
		return nil, ctx.Err()
	}

	done := make(chan struct{})
	go func() {
		<-ended
		close(done)
	}()
	var once sync.Once
	closeTunnel := func() {
		once.Do(func() {
			stop()
			select {
			case <-done:
			case <-time.After(closeWait):
			}
		})
	}
	ports, err := fw.GetPorts()
	if err == nil && (len(ports) == 0 || ports[0].Local == 0) {
		err = errors.New("the forwarder reports no local port")
	}
	if err != nil {
		closeTunnel()
		return nil, err
	}
	return &probe.Tunnel{
		Addr:  net.JoinHostPort(tunnelHost, strconv.Itoa(int(ports[0].Local))),
		Close: closeTunnel,
	}, nil
}
