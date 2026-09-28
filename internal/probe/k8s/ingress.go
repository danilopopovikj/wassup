package k8s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	networkinglisters "k8s.io/client-go/listers/networking/v1"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

const kindIngress = "k8s.ingress"

// certExpiringDays is the warning horizon for CertExpiring.
const certExpiringDays = 14

// specReadTLSSecret is the spec field that allows reading the TLS secrets.
const specReadTLSSecret = "read_tls_secret"

// Where a certificate's expiry came from, as written to Detail["cert_source"].
const (
	sourceCertManager = "cert-manager"
	sourceHandshake   = "handshake"
	sourceSecret      = "secret"
)

const (
	// tlsPort is where an ingress host serves its certificate.
	tlsPort = "443"
	// handshakeTimeout bounds one handshake. The probe's loop waits for it,
	// so a host that does not answer must not hold the tick for long.
	handshakeTimeout = 3 * time.Second
	// handshakeEvery is how long a certificate read by handshake is kept. An
	// expiry date moves on a renewal, weeks apart; a handshake with the
	// public host on every tick would be traffic for nothing.
	handshakeEvery = 5 * time.Minute
	// handshakeRetry is how long a failed handshake is kept, so a host that
	// cannot be reached costs one timeout a minute, not one per tick.
	handshakeRetry = time.Minute
)

// certificateGVR is the cert-manager Certificate resource.
var certificateGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

func init() {
	probe.Register(probe.Access{
		Kind:        kindIngress,
		Source:      "Kubernetes API (ingress informer); the certificate's expiry from cert-manager Certificates when installed, else from a TLS handshake with the ingress host; the TLS secrets only when read_tls_secret is true",
		Delivers:    "cert_days; CertExpired, CertExpiring, CertRenewalFailed; cert (renewal) events",
		SpecFields:  []string{"namespace (required)", "name (required)", "read_tls_secret", "kubeconfig", "context"},
		Needs:       "get/list/watch on ingresses; list on certificates.cert-manager.io (optional); TCP access to the ingress hosts on port 443 (optional); get on the TLS secrets in the namespace only when read_tls_secret is true",
		Implemented: true,
		Facets:      []string{facet.NameIngress, facet.NameCertificate},
		// Not the TLS secrets: read_tls_secret is an opt-in, and whoever
		// opts in grants that one read by hand.
		RBAC: []probe.Rule{
			probe.Reads("networking.k8s.io", "ingresses"),
			probe.Reads("cert-manager.io", "certificates"),
		},
	}, func() probe.Probe { return &ingressProbe{base: base{kind: kindIngress}} })
}

// dialFunc opens the connection a handshake runs on.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// handshakeResult is one remembered handshake with a host.
type handshakeResult struct {
	at   time.Time
	leaf *x509.Certificate
	err  error
}

// ingressProbe is k8s.ingress.
type ingressProbe struct {
	base
	notAfter map[string]time.Time
	emitted  map[string]bool

	// readSecret is the binding's read_tls_secret. A TLS secret holds the
	// private key next to the certificate, so it is read only when asked.
	readSecret bool
	// dial opens the connection of a handshake; nil is the network. Tests
	// point it at a local listener.
	dial dialFunc
	// handshakes remembers the last handshake per host.
	handshakes map[string]handshakeResult
}

// Kind implements probe.Probe.
func (g *ingressProbe) Kind() string { return kindIngress }

// Validate implements probe.Probe.
func (g *ingressProbe) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "namespace", "name"); err != nil {
		return err
	}
	if v, ok := spec[specReadTLSSecret]; ok {
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("%q must be true or false, not %v", specReadTLSSecret, v)
		}
	}
	return nil
}

