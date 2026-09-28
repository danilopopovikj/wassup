package k8s

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/portforward"

	"github.com/danilopopovikj/wassup/internal/probe"
)

func TestParseTunnelTarget(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    tunnelTarget
		wantErr string
	}{
		{in: "shop/db-rw:5432", want: tunnelTarget{"shop", "db-rw", "5432"}},
		{in: "shop/db-rw:postgres", want: tunnelTarget{"shop", "db-rw", "postgres"}},
		{in: "shop/db-1:1", want: tunnelTarget{"shop", "db-1", "1"}},
		{in: "shop/db-1:65535", want: tunnelTarget{"shop", "db-1", "65535"}},
		{in: "", wantErr: "the namespace is missing"},
		{in: "db-rw:5432", wantErr: "the namespace is missing"},
		{in: "/db-rw:5432", wantErr: "the namespace is missing"},
		{in: "shop/db-rw", wantErr: "the port is missing"},
		{in: "shop/db-rw:", wantErr: "the port is missing"},
		{in: "shop/:5432", wantErr: "the name is missing"},
		{in: "shop/db/rw:5432", wantErr: "single words"},
		{in: "shop/db:rw:5432", wantErr: "single words"},
		{in: "shop/db rw:5432", wantErr: "single words"},
		{in: "shop/db-rw:0", wantErr: "not between 1 and 65535"},
		{in: "shop/db-rw:65536", wantErr: "not between 1 and 65535"},
		{in: "shop/db-rw:99999999999999999999", wantErr: "not between 1 and 65535"},
		{in: "shop/db-rw:54 32", wantErr: "a number or the name of a port"},
	} {
		got, err := parseTunnelTarget(tc.in)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tc.in, err)
		case tc.wantErr == "" && got != tc.want:
			t.Errorf("%q: got %+v, want %+v", tc.in, got, tc.want)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%q: got %+v, want an error with %q", tc.in, got, tc.wantErr)
		case tc.wantErr != "" && (!strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "<namespace>/<name>:<port>")):
			t.Errorf("%q: error %q, want it to say %q and the expected form", tc.in, err, tc.wantErr)
		}
	}
}

// dbService is the Service of the bookstore database.
func dbService(ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db-rw", Namespace: "shop"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "db", "role": "primary"}, Ports: ports},
	}
}

// dbPod is a pod of the bookstore database, ready and carrying the labels
// the Service selects, with one container port named "postgres".
func dbPod(name string, port int32, edit ...func(*corev1.Pod)) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": "db", "role": "primary"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres",
			Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9187}, {Name: "postgres", ContainerPort: port}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	for _, e := range edit {
		e(&p)
	}
	return p
}

func notReady(p *corev1.Pod) {
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
}

func terminating(p *corev1.Pod) {
	now := metav1.Now()
	p.DeletionTimestamp = &now
}

func otherLabels(p *corev1.Pod) { p.Labels = map[string]string{"app": "db", "role": "replica"} }

func otherNamespace(p *corev1.Pod) { p.Namespace = "staging" }

