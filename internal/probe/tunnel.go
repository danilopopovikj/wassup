package probe

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Tunnel is an open path to a port the machine running wassup cannot reach
// directly, such as a port-forward to a Service in the cluster. Addr is the
// local host:port to connect to instead; Close takes the path down.
type Tunnel struct {
	Addr  string
	Close func()
}

// TunnelOpener opens a tunnel to target, the part of `via` after the scheme
// ("<namespace>/<name>:<port>"). spec is the binding's spec, for provider
// settings such as kubeconfig and context. A provider registers one opener
// per scheme, so a probe that connects through a tunnel never imports the
// provider.
type TunnelOpener func(ctx context.Context, target string, spec map[string]any) (*Tunnel, error)

var (
	tunnelMu sync.RWMutex
	tunnels  = map[string]TunnelOpener{}
)

// RegisterTunnel adds an opener for a `via` scheme ("k8s.service").
func RegisterTunnel(scheme string, o TunnelOpener) {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	tunnels[scheme] = o
}

// SplitVia splits "k8s.service/shop/db-rw:5432" into its scheme and target.
// ok is false when via names no registered scheme: it is then a label for
// the detail panel and nothing is opened.
func SplitVia(via string) (scheme, target string, ok bool) {
	scheme, target, found := strings.Cut(via, "/")
	if !found || target == "" {
		return "", "", false
	}
	tunnelMu.RLock()
	defer tunnelMu.RUnlock()
	_, ok = tunnels[scheme]
	return scheme, target, ok
}

// OpenTunnel opens the tunnel `via` names.
func OpenTunnel(ctx context.Context, via string, spec map[string]any) (*Tunnel, error) {
	scheme, target, ok := SplitVia(via)
	if !ok {
		return nil, fmt.Errorf("via %q names no tunnel this build can open", via)
	}
	tunnelMu.RLock()
	o := tunnels[scheme]
	tunnelMu.RUnlock()
	return o(ctx, target, spec)
}

// tunnelTarget is "<namespace>/<name>:<port>", the port a number or a name.
var tunnelTarget = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*/[a-z0-9][a-z0-9.-]*:[A-Za-z0-9-]+$`)

// ValidateVia checks the shape of a `via` that names a tunnel, offline. A
// via that names no tunnel is a label and always valid.
func ValidateVia(via string) error {
	scheme, target, ok := SplitVia(via)
	if !ok {
		return nil
	}
	if !tunnelTarget.MatchString(target) {
		return fmt.Errorf("via: %s needs <namespace>/<name>:<port>, got %q", scheme, target)
	}
	return nil
}
