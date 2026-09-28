package k8s

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// selfSignedDER returns a certificate for host valid from notBefore to
// notAfter, and its key.
func selfSignedDER(t *testing.T, host string, notBefore, notAfter time.Time) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		Issuer:       pkix.Name{CommonName: "R3"},
		DNSNames:     []string{host},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

// selfSigned returns a PEM certificate for host valid from notBefore to notAfter.
func selfSigned(t *testing.T, host string, notBefore, notAfter time.Time) []byte {
	t.Helper()
	der, _ := selfSignedDER(t, host, notBefore, notAfter)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// dialRecorder is an injected dialer that counts the handshakes asked for.
type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
	// to is the listener every dial is sent to; empty refuses the dial.
	to string
}

func (d *dialRecorder) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	if d.to == "" {
		return nil, errors.New("connection refused")
	}
	return (&net.Dialer{}).DialContext(ctx, network, d.to)
}

func (d *dialRecorder) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// secretReads counts the reads of secrets recorded by the fake clientset.
func secretReads(t *testing.T, c *Clients) int {
	t.Helper()
	cs, ok := c.Core.(*fake.Clientset)
	if !ok {
		t.Fatalf("core client is %T, not the fake", c.Core)
	}
	n := 0
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "secrets" {
			n++
		}
	}
	return n
}

// tlsSecret is the TLS secret of the bookstore ingress holding crt.
func tlsSecret(crt []byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bookstore-tls", Namespace: "prod"}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: crt, corev1.TLSPrivateKeyKey: []byte("x")}}
}

func ingressObj() *networkingv1.Ingress {
	pt := networkingv1.PathTypePrefix
	path := networkingv1.HTTPIngressPath{Path: "/", PathType: &pt, Backend: networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{Name: "api", Port: networkingv1.ServiceBackendPort{Number: 8000}}}}
	rule := networkingv1.IngressRule{Host: "bookstore.example", IngressRuleValue: networkingv1.IngressRuleValue{
		HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{path}}}}
	return &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
		Spec: networkingv1.IngressSpec{
			TLS:   []networkingv1.IngressTLS{{Hosts: []string{"bookstore.example"}, SecretName: "bookstore-tls"}},
			Rules: []networkingv1.IngressRule{rule},
		}}
}

// cert-manager knows the expiry: neither the secret nor the network is asked.
func TestIngressCertDays(t *testing.T) {
	now := time.Now()
	notAfter := now.Add(10*24*time.Hour + time.Hour).UTC().Truncate(time.Second)
	// The secret holds another date, so a read of it would show in cert_days.
	secret := tlsSecret(selfSigned(t, "bookstore.example", now.Add(-80*24*time.Hour), now.Add(40*24*time.Hour)))
	cert := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": "bookstore", "namespace": "prod"},
		"spec":     map[string]any{"secretName": "bookstore-tls", "issuerRef": map[string]any{"kind": "ClusterIssuer", "name": "letsencrypt"}},
		"status": map[string]any{
			"notBefore": now.Add(-80 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"notAfter":  notAfter.Format(time.RFC3339),
			"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "Failed",
				"message": "The certificate request has failed to complete: DNS-01 challenge failed", "lastTransitionTime": "2026-09-27T10:00:00Z"}}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"}, cert)
	c := newTestClients([]runtime.Object{ingressObj(), secret}, nil, dyn)
	dials := &dialRecorder{}
	p := &ingressProbe{base: base{kind: kindIngress, clients: c}, dial: dials.dial}
	out, _ := startProbe(t, p, testSpec("ingress", "namespace", "prod", "name", "web"))
	o := firstOK(t, out)

	if d := o.Metrics["cert_days"]; d < 9.9 || d > 10.1 {
		t.Errorf("cert_days = %v", d)
	}
	if o.Detail["cert_source"] != "cert-manager" {
		t.Errorf("cert_source = %v, want cert-manager", o.Detail["cert_source"])
	}
	if n := secretReads(t, c); n != 0 {
		t.Errorf("%d reads of secrets, want none", n)
	}
	if got := dials.dialed(); len(got) != 0 {
		t.Errorf("handshakes with %v, want none when cert-manager knows the expiry", got)
	}
	// CertificateFacet names the certificate after the component.
	exp, ok := model.HasCondition(o.Conditions, model.CondCertExpiring)
	if !ok || exp.Ref != "certificate/ingress" || exp.Detail != "10 days left" {
		t.Errorf("CertExpiring = %+v (ok %v)", exp, ok)
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondCertExpired); ok {
		t.Errorf("unexpected CertExpired")
	}
	rf, ok := model.HasCondition(o.Conditions, model.CondCertRenewalFailed)
	if !ok || rf.Detail != "The certificate request has failed to complete: DNS-01 challenge failed" || rf.Since.Format(time.RFC3339) != "2026-09-27T10:00:00Z" {
		t.Errorf("CertRenewalFailed = %+v (ok %v)", rf, ok)
	}
	if hosts, _ := o.Detail["hosts"].([]string); len(hosts) != 1 || hosts[0] != "bookstore.example" {
		t.Errorf("hosts = %v", o.Detail["hosts"])
	}
	if o.Detail["issuer"] != "ClusterIssuer/letsencrypt" {
		t.Errorf("issuer = %v", o.Detail["issuer"])
	}
	if b, _ := o.Detail["backends"].([]string); len(b) != 1 || b[0] != "api:8000" {
		t.Errorf("backends = %v", o.Detail["backends"])
	}
}

