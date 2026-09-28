package netprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// KindCert is the probe kind of the TLS certificate check.
const KindCert = "cert.tls"

// certWarnDays is the number of days left at which CertExpiring is raised.
// It matches model.DefaultThresholds.CertWarnDays; the engine still applies
// the user's own cert_warn_days to the cert_days metric.
const certWarnDays = 14

func init() {
	probe.Register(probe.Access{
		Kind:        KindCert,
		Source:      "a TLS handshake with the endpoint",
		Delivers:    "cert_days until the leaf certificate expires, CertExpiring and CertExpired, subject, issuer and SANs",
		SpecFields:  []string{"addr", "servername", "interval"},
		Needs:       "TCP access to host:port; no credentials",
		Implemented: true,
		Facets:      []string{facet.NameCertificate},
	}, func() probe.Probe { return &Cert{} })
}

// Cert is the cert.tls probe. It performs a TLS handshake against addr and
// reads the leaf certificate of the presented chain.
type Cert struct {
	h probe.Health

	// now and roots are overridable for tests; roots nil means system roots.
	now   func() time.Time
	roots *x509.CertPool
}

// Kind implements probe.Probe.
func (c *Cert) Kind() string { return KindCert }

// Validate implements probe.Probe.
func (c *Cert) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "addr"); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(probe.Str(spec, "addr", ""))
	if err != nil {
		return fmt.Errorf("addr must be host:port: %w", err)
	}
	if host == "" || port == "" {
		return fmt.Errorf("addr must be host:port")
	}
	return nil
}

// Health implements probe.Probe.
func (c *Cert) Health() probe.ProbeHealth { return c.h.Get() }

// clock returns the current time from the test hook or the wall clock.
func (c *Cert) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Start implements probe.Probe.
func (c *Cert) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := c.Validate(spec); err != nil {
		c.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	target := targetOf(spec)
	addr := probe.Str(spec, "addr", "")
	host, _, _ := net.SplitHostPort(addr)
	servername := probe.Str(spec, "servername", host)
	tick := tickOf(spec)
	interval := probe.Dur(spec, "interval", 5*time.Minute)
	c.h.Set(probe.HealthOK, "checking "+addr)
	go loop(ctx, tick, interval, out, func(ctx context.Context) probe.Observation {
		return c.check(ctx, target, addr, servername)
	})
	return nil
}

// check dials once and reports on the leaf certificate.
func (c *Cert) check(ctx context.Context, target, addr, servername string) probe.Observation {
	o := probe.Observation{
		Target: target,
		Probe:  KindCert,
		At:     c.clock(),
		Detail: map[string]any{"addr": addr, "servername": servername},
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// InsecureSkipVerify is deliberate: the point of this probe is to read
	// the chain a server presents even when it is expired, self-signed or
	// issued for another name. With verification on, exactly the broken
	// certificates we want to report would abort the handshake before we
	// could see them. Nothing is sent over the connection, and the chain is
	// verified separately below so the result is still reported.
	dialer := &tls.Dialer{Config: &tls.Config{
		ServerName:         servername,
		InsecureSkipVerify: true, //nolint:gosec // see comment above
	}}
	conn, err := dialer.DialContext(dctx, "tcp", addr)
	if err != nil {
		o.Err = fmt.Sprintf("tls dial %s: %v", addr, err)
		c.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	state := conn.(*tls.Conn).ConnectionState()
	conn.Close()
	if len(state.PeerCertificates) == 0 {
		o.Err = fmt.Sprintf("tls dial %s: no certificate presented", addr)
		c.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	leaf := state.PeerCertificates[0]
	c.describe(&o, leaf, state.PeerCertificates[1:], servername)
	c.h.Set(probe.HealthOK, fmt.Sprintf("%s expires %s", servername, leaf.NotAfter.Format(time.RFC3339)))
	return o
}

// describe fills o's detail, metrics and conditions for a leaf.
func (c *Cert) describe(o *probe.Observation, leaf *x509.Certificate, intermediates []*x509.Certificate, servername string) {
	now := c.clock()
	detail := o.Detail
	detail["subject"] = leaf.Subject.String()
	detail["issuer"] = leaf.Issuer.String()
	detail["not_after"] = leaf.NotAfter.UTC().Format(time.RFC3339)
	detail["not_before"] = leaf.NotBefore.UTC().Format(time.RFC3339)
	detail["sans"] = leaf.DNSNames
	detail["serial"] = leaf.SerialNumber.String()

	// Verify the chain the ordinary way and record the verdict; the metric
	// and conditions above are about expiry and do not depend on it.
	pool := x509.NewCertPool()
	for _, ic := range intermediates {
		pool.AddCert(ic)
	}
	_, verr := leaf.Verify(x509.VerifyOptions{
		DNSName:       servername,
		Intermediates: pool,
		Roots:         c.roots,
		CurrentTime:   now,
	})
	detail["verified"] = verr == nil
	if verr != nil {
		detail["verify_error"] = verr.Error()
	}

	// The facet derives cert_days, CertExpired and CertExpiring the same
	// way for every certificate source.
	facet.EmitCertificate(o, facet.CertificateFacet{Known: true, NotAfter: leaf.NotAfter, WarnDays: certWarnDays}, now)
}
