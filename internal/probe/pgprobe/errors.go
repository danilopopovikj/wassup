package pgprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// explained is a failed read worded for a person: what happened and what to
// check next. The error of the driver stays with it, at the end of the
// message and for errors.Is and errors.As.
type explained struct {
	say   string
	cause error
}

// Error implements error.
func (e *explained) Error() string { return e.say + " (" + oneLine(e.cause) + ")" }

// Unwrap returns the error of the driver.
func (e *explained) Unwrap() error { return e.cause }

// oneLine renders an error on one line. The driver reports every address it
// tried on a line of its own; the same reason is said once.
func oneLine(err error) string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(err.Error(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return strings.Join(out, "; ")
}

// viaHint is how a database inside the cluster is reached.
const viaHint = "via: k8s.service/<namespace>/<service>:<port>"

// isLoopback reports whether host is this machine.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// isRefused reports whether a connection was refused.
func isRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(err.Error()), "connection refused")
}

// isTimeout reports whether an error is a time limit that passed.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isCertificate reports whether the server's certificate was not accepted.
func isCertificate(err error) bool {
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

// address is the host and port the settings name.
func (c *connector) address() string {
	return net.JoinHostPort(c.cfg.Host, strconv.Itoa(int(c.cfg.Port)))
}

// explain words a failed round for a person. connected says whether the
// connection was open when the round failed: a time limit that passes while
// connecting is a server that cannot be reached, one that passes afterwards
// is a server that is slow. An error nothing is known about comes back as it
// is.
func (c *connector) explain(err error, connected bool) error {
	var done *explained
	if err == nil || c.cfg == nil || errors.As(err, &done) || errors.Is(err, probe.ErrReadOnly) {
		return err
	}
	say := func(format string, args ...any) error {
		return &explained{say: fmt.Sprintf(format, args...), cause: err}
	}
	user, addr := c.cfg.User, c.address()

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		msg := strings.ToLower(pgErr.Message)
		switch {
		case pgErr.Code == "28P01":
			return say("authentication failed for user %q: check %s and the user name", user, c.src.password())
		case pgErr.Code == "28000" && (strings.Contains(msg, "no encryption") || strings.Contains(msg, "ssl off")):
			return say("the server only takes encrypted connections: set sslmode to require")
		case pgErr.Code == "28000":
			return say("the server does not let user %q in from this address: check the user, the database and the server's pg_hba.conf", user)
		case pgErr.Code == "3D000":
			return say("database %q does not exist on this server: check the database name", c.cfg.Database)
		case pgErr.Code == "42501":
			return say("role %q is not allowed to read this: %s", user, pgErr.Message)
		case pgErr.Code == "53300":
			return say("the server takes no more connections, every one of them is in use: wassup could not read it")
		case pgErr.Code == "57P03":
			return say("the server is starting or stopping and takes no connections yet: the next round tries again")
		}
		return err
	}

	var dns *net.DNSError
	switch {
	case strings.Contains(err.Error(), "server refused TLS connection"):
		return say("the server does not speak TLS: set sslmode to disable or prefer")
	case isCertificate(err):
		return say("the certificate of %s was not accepted: with sslmode verify-ca or verify-full the DSN names the authority that signed it (sslrootcert), and the host is the name in the certificate", c.cfg.Host)
	case errors.As(err, &dns):
		return say("the name %s does not resolve on this machine: a name of the cluster only resolves inside it. Set %s, or a host this machine reaches", dns.Name, viaHint)
	case isRefused(err) && c.via != "" && c.tunnel != nil:
		return say("connection refused through %s: the port-forward is open, but nothing listens on that port of the pod. Check the port in via", c.via)
	case isRefused(err) && isLoopback(c.cfg.Host):
		return say("connection refused on %s: nothing listens there. A database inside the cluster is reached with %s: wassup then opens the port-forward itself, and none has to be kept running by hand", addr, viaHint)
	case isRefused(err):
		return say("connection refused on %s: nothing listens there, or a firewall turns the connection down. Check host and port", addr)
	case isTimeout(err) && !connected:
		return say("no answer from %s within %s: check that this machine reaches it (VPN, firewall), or set %s for a database inside the cluster", addr, roundTimeout, viaHint)
	case isTimeout(err):
		return say("the server did not answer within %s: it is reached but slow, or the statement waits behind a lock", roundTimeout)
	}
	return err
}

// readAllStats is the role whose members see the statistics of every
// session.
const readAllStats = "pg_read_all_stats"

// quoteIdent writes a role name as an SQL identifier.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// grantHint says how the role of the connection gets to see what it may not
// see now: the statement, and the same for a role CloudNativePG manages.
// what names what is hidden. An unknown role is written as a placeholder.
func grantHint(role, what string) (note string, grant map[string]any) {
	sqlRole, yamlRole := "<role>", "<role>"
	if role != "" {
		sqlRole, yamlRole = quoteIdent(role), role
	}
	sql := "GRANT " + readAllStats + " TO " + sqlRole + ";"
	cnpg := "spec:\n" +
		"  managed:\n" +
		"    roles:\n" +
		"      - name: " + yamlRole + "\n" +
		"        ensure: present\n" +
		"        login: true\n" +
		"        inRoles:\n" +
		"          - " + readAllStats + "\n"
	note = "role " + sqlRole + " lacks " + readAllStats + ", " + what + ". To grant it, a superuser runs: " + sql +
		" For a role CloudNativePG manages, add " + readAllStats + " to its inRoles under spec.managed.roles of the Cluster (the snippet is in the detail)"
	grant = map[string]any{
		"privilege":     readAllStats,
		"sql":           sql,
		"cloudnativepg": cnpg,
		"cloudnativepg_note": "an entry under spec.managed.roles describes the whole role: keep login and every other attribute as the role has them today, " +
			"an attribute left out goes back to its default",
	}
	return note, grant
}

// hiddenFrom names what a role without pg_read_all_stats does not see.
func hiddenFrom(replication, activity bool) string {
	switch {
	case replication && activity:
		return "replication detail and what other sessions do are hidden"
	case replication:
		return "replication detail is hidden"
	}
	return "what other sessions do is hidden"
}

// addGrant puts the way to the missing privilege in the detail, when
// something was hidden from the role.
func addGrant(detail map[string]any, role string, replication, activity bool) {
	if !replication && !activity {
		return
	}
	detail["grant_note"], detail["grant"] = grantHint(role, hiddenFrom(replication, activity))
}