func TestResolveServiceForward(t *testing.T) {
	byNumber := corev1.ServicePort{Name: "postgres", Port: 5432, TargetPort: intstr.FromInt32(6432)}
	byName := corev1.ServicePort{Name: "postgres", Port: 5432, TargetPort: intstr.FromString("postgres")}
	unset := corev1.ServicePort{Name: "postgres", Port: 5432}
	metrics := corev1.ServicePort{Name: "metrics", Port: 9187, TargetPort: intstr.FromString("metrics")}
	udp := corev1.ServicePort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}

	for _, tc := range []struct {
		name    string
		svc     *corev1.Service
		pods    []corev1.Pod
		port    string
		want    forwardTarget
		wantErr string
	}{
		{name: "port by number, target by number",
			svc: dbService(byNumber), pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "5432",
			want: forwardTarget{"shop", "db-1", 6432}},
		{name: "port by name, target by number",
			svc: dbService(metrics, byNumber), pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "postgres",
			want: forwardTarget{"shop", "db-1", 6432}},
		{name: "target by name is looked up on the pod",
			svc: dbService(byName), pods: []corev1.Pod{dbPod("db-1", 15432)}, port: "5432",
			want: forwardTarget{"shop", "db-1", 15432}},
		{name: "second port of the service",
			svc: dbService(byName, metrics), pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "9187",
			want: forwardTarget{"shop", "db-1", 9187}},
		{name: "unset target is the service port",
			svc: dbService(unset), pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "5432",
			want: forwardTarget{"shop", "db-1", 5432}},
		{name: "first ready pod by name, whatever the order listed",
			svc: dbService(byNumber), pods: []corev1.Pod{dbPod("db-3", 5432), dbPod("db-1", 5432, notReady), dbPod("db-2", 5432)}, port: "5432",
			want: forwardTarget{"shop", "db-2", 6432}},
		{name: "a pod being deleted is passed over",
			svc: dbService(byNumber), pods: []corev1.Pod{dbPod("db-1", 5432, terminating), dbPod("db-2", 5432)}, port: "5432",
			want: forwardTarget{"shop", "db-2", 6432}},
		{name: "a pod the selector does not match is passed over",
			svc: dbService(byNumber), pods: []corev1.Pod{dbPod("db-1", 5432, otherLabels), dbPod("db-0", 5432, otherNamespace), dbPod("db-2", 5432)}, port: "5432",
			want: forwardTarget{"shop", "db-2", 6432}},
		{name: "a pod without the named port is passed over",
			svc: dbService(byName), pods: []corev1.Pod{
				dbPod("db-1", 5432, func(p *corev1.Pod) { p.Spec.Containers[0].Ports = nil }), dbPod("db-2", 15432)}, port: "5432",
			want: forwardTarget{"shop", "db-2", 15432}},

		{name: "no selector",
			svc:  &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "db-rw", Namespace: "shop"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{byNumber}}},
			pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "5432",
			wantErr: "service shop/db-rw has no selector, so there is no pod to forward to"},
		{name: "external name",
			svc: &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "db-rw", Namespace: "shop"},
				Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "db.bookstore.example"}},
			port:    "5432",
			wantErr: "service shop/db-rw has no selector (it points outside the cluster)"},
		{name: "unknown port number",
			svc: dbService(byNumber, metrics), pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "6432",
			wantErr: "service shop/db-rw has no port 6432; it has 5432 (postgres), 9187 (metrics)"},
		{name: "unknown port name",
			svc: dbService(byNumber), pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "pg",
			wantErr: "service shop/db-rw has no port pg; it has 5432 (postgres)"},
		{name: "no ports at all",
			svc: dbService(), pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "5432",
			wantErr: "service shop/db-rw has no ports"},
		{name: "not TCP",
			svc: dbService(udp), pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "53",
			wantErr: "is UDP, only TCP can be forwarded"},
		{name: "no pods",
			svc: dbService(byNumber), port: "5432",
			wantErr: "service shop/db-rw has no ready pod to forward to (0 match its selector)"},
		{name: "no ready pod",
			svc: dbService(byNumber), pods: []corev1.Pod{dbPod("db-1", 5432, notReady), dbPod("db-2", 5432, terminating)}, port: "5432",
			wantErr: "service shop/db-rw has no ready pod to forward to (2 match its selector)"},
		{name: "named target on no pod",
			svc: dbService(corev1.ServicePort{Port: 5432, TargetPort: intstr.FromString("pg")}), pods: []corev1.Pod{dbPod("db-1", 5432)}, port: "5432",
			wantErr: `no ready pod of service shop/db-rw has a container port named "pg"`},
	} {
		got, err := resolveServiceForward(tc.svc, tc.pods, tc.port)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr == "" && got != tc.want:
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%s: got %+v, want an error with %q", tc.name, got, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: error %q, want it to say %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestResolvePodPort(t *testing.T) {
	pod := dbPod("db-1", 15432)
	for _, tc := range []struct {
		name    string
		pod     *corev1.Pod
		port    string
		want    int
		wantErr string
	}{
		{name: "a number needs no pod", port: "5432", want: 5432},
		{name: "a number is taken as it is", pod: &pod, port: "8080", want: 8080},
		{name: "a name is looked up", pod: &pod, port: "postgres", want: 15432},
		{name: "unknown name", pod: &pod, port: "pg", wantErr: `pod shop/db-1 has no container port named "pg"`},
		{name: "a name without the pod", port: "postgres", wantErr: "needs the pod"},
		{name: "out of range", port: "70000", wantErr: "not between 1 and 65535"},
	} {
		got, err := resolvePodPort(tc.pod, tc.port)
		switch {
		case tc.wantErr == "" && (err != nil || got != tc.want):
			t.Errorf("%s: got %d, %v, want %d", tc.name, got, err, tc.want)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error %v, want it to say %q", tc.name, err, tc.wantErr)
		}
	}
}

