package probe

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTunnels is an opener for a test scheme: every tunnel it opens ends at
// a listener of its own on this machine, and it counts what it opened and
// what was closed.
type fakeTunnels struct {
	t *testing.T
	// wait, when set, holds the opener until it is closed.
	wait chan struct{}
	// dead makes the tunnels end where nothing listens.
	dead atomic.Bool

	mu     sync.Mutex
	opened []string // the local address of each tunnel, in order
	closed []string
	specs  []map[string]any
}

// register adds the opener under scheme.
func register(t *testing.T, scheme string) *fakeTunnels {
	t.Helper()
	f := &fakeTunnels{t: t}
	RegisterTunnel(scheme, f.open)
	return f
}

func (f *fakeTunnels) open(ctx context.Context, target string, spec map[string]any) (*Tunnel, error) {
	if f.wait != nil {
		select {
		case <-f.wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := ln.Addr().String()
	if f.dead.Load() {
		ln.Close()
	} else {
		f.t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, addr)
	f.specs = append(f.specs, spec)
	return &Tunnel{Addr: addr, Close: func() {
		ln.Close()
		f.mu.Lock()
		f.closed = append(f.closed, addr)
		f.mu.Unlock()
	}}, nil
}

// counts returns how many tunnels were opened and how many closed.
func (f *fakeTunnels) counts() (opened, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opened), len(f.closed)
}

// dial connects through v and returns the address that was reached.
func dial(t *testing.T, v *Via) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := v.Dial(ctx, "tcp", "cache.shop.svc:6379")
	if err != nil {
		t.Fatalf("dial through %s: %v", v, err)
	}
	defer conn.Close()
	return conn.RemoteAddr().String()
}

func TestNewViaIsNilWithoutATunnel(t *testing.T) {
	register(t, "test.none")
	for _, via := range []string{"", "bastion", "k8s.workload/api", "unknown.scheme/shop/cache:6379"} {
		if v := NewVia(map[string]any{"via": via}); v != nil {
			t.Errorf("via %q names no tunnel of this test and got %v", via, v)
		}
	}
	var v *Via
	v.Drop()
	v.Close()
	if v.String() != "" {
		t.Errorf("a binding without a tunnel has no via, got %q", v.String())
	}
	if v := NewVia(map[string]any{"via": "test.none/shop/cache:6379"}); v == nil || v.String() != "test.none/shop/cache:6379" {
		t.Errorf("via = %v", v)
	}
}