// Start implements probe.Probe.
func (g *ingressProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := g.Validate(spec); err != nil {
		g.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c, err := g.connect(spec)
	if err != nil {
		return err
	}
	ns, name := probe.Str(spec, "namespace", ""), probe.Str(spec, "name", "")
	target := targetOf(spec, name)
	g.notAfter = map[string]time.Time{}
	g.emitted = map[string]bool{}
	g.handshakes = map[string]handshakeResult{}
	g.readSecret, _ = spec[specReadTLSSecret].(bool)

	f := c.informers().factory
	ingInf := f.Networking().V1().Ingresses().Informer()
	ingresses := f.Networking().V1().Ingresses().Lister()

	kick, notify := kicker()
	watch(ctx, ingInf, func(obj any) bool {
		ing, ok := obj.(*networkingv1.Ingress)
		return ok && ing.Namespace == ns && ing.Name == name
	}, notify)

	go func() {
		if err := g.await(ctx, c, out, target, ingInf); err != nil {
			return
		}
		runLoop(ctx, tickOf(spec), kick, func() {
			o, err := g.observe(ctx, c, ingresses, ns, name, target)
			if err != nil {
				g.fail(ctx, out, target, err)
				return
			}
			g.emit(ctx, out, o)
		})
	}()
	return nil
}

// certInfo is what the probe learned about the certificate of one TLS entry.
// source says where the expiry came from; err says why there is none.
type certInfo struct {
	secret    string
	source    string
	hosts     []string
	subject   string
	issuer    string
	notBefore time.Time
	notAfter  time.Time
	err       error
}

// managedCert is what a cert-manager Certificate says about the secret it
// issues into.
type managedCert struct {
	name        string
	notBefore   time.Time
	notAfter    time.Time
	renewalTime string
}

// observe reads the ingress, cert-manager and the certificates once.
func (g *ingressProbe) observe(ctx context.Context, c *Clients, ingresses networkinglisters.IngressLister, ns, name, target string) (probe.Observation, error) {
	now := time.Now()
	ing, err := ingresses.Ingresses(ns).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return probe.Observation{}, fmt.Errorf("ingress %s/%s not found", ns, name)
		}
		return probe.Observation{}, err
	}
	o := probe.Observation{Target: target, At: now, Metrics: map[string]float64{}, Detail: map[string]any{}}

	hostSet := map[string]bool{}
	var backends []string
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			hostSet[r.Host] = true
		}
		if r.HTTP == nil {
			continue
		}
		for _, p := range r.HTTP.Paths {
			backends = append(backends, backendString(p.Backend, p.Path))
		}
	}
	if ing.Spec.DefaultBackend != nil {
		backends = append(backends, backendString(*ing.Spec.DefaultBackend, "default"))
	}
	for _, t := range ing.Spec.TLS {
		for _, h := range t.Hosts {
			hostSet[h] = true
		}
	}

	// cert-manager first: it knows the expiry and whether the renewal works,
	// and asking it touches no key material.
	certificate := facet.CertificateFacet{WarnDays: certExpiringDays}
	var issuers []string
	managed := map[string]managedCert{}
	if c.Dynamic != nil && len(ing.Spec.TLS) > 0 {
		if certs, err := c.Dynamic.Resource(certificateGVR).Namespace(ns).List(ctx, metav1.ListOptions{}); err == nil {
			for i := range certs.Items {
				cert := &certs.Items[i]
				secret, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
				if !ingressUsesSecret(ing, secret) {
					continue
				}
				kind, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "kind")
				iname, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
				if iname != "" {
					issuers = append(issuers, strings.TrimSpace(firstNonEmpty(kind, "Issuer")+"/"+iname))
				}
				if f := renewalFailure(cert); f != nil && certificate.RenewalFailed == nil {
					certificate.RenewalFailed = f
				}
				if _, seen := managed[secret]; !seen {
					managed[secret] = managedCertOf(cert)
				}
			}
		} else if !apierrors.IsNotFound(err) && !isNoMatch(err) {
			o.Detail["cert_manager_error"] = err.Error()
		}
	}

	// Certificates. The facet carries one certificate: the one that expires
	// first among the TLS entries of the ingress.
	var infos []certInfo
	var notes []string
	source := ""
	tlsDetail := map[string]any{}
	for _, t := range ing.Spec.TLS {
		if t.SecretName == "" {
			continue
		}
		mc, isManaged := managed[t.SecretName]
		ci := g.expiry(ctx, c, ns, t, mc, isManaged, now)
		ci.hosts = t.Hosts
		infos = append(infos, ci)
		d := map[string]any{"hosts": t.Hosts}
		if mc.renewalTime != "" {
			d["renewal_time"] = mc.renewalTime
		}
		if ci.err != nil {
			d["note"] = ci.err.Error()
			notes = append(notes, ci.err.Error())
			tlsDetail[t.SecretName] = d
			continue
		}
		days := ci.notAfter.Sub(now).Hours() / 24
		if !certificate.Known || ci.notAfter.Before(certificate.NotAfter) {
			certificate.Known, certificate.NotAfter = true, ci.notAfter
			source = ci.source
		}
		d["source"] = ci.source
		d["not_after"] = ci.notAfter
		d["days"] = round1(days)
		if !ci.notBefore.IsZero() {
			d["not_before"] = ci.notBefore
		}
		if ci.subject != "" {
			d["subject"] = ci.subject
		}
		if ci.issuer != "" {
			d["issuer"] = ci.issuer
		}
		tlsDetail[t.SecretName] = d
		// Renewal marker: notAfter moved while watching, or the certificate
		// was issued within the lookback on the first sighting.
		prev, seen := g.notAfter[t.SecretName]
		switch {
		case seen && !prev.Equal(ci.notAfter):
			g.addEvent(&o, model.Event{At: now, Kind: "cert", Target: target, Summary: "cert renewal of " + target, Ref: t.SecretName})
		case !seen && within(ci.notBefore, lookback, now):
			g.addEvent(&o, model.Event{At: ci.notBefore, Kind: "cert", Target: target, Summary: "cert renewal of " + target, Ref: t.SecretName})
		}
		g.notAfter[t.SecretName] = ci.notAfter
	}
	switch {
	case certificate.Known:
		o.Detail["cert_source"] = source
	case len(notes) > 0:
		// No expiry from any source: cert_days stays out of the observation,
		// and the panel says why instead of showing a number nobody measured.
		o.Detail["cert_note"] = "certificate expiry unknown: " + strings.Join(notes, "; ")
	}
	if len(issuers) == 0 {
		for _, ci := range infos {
			if ci.err == nil && ci.issuer != "" {
				issuers = append(issuers, ci.issuer)
			}
		}
	}

	hosts := sortedKeys(hostSet)
	o.Detail["hosts"] = hosts
	o.Detail["backends"] = backends
	if len(issuers) > 0 {
		o.Detail["issuer"] = strings.Join(uniqueStrings(issuers), ", ")
	}
	if len(tlsDetail) > 0 {
		o.Detail["tls"] = tlsDetail
	}
	if ing.Spec.IngressClassName != nil {
		o.Detail["class"] = *ing.Spec.IngressClassName
	}
	var addrs []string
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		addrs = append(addrs, firstNonEmpty(lb.IP, lb.Hostname))
	}
	if len(addrs) > 0 {
		o.Detail["addresses"] = addrs
	}
	// The facet writes cert_days and the certificate conditions.
	facet.EmitIngress(&o, facet.IngressFacet{Hosts: hosts, Certificate: certificate}, now)
	return o, nil
}