// recordedForward is a tunneler on a fake cluster whose forwarding records
// where it was asked to go instead of going there.
type recordedForward struct {
	tunneler
	core  *fake.Clientset
	to    []forwardTarget
	specs []map[string]any
}

func newRecordedForward(objs ...runtime.Object) *recordedForward {
	r := &recordedForward{core: fake.NewSimpleClientset(objs...)}
	r.clients = func(spec map[string]any) (*Clients, error) {
		r.specs = append(r.specs, spec)
		return &Clients{Core: r.core}, nil
	}
	r.forward = func(_ context.Context, _ *Clients, to forwardTarget) (*probe.Tunnel, error) {
		r.to = append(r.to, to)
		return &probe.Tunnel{Addr: "127.0.0.1:40123", Close: func() {}}, nil
	}
	return r
}

// did reports whether the fake cluster saw the verb on the resource.
func (r *recordedForward) did(verb, resource string) bool {
	for _, a := range r.core.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			return true
		}
	}
	return false
}

func forbidden(resource, name string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, name,
		errors.New(`User "reader" cannot get resource "`+resource+`" in API group "" in the namespace "shop"`))
}

func TestOpenServiceTunnel(t *testing.T) {
	svc := dbService(corev1.ServicePort{Name: "postgres", Port: 5432, TargetPort: intstr.FromString("postgres")})
	ready, replica, down := dbPod("db-2", 15432), dbPod("db-1", 15432, otherLabels), dbPod("db-0", 15432, notReady)
	r := newRecordedForward(svc, &ready, &replica, &down)
	spec := map[string]any{"kubeconfig": "/home/reader/.kube/bookstore", "context": "bookstore"}

	tun, err := r.openService(t.Context(), "shop/db-rw:5432", spec)
	if err != nil {
		t.Fatalf("openService: %v", err)
	}
	if tun.Addr != "127.0.0.1:40123" {
		t.Errorf("Addr = %q", tun.Addr)
	}
	if len(r.to) != 1 || r.to[0] != (forwardTarget{"shop", "db-2", 15432}) {
		t.Errorf("forwarded to %+v, want db-2 port 15432", r.to)
	}
	// The binding's kubeconfig and context reach the client resolution.
	if len(r.specs) != 1 || r.specs[0]["kubeconfig"] != spec["kubeconfig"] || r.specs[0]["context"] != "bookstore" {
		t.Errorf("clients asked for with %v", r.specs)
	}
	if !r.did("get", "services") || !r.did("list", "pods") {
		t.Errorf("actions = %v", r.core.Actions())
	}
}

func TestOpenServiceTunnelErrors(t *testing.T) {
	svc := dbService(corev1.ServicePort{Name: "postgres", Port: 5432})
	pod := dbPod("db-1", 5432)
	noSelector := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "shop"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 5432}}}}

	for _, tc := range []struct {
		name    string
		target  string
		deny    string // resource whose reads are forbidden
		wantErr []string
	}{
		{name: "bad target", target: "db-rw", wantErr: []string{"<namespace>/<name>:<port>"}},
		{name: "no such service", target: "shop/orders:5432", wantErr: []string{"service shop/orders not found"}},
		{name: "no selector", target: "shop/legacy:5432", wantErr: []string{"service shop/legacy has no selector"}},
		{name: "no such port", target: "shop/db-rw:3306", wantErr: []string{"service shop/db-rw has no port 3306"}},
		{name: "services denied", target: "shop/db-rw:5432", deny: "services",
			wantErr: []string{"not allowed to read service shop/db-rw", "the account needs get on services in namespace shop"}},
		{name: "pods denied", target: "shop/db-rw:5432", deny: "pods",
			wantErr: []string{"not allowed to read pods of service shop/db-rw", "the account needs list on pods in namespace shop"}},
	} {
		r := newRecordedForward(svc, noSelector, &pod)
		if tc.deny != "" {
			r.core.PrependReactor("*", tc.deny, func(a k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, forbidden(tc.deny, "")
			})
		}
		tun, err := r.openService(t.Context(), tc.target, nil)
		if err == nil {
			t.Errorf("%s: got a tunnel to %v, want an error", tc.name, tun.Addr)
			continue
		}
		for _, want := range tc.wantErr {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q, want it to say %q", tc.name, err, want)
			}
		}
		if len(r.to) != 0 {
			t.Errorf("%s: forwarded to %+v, want nothing opened", tc.name, r.to)
		}
	}
}