// Nothing is opened before the first connection, the connection goes to the
// tunnel whatever address was asked for, and the bindings that name the
// same tunnel hold one between them, which goes with the last.
func TestBindingsShareOneTunnel(t *testing.T) {
	f := register(t, "test.share")
	spec := map[string]any{"via": "test.share/shop/cache:6379"}
	a, b := NewVia(spec), NewVia(map[string]any{"via": "test.share/shop/cache:6379", "key": "orders"})
	if opened, _ := f.counts(); opened != 0 {
		t.Fatalf("%d tunnels were opened before a connection was asked for", opened)
	}
	first, second := dial(t, a), dial(t, b)
	if opened, _ := f.counts(); opened != 1 {
		t.Fatalf("two bindings to the same place opened %d tunnels, want 1", opened)
	}
	if first != f.opened[0] || second != f.opened[0] {
		t.Errorf("the connections went to %s and %s, the tunnel is at %s", first, second, f.opened[0])
	}
	if again := dial(t, a); again != first {
		t.Errorf("the second connection of a binding went to %s, want the tunnel it holds at %s", again, first)
	}

	a.Close()
	if _, closed := f.counts(); closed != 0 {
		t.Fatal("the tunnel was closed while a binding still holds it")
	}
	a.Close()
	b.Close()
	if _, closed := f.counts(); closed != 1 {
		t.Fatalf("the tunnel was closed %d times, want once, with the last binding", closed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := a.Dial(ctx, "tcp", ""); err == nil {
		t.Error("a binding that was closed opened a connection")
	}
	if opened, _ := f.counts(); opened != 1 {
		t.Errorf("a binding that was closed opened a tunnel: %d", opened)
	}
}

// Another cluster is another tunnel, although via reads the same.
func TestTheClusterIsPartOfTheTunnel(t *testing.T) {
	f := register(t, "test.cluster")
	a := NewVia(map[string]any{"via": "test.cluster/shop/cache:6379", "context": "bookstore-prod"})
	b := NewVia(map[string]any{"via": "test.cluster/shop/cache:6379", "context": "bookstore-staging"})
	defer a.Close()
	defer b.Close()
	if dial(t, a) == dial(t, b) {
		t.Error("two clusters were reached through one tunnel")
	}
	if opened, _ := f.counts(); opened != 2 {
		t.Errorf("opened %d tunnels, want one per cluster", opened)
	}
	if got := f.specs[0]["context"]; got != "bookstore-prod" {
		t.Errorf("the opener got context %v, want the one of the binding", got)
	}
}

// A binding that drops its tunnel opens a new one with its next connection.
// The others keep the old one until they fail or stop, and it is closed
// with the last of them.
func TestDropOpensANewTunnel(t *testing.T) {
	f := register(t, "test.drop")
	spec := map[string]any{"via": "test.drop/shop/api:8080"}
	a, b, c := NewVia(spec), NewVia(spec), NewVia(spec)
	old := dial(t, a)
	dial(t, b)

	a.Drop()
	a.Drop()
	if _, closed := f.counts(); closed != 0 {
		t.Fatal("the tunnel was closed under the binding that still holds it")
	}
	fresh := dial(t, a)
	if fresh == old {
		t.Fatal("the binding went back into the tunnel it dropped")
	}
	if got := dial(t, c); got != fresh {
		t.Errorf("a binding that comes later went to %s, want the new tunnel at %s", got, fresh)
	}
	if got := dial(t, b); got != old {
		t.Errorf("the binding that did not fail was moved to %s", got)
	}
	b.Drop()
	if got := dial(t, b); got != fresh {
		t.Errorf("after its own failure the binding went to %s, want the new tunnel at %s", got, fresh)
	}
	if opened, closed := f.counts(); opened != 2 || closed != 1 || f.closed[0] != old {
		t.Errorf("opened %d, closed %v; want 2 opened and the old one closed", opened, f.closed)
	}
	a.Close()
	b.Close()
	c.Close()
	if _, closed := f.counts(); closed != 2 {
		t.Errorf("closed %d tunnels, want both", closed)
	}
}

// A tunnel whose local end does not answer is dropped by the connection
// that found out.
func TestDialDropsATunnelThatDoesNotAnswer(t *testing.T) {
	f := register(t, "test.dead")
	f.dead.Store(true)
	v := NewVia(map[string]any{"via": "test.dead/shop/api:8080"})
	defer v.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := v.Dial(ctx, "tcp", "api.shop.svc:8080")
	if err == nil || !strings.Contains(err.Error(), "via test.dead/shop/api:8080") {
		t.Fatalf("err = %v, want one that names the tunnel", err)
	}
	if _, closed := f.counts(); closed != 1 {
		t.Fatalf("the tunnel that did not answer was closed %d times, want once", closed)
	}
	f.dead.Store(false)
	dial(t, v)
	if opened, _ := f.counts(); opened != 2 {
		t.Errorf("opened %d tunnels, want a new one after the failure", opened)
	}
}

// A tunnel that cannot be opened says so in words that do not blame the
// server, and the next connection tries again.
func TestATunnelThatCannotBeOpened(t *testing.T) {
	var calls atomic.Int32
	RegisterTunnel("test.refused", func(context.Context, string, map[string]any) (*Tunnel, error) {
		calls.Add(1)
		return nil, errors.New("not allowed to port-forward")
	})
	v := NewVia(map[string]any{"via": "test.refused/shop/api:8080"})
	defer v.Close()
	for i := 0; i < 2; i++ {
		_, err := v.Dial(context.Background(), "tcp", "")
		if !errors.Is(err, ErrNoTunnel) || !strings.Contains(err.Error(), "via test.refused/shop/api:8080") || !strings.Contains(err.Error(), "not allowed to port-forward") {
			t.Fatalf("err = %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("the opener was asked %d times, want once per connection", calls.Load())
	}
}

// Bindings that start together wait for one opening.
func TestBindingsThatStartTogetherOpenOnce(t *testing.T) {
	f := register(t, "test.together")
	f.wait = make(chan struct{})
	spec := map[string]any{"via": "test.together/shop/api:8080"}
	const n = 5
	vias := make([]*Via, n)
	addrs := make([]string, n)
	var wg sync.WaitGroup
	for i := range vias {
		vias[i] = NewVia(spec)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, err := vias[i].Dial(ctx, "tcp", "")
			if err != nil {
				t.Errorf("binding %d: %v", i, err)
				return
			}
			addrs[i] = conn.RemoteAddr().String()
			conn.Close()
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(f.wait)
	wg.Wait()
	if opened, _ := f.counts(); opened != 1 {
		t.Fatalf("%d bindings opened %d tunnels, want 1", n, opened)
	}
	for i, a := range addrs {
		if a != f.opened[0] {
			t.Errorf("binding %d went to %q, want %s", i, a, f.opened[0])
		}
	}
	for _, v := range vias {
		v.Close()
	}
	if _, closed := f.counts(); closed != 1 {
		t.Errorf("closed %d times, want once", closed)
	}
}

func TestViaName(t *testing.T) {
	RegisterTunnel("test.service", func(context.Context, string, map[string]any) (*Tunnel, error) { return nil, nil })
	RegisterTunnel("test.pod", func(context.Context, string, map[string]any) (*Tunnel, error) { return nil, nil })
	for via, want := range map[string][2]string{
		"test.service/shop/cache:6379": {"cache.shop.svc", "6379"},
		"test.service/shop/api:http":   {"api.shop.svc", ""},
		"test.pod/shop/cache-0:6379":   {"cache-0", "6379"},
		"bastion":                      {"", ""},
		"":                             {"", ""},
	} {
		host, port := ViaName(via)
		if host != want[0] || port != want[1] {
			t.Errorf("ViaName(%q) = %q, %q, want %q, %q", via, host, port, want[0], want[1])
		}
	}
	for via, want := range map[string]string{
		"test.service/shop/api:8080": "http://api.shop.svc:8080",
		"test.service/shop/api:http": "http://api.shop.svc",
	} {
		if got := NewVia(map[string]any{"via": via}).URL(); got != want {
			t.Errorf("URL of %q = %q, want %q", via, got, want)
		}
	}
}