// Without cert-manager and without an answer from the host, the secret is
// still not read: the observation carries no cert_days and says why.
func TestIngressDoesNotReadTheSecretByDefault(t *testing.T) {
	now := time.Now()
	secret := tlsSecret(selfSigned(t, "bookstore.example", now.Add(-80*24*time.Hour), now.Add(10*24*time.Hour+time.Hour)))
	c := newTestClients([]runtime.Object{ingressObj(), secret}, nil, nil)
	dials := &dialRecorder{}
	p := &ingressProbe{base: base{kind: kindIngress, clients: c}, dial: dials.dial}
	out, _ := startProbe(t, p, testSpec("ingress", "namespace", "prod", "name", "web"))
	o := firstOK(t, out)

	if n := secretReads(t, c); n != 0 {
		t.Errorf("%d reads of secrets, want none without read_tls_secret", n)
	}
	if d, ok := o.Metrics["cert_days"]; ok {
		t.Errorf("cert_days = %v, want it left out when no source knows the expiry", d)
	}
	if len(o.Conditions) != 0 {
		t.Errorf("conditions = %+v", o.Conditions)
	}
	if _, ok := o.Detail["cert_source"]; ok {
		t.Errorf("cert_source = %v, want none", o.Detail["cert_source"])
	}
	note, _ := o.Detail["cert_note"].(string)
	for _, want := range []string{"no cert-manager certificate for secret bookstore-tls", "handshake with bookstore.example:443", "read_tls_secret: true"} {
		if !strings.Contains(note, want) {
			t.Errorf("cert_note lacks %q: %q", want, note)
		}
	}
	if got := dials.dialed(); len(got) != 1 || got[0] != "bookstore.example:443" {
		t.Errorf("handshakes with %v, want one with bookstore.example:443", got)
	}
}

// With read_tls_secret the secret is the last resort and is read.
func TestIngressReadsTheSecretWhenOptedIn(t *testing.T) {
	now := time.Now()
	secret := tlsSecret(selfSigned(t, "bookstore.example", now.Add(-80*24*time.Hour), now.Add(10*24*time.Hour+time.Hour)))
	c := newTestClients([]runtime.Object{ingressObj(), secret}, nil, nil)
	p := &ingressProbe{base: base{kind: kindIngress, clients: c}, dial: (&dialRecorder{}).dial}
	spec := testSpec("ingress", "namespace", "prod", "name", "web")
	spec["read_tls_secret"] = true
	out, _ := startProbe(t, p, spec)
	o := firstOK(t, out)

	if d, ok := o.Metrics["cert_days"]; !ok || d < 9.9 || d > 10.1 {
		t.Errorf("cert_days = %v (present %v)", d, ok)
	}
	if o.Detail["cert_source"] != "secret" {
		t.Errorf("cert_source = %v, want secret", o.Detail["cert_source"])
	}
	if _, ok := o.Detail["cert_note"]; ok {
		t.Errorf("cert_note = %v, want none", o.Detail["cert_note"])
	}
	if n := secretReads(t, c); n == 0 {
		t.Errorf("no read of the secret with read_tls_secret: true")
	}
	if o.Detail["issuer"] != "bookstore.example" {
		t.Errorf("issuer = %v", o.Detail["issuer"])
	}
}

