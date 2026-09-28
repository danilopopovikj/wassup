package redisprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/redis/go-redis/v9"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// server is what the messages name of a connection: where it goes and where
// its password comes from.
type server struct {
	addr        string
	user        string
	passwordEnv string
	tls         bool
	// via is the tunnel the connection goes through, "" when it goes
	// straight to addr.
	via string
}

// serverOf describes the connection of a spec for the messages.
func serverOf(opt *redis.Options, spec map[string]any) server {
	return server{addr: opt.Addr, user: opt.Username, passwordEnv: probe.Str(spec, "password_env", ""), tls: opt.TLSConfig != nil,
		via: probe.NewVia(spec).String()}
}

// isBroken reports whether the connection ended instead of answering.
func isBroken(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// password names where the password comes from.
func (s server) password() string {
	if s.passwordEnv != "" {
		return "the password in " + s.passwordEnv
	}
	return "the password"
}

// isLoopback reports whether the address is on this machine.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isCertificate reports whether the server's certificate was not accepted.
func isCertificate(err error) bool {
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

// explain words a failed command for a person: what happened and what to
// check next, with the command and the error of the driver at the end so
// nothing is lost. An error nothing is known about is the command and the
// error, as before.
func (s server) explain(command string, err error) string {
	raw := command + ": " + err.Error()
	if errors.Is(err, probe.ErrReadOnly) || errors.Is(err, probe.ErrNoTunnel) {
		return raw
	}
	say := func(format string, args ...any) string {
		return fmt.Sprintf(format, args...) + " (" + strings.Join(strings.Fields(raw), " ") + ")"
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	var dns *net.DNSError
	var ne net.Error
	switch {
	case strings.HasPrefix(msg, "WRONGPASS"):
		if s.user != "" {
			return say("authentication failed for user %q: check %s and the user name", s.user, s.password())
		}
		return say("authentication failed: check %s", s.password())
	case strings.HasPrefix(msg, "NOAUTH"):
		return say("the server asks for a password and none was sent: set password_env to the variable that holds it")
	case strings.Contains(lower, "without any password configured"):
		return say("the server has no password and one was sent: take password_env out of the binding")
	case strings.HasPrefix(msg, "NOPERM"):
		return say("user %q is not allowed to run %s: its ACL needs +info, +llen and +lindex on the keys wassup reads", s.user, strings.ToUpper(strings.Fields(command)[0]))
	case strings.HasPrefix(msg, "LOADING"):
		return say("the server is loading its data and answers nothing yet: the next round tries again")
	case strings.Contains(lower, "first record does not look like a tls handshake"):
		return say("the server at %s does not speak TLS: set tls to false, or use redis:// instead of rediss://", s.addr)
	case s.via != "" && isBroken(err):
		// What is dialled is the local end of the tunnel: the address of
		// the server and a firewall have nothing to do with it.
		hint := ""
		if !s.tls && !errors.Is(err, syscall.ECONNREFUSED) {
			hint = ". If it goes on, the server may only take TLS: set tls to true"
		}
		return say("the connection through %s did not hold, the port-forward may have ended: the next round opens a new one%s", s.via, hint)
	case isCertificate(err):
		return say("the certificate of %s was not accepted: the address has to be the name in the certificate, signed by an authority this machine trusts", s.addr)
	case errors.As(err, &dns):
		return say("the name %s does not resolve on this machine: a name of the cluster only resolves inside it. Use an address this machine reaches", dns.Name)
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(lower, "connection refused"):
		if isLoopback(s.addr) {
			return say("connection refused on %s: nothing listens there. Is the port-forward to the server still running?", s.addr)
		}
		return say("connection refused on %s: nothing listens there, or a firewall turns the connection down. Check the address and the port", s.addr)
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return say("no answer from %s within %s: check that this machine reaches it (VPN, firewall)", s.addr, roundTimeout)
	case (errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) && !s.tls:
		return say("%s closed the connection before it answered: a server that only takes TLS does that to a plain connection. If it does, set tls to true", s.addr)
	}
	return raw
}
