package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
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

// ViaName returns the name a server behind a tunnel goes by where it runs,
// for a binding that names no host of its own: the Service as the cluster
// resolves it ("cache.shop.svc"), or the pod. It is what the messages, the
// Host header and the TLS handshake say; the address to dial is the
// tunnel's. port is the port of via when it is a number, "" when it is the
// name of a port or via names no tunnel.
func ViaName(via string) (host, port string) {
	scheme, target, ok := SplitVia(via)
	if !ok {
		return "", ""
	}
	ns, rest, _ := strings.Cut(target, "/")
	name, port, _ := strings.Cut(rest, ":")
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		port = ""
	}
	if strings.HasSuffix(scheme, ".service") {
		name += "." + ns + ".svc"
	}
	return name, port
}

// dialTimeout bounds the connection to the local end of a tunnel.
const dialTimeout = 5 * time.Second

// ErrNoTunnel is in the error of a connection whose tunnel could not be
// opened. The server was not reached, so what the error says is about the
// way to it: a probe that words the errors of its server for a person
// leaves this one as it is.
var ErrNoTunnel = errors.New("the tunnel could not be opened")

// Via is the way of one binding through the tunnel its `via` names. The
// tunnel is opened with the first connection, not before, and the bindings
// of one process that name the same tunnel share it: five probes of one
// Service are one port-forward, not five. Drop after a connection that
// failed, Close when the probe stops, both after the connections through
// the tunnel were closed. A nil Via is a binding without a tunnel: Drop and
// Close do nothing.
type Via struct {
	via  string
	spec map[string]any

	mu     sync.Mutex
	held   *sharedTunnel
	closed bool
}

// NewVia returns the way through the tunnel spec["via"] names, or nil when
// it names none: no via, or a label.
func NewVia(spec map[string]any) *Via {
	via := Str(spec, "via", "")
	if _, _, ok := SplitVia(via); !ok {
		return nil
	}
	return &Via{via: via, spec: spec}
}

// String returns the `via` of the binding.
func (v *Via) String() string {
	if v == nil {
		return ""
	}
	return v.via
}

// URL returns the URL of an HTTP server behind the tunnel for a binding
// that gives none: plain HTTP, which is what a port inside a cluster
// speaks in nearly every case, to the name of ViaName.
func (v *Via) URL() string {
	host, port := ViaName(v.via)
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return "http://" + host
}

// URLOf returns the URL of an HTTP server a spec names under key, without
// a slash at its end. Through a tunnel the address is the tunnel's, so the
// URL may be left out: the server is then read over plain HTTP under the
// name it has in the cluster. A URL that is set is absolute, with or
// without a tunnel. v is nil for a binding without a tunnel.
func (v *Via) URLOf(spec map[string]any, key string) (string, error) {
	if raw, set := spec[key]; v != nil && (!set || raw == nil || raw == "") {
		return v.URL(), nil
	}
	if err := RequireString(spec, key); err != nil {
		if v != nil {
			return "", fmt.Errorf("%s must be a string, got %T", key, spec[key])
		}
		return "", err
	}
	raw := Str(spec, key, "")
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%s %q must be an absolute http(s) URL", key, raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// Through returns tr with its connections going through the tunnel. The
// URL of a request still names the server: its host is the Host header and
// the name a certificate is checked against, and is never dialled. A proxy
// of the environment is left out, it does not lead into the tunnel.
func (v *Via) Through(tr *http.Transport) *http.Transport {
	tr.Proxy = nil
	tr.DialContext = v.Dial
	return tr
}

// Dial connects to the local end of the tunnel, whatever address is asked
// for, and opens the tunnel when the binding holds none. ctx bounds the
// opening and the connection, not the life of the tunnel.
func (v *Via) Dial(ctx context.Context, network, _ string) (net.Conn, error) {
	addr, err := v.addr(ctx)
	if err != nil {
		// Worded here: the server was not reached yet, and what the
		// cluster said must not be read as its answer.
		return nil, fmt.Errorf("via %s: %w, so the server was not reached: %w", v.via, ErrNoTunnel, err)
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		v.Drop()
		return nil, fmt.Errorf("via %s: %w", v.via, err)
	}
	return conn, nil
}

// addr returns the local address of the tunnel the binding holds, taking
// hold of one first when it has none.
func (v *Via) addr(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return "", errors.New("the probe has stopped")
	}
	if v.held == nil {
		s, err := hold(ctx, v.via, v.spec)
		if err != nil {
			return "", err
		}
		v.held = s
	}
	return v.held.t.Addr, nil
}

// Drop lets go of the tunnel after a connection through it failed. It may
// be the tunnel that broke, so the next connection opens a new one; the
// bindings that still hold this one keep it until they fail too.
func (v *Via) Drop() {
	if v == nil {
		return
	}
	v.mu.Lock()
	s := v.held
	v.held = nil
	v.mu.Unlock()
	if s != nil {
		s.release(true)
	}
}

// Close lets go of the tunnel when the probe stops; the tunnel is taken
// down with the last binding that held it. Nothing is opened after it.
func (v *Via) Close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	s := v.held
	v.held, v.closed = nil, true
	v.mu.Unlock()
	if s != nil {
		s.release(false)
	}
}

// sharedTunnel is one open tunnel and the number of bindings that hold it.
type sharedTunnel struct {
	key string
	// ready is closed once the opener has answered: t or err is then set.
	ready chan struct{}
	t     *Tunnel
	err   error
	holds int
}

var (
	sharedMu      sync.Mutex
	sharedTunnels = map[string]*sharedTunnel{}
)

// hold returns the tunnel via names in the cluster the spec names, opening
// it when no binding holds one. Bindings that ask while it is being opened
// wait for the same one.
func hold(ctx context.Context, via string, spec map[string]any) (*sharedTunnel, error) {
	key := strings.Join([]string{via, Str(spec, "kubeconfig", ""), Str(spec, "context", "")}, "\x00")
	sharedMu.Lock()
	s, open := sharedTunnels[key]
	if !open {
		s = &sharedTunnel{key: key, ready: make(chan struct{})}
		sharedTunnels[key] = s
	}
	s.holds++
	sharedMu.Unlock()
	if !open {
		s.t, s.err = OpenTunnel(ctx, via, spec)
		if s.err == nil && s.t == nil {
			s.err = fmt.Errorf("via %q: the opener returned no tunnel", via)
		}
		close(s.ready)
	}
	select {
	case <-s.ready:
	case <-ctx.Done():
		s.release(false)
		return nil, ctx.Err()
	}
	if s.err != nil {
		s.release(true)
		return nil, s.err
	}
	return s, nil
}

// release gives one hold back and takes the tunnel down with the last one.
// broken takes it off the list first, so that whoever asks next opens a new
// tunnel instead of joining this one.
func (s *sharedTunnel) release(broken bool) {
	sharedMu.Lock()
	s.holds--
	last := s.holds == 0
	if (broken || last) && sharedTunnels[s.key] == s {
		delete(sharedTunnels, s.key)
	}
	sharedMu.Unlock()
	if last && s.t != nil && s.t.Close != nil {
		s.t.Close()
	}
}