// addEvent appends an event once per (summary, ref, minute).
func (g *ingressProbe) addEvent(o *probe.Observation, e model.Event) {
	key := e.Summary + "|" + e.Ref + "|" + e.At.Truncate(time.Minute).Format(time.RFC3339)
	if g.emitted[key] {
		return
	}
	g.emitted[key] = true
	o.Events = append(o.Events, e)
}

// backendString renders an ingress backend as "service:port path".
func backendString(b networkingv1.IngressBackend, path string) string {
	var s string
	switch {
	case b.Service != nil:
		port := b.Service.Port.Name
		if b.Service.Port.Number != 0 {
			port = fmt.Sprint(b.Service.Port.Number)
		}
		s = b.Service.Name
		if port != "" {
			s += ":" + port
		}
	case b.Resource != nil:
		s = b.Resource.Kind + "/" + b.Resource.Name
	}
	if path != "" && path != "/" {
		s += " " + path
	}
	return s
}

// ingressUsesSecret reports whether a TLS entry names the secret.
func ingressUsesSecret(ing *networkingv1.Ingress, secret string) bool {
	if secret == "" {
		return false
	}
	for _, t := range ing.Spec.TLS {
		if t.SecretName == secret {
			return true
		}
	}
	return false
}

// expiry learns the certificate of one TLS entry from the source that costs
// the least access: cert-manager's Certificate, then a handshake with the
// entry's hosts, and the TLS secret only when the binding allows it. When no
// source answers, the returned err lists what was tried.
func (g *ingressProbe) expiry(ctx context.Context, c *Clients, ns string, t networkingv1.IngressTLS, mc managedCert, isManaged bool, now time.Time) certInfo {
	var tried []string
	switch {
	case isManaged && !mc.notAfter.IsZero():
		return certInfo{secret: t.SecretName, source: sourceCertManager, notBefore: mc.notBefore, notAfter: mc.notAfter}
	case isManaged:
		tried = append(tried, fmt.Sprintf("cert-manager certificate %s has not issued yet", mc.name))
	default:
		tried = append(tried, "no cert-manager certificate for secret "+t.SecretName)
	}

	dialed := false
	for _, host := range t.Hosts {
		// A wildcard names no host to connect to.
		if host == "" || strings.Contains(host, "*") {
			continue
		}
		dialed = true
		leaf, err := g.handshake(ctx, host, now)
		if err != nil {
			tried = append(tried, err.Error())
			continue
		}
		ci := describeLeaf(leaf)
		ci.secret, ci.source = t.SecretName, sourceHandshake
		return ci
	}
	if !dialed {
		tried = append(tried, "the ingress names no host to handshake with for secret "+t.SecretName)
	}

	if !g.readSecret {
		tried = append(tried, fmt.Sprintf("secret %s is not read (set %s: true to allow it)", t.SecretName, specReadTLSSecret))
		return certInfo{secret: t.SecretName, err: fmt.Errorf("%s", strings.Join(tried, "; "))}
	}
	ci := readTLSSecret(ctx, c, ns, t.SecretName)
	if ci.err != nil {
		tried = append(tried, fmt.Sprintf("secret %s: %v", t.SecretName, ci.err))
		ci.err = fmt.Errorf("%s", strings.Join(tried, "; "))
	}
	return ci
}

