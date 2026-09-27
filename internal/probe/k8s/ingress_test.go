package k8s

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// selfSigned returns a PEM certificate for host valid from notBefore to notAfter.
func selfSigned(t *testing.T, host string, notBefore, notAfter time.Time) []byte {
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
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

func TestIngressCertDays(t *testing.T) {
	now := time.Now()
	crt := selfSigned(t, "bookstore.example", now.Add(-80*24*time.Hour), now.Add(10*24*time.Hour+time.Hour))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bookstore-tls", Namespace: "prod"}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: crt, corev1.TLSPrivateKeyKey: []byte("x")}}
	cert := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": "bookstore", "namespace": "prod"},
		"spec":     map[string]any{"secretName": "bookstore-tls", "issuerRef": map[string]any{"kind": "ClusterIssuer", "name": "letsencrypt"}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "Failed",
			"message": "The certificate request has failed to complete: DNS-01 challenge failed", "lastTransitionTime": "2026-09-27T10:00:00Z"}}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"}, cert)
	c := newTestClients([]runtime.Object{ingressObj(), secret}, nil, dyn)
	p := &ingressProbe{base: base{kind: kindIngress, clients: c}}
	out, _ := startProbe(t, p, testSpec("ingress", "namespace", "prod", "name", "web"))
	o := firstOK(t, out)

	if d := o.Metrics["cert_days"]; d < 9.9 || d > 10.1 {
		t.Errorf("cert_days = %v", d)
	}
	exp, ok := model.HasCondition(o.Conditions, model.CondCertExpiring)
	if !ok || exp.Ref != "secret/bookstore-tls" {
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

func TestIngressCertExpiredAndRenewalEvent(t *testing.T) {
	now := time.Now()
	crt := selfSigned(t, "bookstore.example", now.Add(-90*24*time.Hour), now.Add(-time.Hour))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bookstore-tls", Namespace: "prod"}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: crt}}
	c := newTestClients([]runtime.Object{ingressObj(), secret}, nil, nil)
	p := &ingressProbe{base: base{kind: kindIngress, clients: c}}
	out, _ := startProbe(t, p, testSpec("ingress", "namespace", "prod", "name", "web"))
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
