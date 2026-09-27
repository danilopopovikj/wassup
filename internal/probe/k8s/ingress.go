package k8s

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	networkinglisters "k8s.io/client-go/listers/networking/v1"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

const kindIngress = "k8s.ingress"

// certExpiringDays is the warning horizon for CertExpiring.
const certExpiringDays = 14

// certificateGVR is the cert-manager Certificate resource.
var certificateGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

func init() {
	probe.Register(probe.Access{
		Kind:        kindIngress,
		Source:      "Kubernetes API (ingress informer), the TLS secrets it names and, when installed, cert-manager Certificates",
		Delivers:    "cert_days; CertExpired, CertExpiring, CertRenewalFailed; cert (renewal) events",
		SpecFields:  []string{"namespace (required)", "name (required)", "kubeconfig", "context"},
		Needs:       "get/list/watch on ingresses; get on the TLS secrets in the namespace; list on certificates.cert-manager.io (optional)",
		Implemented: true,
	}, func() probe.Probe { return &ingressProbe{base: base{kind: kindIngress}} })
}

// ingressProbe is k8s.ingress.
type ingressProbe struct {
	base
	notAfter map[string]time.Time
	emitted  map[string]bool
}

// Kind implements probe.Probe.
func (g *ingressProbe) Kind() string { return kindIngress }

// Validate implements probe.Probe.
func (g *ingressProbe) Validate(spec map[string]any) error {
	return probe.RequireString(spec, "namespace", "name")
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

	f := c.informers().factory
	ingInf := f.Networking().V1().Ingresses().Informer()
	ingresses := f.Networking().V1().Ingresses().Lister()

	kick, notify := kicker()
	watch(ctx, ingInf, func(obj any) bool {
		ing, ok := obj.(*networkingv1.Ingress)
		return ok && ing.Namespace == ns && ing.Name == name
	}, notify)

	go func() {
		if err := g.syncOrFail(ctx, c, ingInf); err != nil {
			probe.Send(ctx, out, probe.Observation{Target: target, Probe: kindIngress, At: time.Now(), Err: err.Error()})
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

// certInfo is what the probe learned about one TLS secret.
type certInfo struct {
	secret    string
	hosts     []string
	subject   string
	issuer    string
	notBefore time.Time
	notAfter  time.Time
	err       error
}

// observe reads the ingress, its TLS secrets and cert-manager once.
func (g *ingressProbe) observe(ctx context.Context, c *Clients, ingresses networkinglisters.IngressLister, ns, name, target string) (probe.Observation, error) {
	now := time.Now()
	ing, err := ingresses.Ingresses(ns).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
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

	// Certificates.
	minDays := math.Inf(1)
	var infos []certInfo
	tlsDetail := map[string]any{}
	for _, t := range ing.Spec.TLS {
		if t.SecretName == "" {
			continue
		}
		ci := readTLSSecret(ctx, c, ns, t.SecretName)
		ci.hosts = t.Hosts
		infos = append(infos, ci)
		d := map[string]any{"hosts": t.Hosts}
		if ci.err != nil {
			d["error"] = ci.err.Error()
			tlsDetail[t.SecretName] = d
			continue
		}
		days := ci.notAfter.Sub(now).Hours() / 24
		minDays = math.Min(minDays, days)
		d["not_after"] = ci.notAfter
		d["not_before"] = ci.notBefore
		d["subject"] = ci.subject
		d["issuer"] = ci.issuer
		d["days"] = round1(days)
		tlsDetail[t.SecretName] = d
		ref := "secret/" + t.SecretName
		which := firstNonEmpty(strings.Join(t.Hosts, ","), ci.subject)
		switch {
		case days <= 0:
			o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondCertExpired, Ref: ref, Since: ci.notAfter, Detail: fmt.Sprintf("%s expired %s", which, ci.notAfter.Format(time.RFC3339))})
		case days <= certExpiringDays:
			o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondCertExpiring, Ref: ref, Since: ci.notAfter.Add(-certExpiringDays * 24 * time.Hour), Detail: fmt.Sprintf("%s expires %s", which, ci.notAfter.Format(time.RFC3339))})
		}
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
	if !math.IsInf(minDays, 1) {
		o.Metrics["cert_days"] = round1(minDays)
	}

	// cert-manager, when present.
	var issuers []string
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
					o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondCertRenewalFailed, Ref: "certificate/" + cert.GetName(), Since: since, Detail: firstNonEmpty(msg, reason)})
				}
				if ra, ok, _ := unstructured.NestedString(cert.Object, "status", "renewalTime"); ok {
					if d, ok := tlsDetail[secret].(map[string]any); ok {
						d["renewal_time"] = ra
					}
				}
			}
		} else if !errors.IsNotFound(err) && !isNoMatch(err) {
			o.Detail["cert_manager_error"] = err.Error()
		}
	}
	if len(issuers) == 0 {
		for _, ci := range infos {
			if ci.err == nil && ci.issuer != "" {
				issuers = append(issuers, ci.issuer)
			}
		}
	}

	o.Detail["hosts"] = sortedKeys(hostSet)
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

// readTLSSecret fetches a TLS secret and parses its leaf certificate.
func readTLSSecret(ctx context.Context, c *Clients, ns, name string) certInfo {
	ci := certInfo{secret: name}
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
	ci.subject = leaf.Subject.CommonName
	if ci.subject == "" && len(leaf.DNSNames) > 0 {
		ci.subject = leaf.DNSNames[0]
	}
	ci.issuer = firstNonEmpty(leaf.Issuer.CommonName, strings.Join(leaf.Issuer.Organization, ","))
	ci.notBefore, ci.notAfter = leaf.NotBefore, leaf.NotAfter
	return ci
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