// handshake returns the leaf certificate host presents, from memory while
// the last answer is fresh.
func (g *ingressProbe) handshake(ctx context.Context, host string, now time.Time) (*x509.Certificate, error) {
	if h, ok := g.handshakes[host]; ok {
		keep := handshakeEvery
		if h.err != nil {
			keep = handshakeRetry
		}
		if now.Sub(h.at) < keep {
			return h.leaf, h.err
		}
	}
	dial := g.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	leaf, err := leafByHandshake(ctx, dial, host)
	if ctx.Err() != nil {
		// The probe is stopping; that says nothing about the host.
		return leaf, err
	}
	g.handshakes[host] = handshakeResult{at: now, leaf: leaf, err: err}
	return leaf, err
}

// leafByHandshake connects to host on the TLS port, names host in the
// handshake and returns the first certificate of the chain presented.
func leafByHandshake(ctx context.Context, dial dialFunc, host string) (*x509.Certificate, error) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	addr := net.JoinHostPort(host, tlsPort)
	raw, err := dial(hctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("handshake with %s: %w", addr, err)
	}
	defer raw.Close()
	// Verification stays on. An expired or self-signed certificate is
	// exactly the one whose date must be reported, and the handshake that
	// refuses it hands over the chain it refused. Nothing is sent over the
	// connection and nothing read from it is trusted.
	conn := tls.Client(raw, &tls.Config{ServerName: host})
	chain, err := presentedChain(hctx, conn)
	if err != nil {
		return nil, fmt.Errorf("handshake with %s: %w", addr, err)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("handshake with %s: no certificate presented", addr)
	}
	return chain[0], nil
}