// The host itself answers: the expiry comes from the certificate it presents.
func TestIngressCertDaysFromHandshake(t *testing.T) {
	now := time.Now()
	der, key := selfSignedDER(t, "bookstore.example", now.Add(-80*24*time.Hour), now.Add(10*24*time.Hour+time.Hour))
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		asked = append(asked, h.ServerName)
		mu.Unlock()
		return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
	}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// The secret holds another date, so a read of it would show in cert_days.
	secret := tlsSecret(selfSigned(t, "bookstore.example", now.Add(-80*24*time.Hour), now.Add(40*24*time.Hour)))
	c := newTestClients([]runtime.Object{ingressObj(), secret}, nil, nil)
	dials := &dialRecorder{to: srv.Listener.Addr().String()}
	p := &ingressProbe{base: base{kind: kindIngress, clients: c}, dial: dials.dial}
	out, _ := startProbe(t, p, testSpec("ingress", "namespace", "prod", "name", "web"))
	o := firstOK(t, out)

	if d, ok := o.Metrics["cert_days"]; !ok || d < 9.9 || d > 10.1 {
		t.Errorf("cert_days = %v (present %v)", d, ok)
	}
	if o.Detail["cert_source"] != "handshake" {
		t.Errorf("cert_source = %v, want handshake", o.Detail["cert_source"])
	}
	if exp, ok := model.HasCondition(o.Conditions, model.CondCertExpiring); !ok || exp.Detail != "10 days left" {
		t.Errorf("CertExpiring = %+v (ok %v)", exp, ok)
	}
	if n := secretReads(t, c); n != 0 {
		t.Errorf("%d reads of secrets, want none", n)
	}
	mu.Lock()
	names := append([]string(nil), asked...)
	mu.Unlock()
	if len(names) == 0 || names[0] != "bookstore.example" {
		t.Errorf("server names asked for = %v, want bookstore.example", names)
	}

	// The answer is kept: the next ticks report it without a new handshake.
	firstOK(t, out)
	o = firstOK(t, out)
	if d := o.Metrics["cert_days"]; d < 9.9 || d > 10.1 || o.Detail["cert_source"] != "handshake" {
		t.Errorf("later tick: cert_days = %v, cert_source = %v", d, o.Detail["cert_source"])
	}
	if got := dials.dialed(); len(got) != 1 || got[0] != "bookstore.example:443" {
		t.Errorf("handshakes with %v, want one with bookstore.example:443", got)
	}
}

func TestIngressValidateReadTLSSecret(t *testing.T) {
	p := &ingressProbe{base: base{kind: kindIngress}}
	for _, tc := range []struct {
		name  string
		value any
		set   bool
		ok    bool
	}{
		{"absent", nil, false, true},
		{"true", true, true, true},
		{"false", false, true, true},
		{"a word", "yes", true, false},
		{"a quoted true", "true", true, false},
		{"a number", 1, true, false},
	} {
		spec := map[string]any{"namespace": "prod", "name": "web"}
		if tc.set {
			spec["read_tls_secret"] = tc.value
		}
		err := p.Validate(spec)
		if (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v, want ok %v", tc.name, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "read_tls_secret") {
			t.Errorf("%s: the error does not name the field: %v", tc.name, err)
		}
	}
	a, _ := probe.AccessFor(kindIngress)
	if !strings.Contains(strings.Join(a.SpecFields, " "), "read_tls_secret") {
		t.Errorf("SpecFields = %v, want read_tls_secret", a.SpecFields)
	}
	if !strings.Contains(a.Needs, "only when read_tls_secret is true") || !strings.Contains(a.Source, "only when read_tls_secret is true") {
		t.Errorf("access does not say the secrets are opt-in: source %q, needs %q", a.Source, a.Needs)
	}
}

// The renewal is seen through the secret here, so the binding opts in.
func TestIngressCertExpiredAndRenewalEvent(t *testing.T) {
	now := time.Now()
	crt := selfSigned(t, "bookstore.example", now.Add(-90*24*time.Hour), now.Add(-time.Hour))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bookstore-tls", Namespace: "prod"}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: crt}}
	c := newTestClients([]runtime.Object{ingressObj(), secret}, nil, nil)
	p := &ingressProbe{base: base{kind: kindIngress, clients: c}, dial: (&dialRecorder{}).dial}
	spec := testSpec("ingress", "namespace", "prod", "name", "web")
	spec["read_tls_secret"] = true
	out, _ := startProbe(t, p, spec)
	o := firstOK(t, out)
	if o.Metrics["cert_days"] > 0 {
		t.Errorf("cert_days = %v", o.Metrics["cert_days"])
	}
	if _, ok := model.HasCondition(o.Conditions, model.CondCertExpired); !ok {
		t.Errorf("no CertExpired in %+v", o.Conditions)
	}
	// Renewal: a fresh certificate replaces the secret.
	secret.Data[corev1.TLSCertKey] = selfSigned(t, "bookstore.example", now.Add(-time.Minute), now.Add(90*24*time.Hour))
	if _, err := c.Core.CoreV1().Secrets("prod").Update(t.Context(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	o = waitFor(t, out, func(o probe.Observation) bool { return len(o.Events) > 0 })
	if o.Events[0].Kind != "cert" || o.Events[0].Summary != "cert renewal of ingress" || o.Events[0].Ref != "bookstore-tls" {
		t.Errorf("event = %+v", o.Events[0])
	}
	if o.Metrics["cert_days"] < 89 || len(o.Conditions) != 0 {
		t.Errorf("after renewal: %v %+v", o.Metrics, o.Conditions)
	}
}