func TestOpenPodTunnel(t *testing.T) {
	pod := dbPod("db-1", 15432)

	// By number the pod is not read: the forward is all the access needed.
	r := newRecordedForward(&pod)
	if _, err := r.openPod(t.Context(), "shop/db-1:5432", nil); err != nil {
		t.Fatalf("openPod by number: %v", err)
	}
	if len(r.to) != 1 || r.to[0] != (forwardTarget{"shop", "db-1", 5432}) {
		t.Errorf("forwarded to %+v, want db-1 port 5432", r.to)
	}
	if len(r.core.Actions()) != 0 {
		t.Errorf("actions = %v, want none for a port by number", r.core.Actions())
	}

	// By name the port is looked up on the pod.
	r = newRecordedForward(&pod)
	if _, err := r.openPod(t.Context(), "shop/db-1:postgres", nil); err != nil {
		t.Fatalf("openPod by name: %v", err)
	}
	if len(r.to) != 1 || r.to[0] != (forwardTarget{"shop", "db-1", 15432}) {
		t.Errorf("forwarded to %+v, want db-1 port 15432", r.to)
	}

	for _, tc := range []struct{ target, wantErr string }{
		{"shop/db-9:postgres", "pod shop/db-9 not found"},
		{"shop/db-1:pg", `pod shop/db-1 has no container port named "pg"`},
		{"shop/db-1", "the port is missing"},
	} {
		r = newRecordedForward(&pod)
		if _, err := r.openPod(t.Context(), tc.target, nil); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error %v, want it to say %q", tc.target, err, tc.wantErr)
		}
		if len(r.to) != 0 {
			t.Errorf("%s: forwarded to %+v, want nothing opened", tc.target, r.to)
		}
	}
}

func TestTunnelClientErrors(t *testing.T) {
	r := &tunneler{clients: func(map[string]any) (*Clients, error) {
		return nil, errors.New("kubeconfig /home/reader/.kube/bookstore: no such file")
	}}
	if _, err := r.openService(t.Context(), "shop/db-rw:5432", nil); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error = %v, want the kubeconfig's", err)
	}
	r = &tunneler{clients: func(map[string]any) (*Clients, error) { return &Clients{}, nil }}
	if _, err := r.openPod(t.Context(), "shop/db-1:5432", nil); err == nil || !strings.Contains(err.Error(), "no Kubernetes client") {
		t.Errorf("error = %v, want it to say there is no client", err)
	}
}

// The real forward needs an API server configuration; the fake cluster has
// none, which must be an error and not a panic.
func TestSpdyForwardNeedsAServer(t *testing.T) {
	_, err := spdyForward(t.Context(), &Clients{Core: fake.NewSimpleClientset()}, forwardTarget{"shop", "db-1", 5432})
	if err == nil || !strings.Contains(err.Error(), "pod shop/db-1 port 5432") {
		t.Errorf("error = %v, want it to name the target", err)
	}
}