// presentedChain completes the handshake and returns the certificates the
// server presented, whether or not they passed verification.
func presentedChain(ctx context.Context, conn *tls.Conn) ([]*x509.Certificate, error) {
	err := conn.HandshakeContext(ctx)
	var refused *tls.CertificateVerificationError
	if errors.As(err, &refused) {
		return refused.UnverifiedCertificates, nil
	}
	if err != nil {
		return nil, err
	}
	return conn.ConnectionState().PeerCertificates, nil
}

// describeLeaf copies what the panel shows of a certificate.
func describeLeaf(leaf *x509.Certificate) certInfo {
	ci := certInfo{notBefore: leaf.NotBefore, notAfter: leaf.NotAfter}
	ci.subject = leaf.Subject.CommonName
	if ci.subject == "" && len(leaf.DNSNames) > 0 {
		ci.subject = leaf.DNSNames[0]
	}
	ci.issuer = firstNonEmpty(leaf.Issuer.CommonName, strings.Join(leaf.Issuer.Organization, ","))
	return ci
}

// managedCertOf reads the dates cert-manager keeps in a Certificate's status.
func managedCertOf(cert *unstructured.Unstructured) managedCert {
	mc := managedCert{name: cert.GetName()}
	if v, ok, _ := unstructured.NestedString(cert.Object, "status", "notAfter"); ok {
		mc.notAfter, _ = time.Parse(time.RFC3339, v)
	}
	if v, ok, _ := unstructured.NestedString(cert.Object, "status", "notBefore"); ok {
		mc.notBefore, _ = time.Parse(time.RFC3339, v)
	}
	mc.renewalTime, _, _ = unstructured.NestedString(cert.Object, "status", "renewalTime")
	return mc
}

// renewalFailure returns the failure a Certificate reports through a Ready
// condition that is not true, or nil.
func renewalFailure(cert *unstructured.Unstructured) *facet.Failure {
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, raw := range conds {
		cm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := cm["type"].(string)
		status, _ := cm["status"].(string)
		if typ != "Ready" || status == "True" {
			continue
		}
		msg, _ := cm["message"].(string)
		reason, _ := cm["reason"].(string)
		since := time.Time{}
		if lt, ok := cm["lastTransitionTime"].(string); ok {
			since, _ = time.Parse(time.RFC3339, lt)
		}
		return &facet.Failure{Ref: "certificate/" + cert.GetName(), At: since, Reason: firstNonEmpty(msg, reason)}
	}
	return nil
}

// readTLSSecret fetches a TLS secret and parses its leaf certificate. It is
// the last resort and runs only with read_tls_secret: the secret holds the
// private key, and the API server has no way to hand out the certificate
// without it.
func readTLSSecret(ctx context.Context, c *Clients, ns, name string) certInfo {
	ci := certInfo{secret: name, source: sourceSecret}
	if c.Core == nil {
		ci.err = fmt.Errorf("no core client")
		return ci
	}
	sec, err := c.Core.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		ci.err = err
		return ci
	}
	leaf, err := leafCertificate(sec.Data[corev1.TLSCertKey])
	if err != nil {
		ci.err = err
		return ci
	}
	described := describeLeaf(leaf)
	described.secret, described.source = name, sourceSecret
	return described
}

// leafCertificate parses a PEM chain and returns the certificate that
// expires first, which for a well-formed chain is the leaf.
func leafCertificate(pemData []byte) (*x509.Certificate, error) {
	if len(pemData) == 0 {
		return nil, fmt.Errorf("secret has no tls.crt")
	}
	var leaf *x509.Certificate
	rest := pemData
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		if leaf == nil || cert.NotAfter.Before(leaf.NotAfter) {
			leaf = cert
		}
	}
	if leaf == nil {
		return nil, fmt.Errorf("tls.crt holds no certificate")
	}
	return leaf, nil
}

// isNoMatch reports whether an error says the resource type does not exist.
func isNoMatch(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no matches for kind") || strings.Contains(msg, "the server could not find the requested resource") || strings.Contains(msg, "could not find the requested resource")
}

// uniqueStrings sorts and dedupes.
func uniqueStrings(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		if s != "" {
			set[s] = true
		}
	}
	out := sortedKeys(set)
	sort.Strings(out)
	return out
}