// The forward itself needs a cluster, the way to it does not: against a
// local server that refuses, the request goes to the pod's portforward
// subresource and the refusal comes back in plain words.
func TestSpdyForwardRefused(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,` +
			`"message":"pods \"db-1\" is forbidden: User \"reader\" cannot create resource \"pods/portforward\" in API group \"\" in the namespace \"shop\""}`))
	}))
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL, TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	tuneConfig(cfg)
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var tun *probe.Tunnel
	out := captureStderr(t, func() {
		tun, err = spdyForward(t.Context(), &Clients{Core: core, REST: cfg}, forwardTarget{"shop", "db-1", 5432})
	})
	if err == nil {
		tun.Close()
		t.Fatalf("got a tunnel to %s through a server that refuses", tun.Addr)
	}
	for _, want := range []string{"not allowed to port-forward to pod shop/db-1 port 5432", "the account needs create on pods/portforward in namespace shop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q, want it to say %q", err, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "POST /api/v1/namespaces/shop/pods/db-1/portforward" {
		t.Errorf("requests = %v, want one POST to the pod's portforward", asked)
	}
	if out != "" {
		t.Errorf("the forward wrote to stderr:\n%s", out)
	}
}

func TestTunnelSchemesRegistered(t *testing.T) {
	for _, via := range []string{"k8s.service/shop/db-rw:5432", "k8s.pod/shop/db-1:5432"} {
		scheme, target, ok := probe.SplitVia(via)
		if !ok || scheme == "" || !strings.HasPrefix(target, "shop/") {
			t.Errorf("%s: scheme %q, target %q, ok %v", via, scheme, target, ok)
		}
	}
	if _, _, ok := probe.SplitVia("k8s.deployment/shop/api:8000"); ok {
		t.Errorf("k8s.deployment is not a scheme of this package")
	}
}

func TestForwardError(t *testing.T) {
	to := forwardTarget{"shop", "db-1", 5432}
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"denied by the API server",
			errors.New(`error upgrading connection: pods "db-1" is forbidden: User "reader" cannot create resource "pods/portforward" in API group "" in the namespace "shop"`),
			[]string{"not allowed to port-forward to pod shop/db-1 port 5432", "the account needs create on pods/portforward in namespace shop", `User "reader"`}},
		{"denied, typed", forbidden("pods/portforward", "db-1"),
			[]string{"the account needs create on pods/portforward in namespace shop"}},
		{"pod gone", errors.New(`error upgrading connection: pods "db-1" not found`),
			[]string{"port-forward to pod shop/db-1 port 5432: the pod was not found"}},
		{"too slow", context.DeadlineExceeded,
			[]string{"port-forward to pod shop/db-1 port 5432 was not ready in time"}},
		{"anything else", errors.New("error upgrading connection: dial tcp 203.0.113.10:6443: connect: connection refused"),
			[]string{"port-forward to pod shop/db-1 port 5432: error upgrading connection", "connection refused"}},
	} {
		got := forwardError(to, tc.err)
		for _, want := range tc.want {
			if !strings.Contains(got.Error(), want) {
				t.Errorf("%s: %q, want it to say %q", tc.name, got, want)
			}
		}
		if !errors.Is(got, tc.err) {
			t.Errorf("%s: the cause is not kept in %q", tc.name, got)
		}
	}
}

// fakeForwarder stands in for the library's forwarder: it listens at once
// (or fails, or never gets there) and runs until its lifetime ends.
type fakeForwarder struct {
	live  context.Context
	ready chan struct{}

	failWith error         // ForwardPorts fails with it before listening
	never    bool          // ForwardPorts never listens
	deaf     bool          // ForwardPorts does not end when stopped
	local    uint16        // the local port reported
	portsErr error         // GetPorts fails with it
	stopped  chan struct{} // closed when ForwardPorts returns
	mu       sync.Mutex
	runs     int
}

func (f *fakeForwarder) ForwardPorts() error {
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()
	defer close(f.stopped)
	if f.failWith != nil {
		return f.failWith
	}
	if !f.never {
		close(f.ready)
	}
	if f.deaf {
		select {}
	}
	<-f.live.Done()
	return nil
}

func (f *fakeForwarder) GetPorts() ([]portforward.ForwardedPort, error) {
	if f.portsErr != nil {
		return nil, f.portsErr
	}
	return []portforward.ForwardedPort{{Local: f.local, Remote: 5432}}, nil
}

// build returns the factory openTunnel calls, filling in what it hands over.
func (f *fakeForwarder) build(live context.Context, ready chan struct{}) (forwarder, error) {
	f.live, f.ready, f.stopped = live, ready, make(chan struct{})
	return f, nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestTunnelOutlivesTheOpeningContext(t *testing.T) {
	f := &fakeForwarder{local: 40123}
	ctx, cancel := context.WithCancel(context.Background())
	tun, err := openTunnel(ctx, time.Second, f.build)
	if err != nil {
		t.Fatalf("openTunnel: %v", err)
	}
	if tun.Addr != "127.0.0.1:40123" {
		t.Errorf("Addr = %q, want 127.0.0.1:40123", tun.Addr)
	}

	// The round that opened the tunnel is over; the tunnel is not.
	cancel()
	time.Sleep(50 * time.Millisecond)
	if isClosed(f.stopped) || f.live.Err() != nil {
		t.Fatalf("the tunnel ended with the context that opened it")
	}

	tun.Close()
	if !isClosed(f.stopped) {
		t.Errorf("Close returned before the forwarder ended")
	}
	if f.live.Err() == nil {
		t.Errorf("Close did not stop the forwarder")
	}
}

func TestTunnelCloseIsIdempotent(t *testing.T) {
	f := &fakeForwarder{local: 40123}
	tun, err := openTunnel(t.Context(), time.Second, f.build)
	if err != nil {
		t.Fatalf("openTunnel: %v", err)
	}
	tun.Close()
	tun.Close()

	// From several goroutines at once, as a probe and the runtime may.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tun.Close()
		}()
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("repeated Close did not return")
	}
	if !isClosed(f.stopped) || f.runs != 1 {
		t.Errorf("forwarder stopped %v after %d runs, want stopped after one", isClosed(f.stopped), f.runs)
	}
}

// A forwarder that does not end must not hold Close for longer than the wait.
func TestTunnelCloseIsBounded(t *testing.T) {
	f := &fakeForwarder{local: 40123, deaf: true}
	tun, err := openTunnel(t.Context(), 100*time.Millisecond, f.build)
	if err != nil {
		t.Fatalf("openTunnel: %v", err)
	}
	start := time.Now()
	tun.Close()
	if took := time.Since(start); took < 100*time.Millisecond || took > 2*time.Second {
		t.Errorf("Close took %s, want about the wait of 100ms", took)
	}
	start = time.Now()
	tun.Close()
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Errorf("second Close took %s, want it to return at once", took)
	}
	if tunnelCloseWait != 2*time.Second {
		t.Errorf("tunnelCloseWait = %s, want 2s", tunnelCloseWait)
	}
}

func TestOpenTunnelFailures(t *testing.T) {
	denied := errors.New(`error upgrading connection: pods "db-1" is forbidden`)

	// The forwarder fails before it listens.
	f := &fakeForwarder{failWith: denied}
	if tun, err := openTunnel(t.Context(), time.Second, f.build); !errors.Is(err, denied) || tun != nil {
		t.Errorf("failed forwarder: tunnel %v, error %v", tun, err)
	}

	// The forwarder cannot be built.
	broken := errors.New("you must specify at least 1 port")
	_, err := openTunnel(t.Context(), time.Second, func(context.Context, chan struct{}) (forwarder, error) { return nil, broken })
	if !errors.Is(err, broken) {
		t.Errorf("unbuildable forwarder: error %v", err)
	}

	// The forwarder never listens: the opening context bounds the wait, and
	// the forwarder is told to stop.
	f = &fakeForwarder{never: true}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	tun, err := openTunnel(ctx, time.Second, f.build)
	if !errors.Is(err, context.DeadlineExceeded) || tun != nil {
		t.Errorf("slow forwarder: tunnel %v, error %v", tun, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("waited %s for a forwarder that never listens", took)
	}
	select {
	case <-f.stopped:
	case <-time.After(2 * time.Second):
		t.Errorf("the forwarder was left running after the opening failed")
	}

	// The forwarder listens but cannot say where.
	for _, f := range []*fakeForwarder{{portsErr: errors.New("listeners not ready")}, {local: 0}} {
		tun, err := openTunnel(t.Context(), time.Second, f.build)
		if err == nil || tun != nil {
			t.Errorf("no local port: tunnel %v, error %v", tun, err)
		}
		if !isClosed(f.stopped) {
			t.Errorf("no local port: the forwarder was left running")
		}
	}
}
