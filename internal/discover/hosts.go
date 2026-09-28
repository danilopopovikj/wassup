package discover

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/danilopopovikj/wassup/internal/discover/redact"
	"github.com/danilopopovikj/wassup/internal/model"
)

// dsnRe finds connection strings in text: postgres://user:pw@host:5432/db,
// redis://host:6379/0, amqp://..., http(s)://host/...
var dsnRe = regexp.MustCompile(`(?i)\b(postgres(?:ql)?|redis|rediss|amqps?|https?|grpcs?|mongodb(?:\+srv)?|mysql|nats|kafka)://[^\s"'` + "`" + `<>\\)]+`)

// hostRef is a parsed connection target.
type hostRef struct {
	Scheme string
	Host   string // hostname only
	Port   string
	Raw    string
}

// parseDSN reads the host of a connection string. The string is reduced to
// scheme and host first, so that a password with an odd character neither
// breaks the parsing nor travels any further.
func parseDSN(raw string) (hostRef, bool) {
	raw = redact.URLHost(strings.TrimRight(raw, ".,;"))
	if raw == "" {
		return hostRef{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return hostRef{}, false
	}
	h := u.Hostname()
	if h == "" {
		return hostRef{}, false
	}
	return hostRef{Scheme: strings.ToLower(u.Scheme), Host: strings.ToLower(h), Port: u.Port(), Raw: raw}, true
}

// edgeKindFor maps a scheme (and the destination type) to an edge kind.
func edgeKindFor(scheme, dstType string) string {
	switch scheme {
	case "postgres", "postgresql", "mysql", "mongodb", "mongodb+srv":
		return "sql"
	case "redis", "rediss":
		if dstType == "queue" {
			return "queue"
		}
		return "cache"
	case "amqp", "amqps", "nats", "kafka":
		return "queue"
	case "grpc", "grpcs":
		return "grpc"
	case "http", "https":
		if dstType == "external" {
			return "external"
		}
		return "http"
	}
	return "tcp"
}

// hostNameRe is what a host may look like once it is resolved.
var hostNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// hasPlaceholder reports whether a text still holds a template expression
// (${DOMAIN}, $(VAR), {{ .Values.host }}, <your-host>) that something else
// fills in at deploy time. Such a text names nothing.
func hasPlaceholder(s string) bool {
	return strings.Contains(s, "${") || strings.Contains(s, "$(") || strings.Contains(s, "{{") || strings.Contains(s, "}}") ||
		strings.ContainsAny(s, "<>") || strings.Contains(s, redact.Placeholder)
}

// tunnelDomains are services that expose a developer's machine. A host
// under one of them is somebody's laptop, not a part of the system.
var tunnelDomains = []string{"ngrok.io", "ngrok.app", "ngrok.dev", "ngrok-free.app", "ngrok-free.dev", "ngrok.com",
	"localhost.run", "lhr.life", "lhr.rocks", "loca.lt", "localtunnel.me", "trycloudflare.com", "serveo.net", "pagekite.me",
	"telebit.cloud", "tunnelmole.net", "localto.net", "devtunnels.ms", "nip.io", "sslip.io"}

// isDevHost reports whether a host is a developer's machine or a tunnel to
// one: localhost in its forms, the reserved test domains, and the tunnels.
func isDevHost(h string) bool {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	switch h {
	case "localhost", "0.0.0.0", "::1", "host.docker.internal", "host.minikube.internal", "kubernetes.docker.internal":
		return true
	}
	if strings.HasPrefix(h, "127.") {
		return true
	}
	for _, tld := range []string{".localhost", ".test", ".invalid"} {
		if strings.HasSuffix(h, tld) {
			return true
		}
	}
	for _, d := range tunnelDomains {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// isReservedHost reports whether a host is under a domain reserved for
// documentation (example.com, example.net, example.org): it appears in
// samples and comments and is never a dependency.
func isReservedHost(h string) bool {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	for _, d := range []string{"example.com", "example.net", "example.org"} {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// usableHost reports whether a host read from a file can name a component:
// it is resolved, shaped like a host, and neither a developer's machine nor
// a documentation domain.
func usableHost(h string) bool {
	h = strings.ToLower(h)
	return h != "" && !hasPlaceholder(h) && hostNameRe.MatchString(h) && !isDevHost(h) && !isReservedHost(h)
}

// isInternalHost guesses whether a host is inside the cluster or network.
func isInternalHost(h string) bool {
	if h == "localhost" || h == "127.0.0.1" || strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".svc.cluster.local") || strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".local") {
		return true
	}
	if !strings.Contains(h, ".") {
		return true // a bare service name
	}
	parts := strings.Split(h, ".")
	if len(parts) == 2 && !knownTLD[parts[1]] {
		return true // name.namespace
	}
	// private IPs
	for _, p := range []string{"10.", "192.168.", "172.16.", "172.17.", "172.18.", "172.19.", "172.2", "172.30.", "172.31."} {
		if strings.HasPrefix(h, p) {
			return true
		}
	}
	return false
}

var knownTLD = map[string]bool{"com": true, "io": true, "net": true, "org": true, "dev": true, "app": true, "ai": true, "co": true, "cloud": true, "run": true, "sh": true, "me": true, "us": true, "eu": true, "de": true, "uk": true, "fr": true, "nl": true, "mk": true}

// resolveLinks turns hosts into candidate ids using each candidate's
// addresses; unknown public hosts become external candidates. A link marked
// Internal names a Service of the cluster: when nothing answers to it, it is
// listed as unresolved and never becomes an external.
func resolveLinks(f *Findings) {
	addr := map[string]string{} // address -> candidate id
	namespaces := map[string]bool{}
	// A Service nothing was found for must not take an address away from
	// the component that answers to it, so those go first and are
	// overwritten.
	for _, custom := range []bool{true, false} {
		for _, c := range f.Candidates {
			if (c.Type == "custom") != custom {
				continue
			}
			for _, a := range c.Addresses {
				addr[strings.ToLower(a)] = c.ID
			}
			if c.Namespace != "" {
				namespaces[strings.ToLower(c.Namespace)] = true
			}
		}
	}
	if f.Cluster != nil {
		for _, ns := range f.Cluster.Namespaces {
			namespaces[strings.ToLower(ns)] = true
		}
	}
	// internal tells api.app (a Service in the namespace "app") from
	// api.stripe.com: the last label of a cluster name is a namespace.
	internal := func(l Link) bool {
		h := strings.ToLower(l.Host)
		if l.Internal || isInternalHost(h) {
			return true
		}
		parts := strings.Split(h, ".")
		return len(parts) == 2 && namespaces[parts[1]]
	}
	lookup := func(l Link) string {
		h := strings.ToLower(l.Host)
		if id, ok := addr[h]; ok {
			return id
		}
		if !internal(l) {
			return "" // api.stripe.com must never collapse to the "api" service
		}
		// name.namespace.svc.cluster.local → name.namespace → name
		parts := strings.Split(h, ".")
		for n := len(parts) - 1; n >= 1; n-- {
			if id, ok := addr[strings.Join(parts[:n], ".")]; ok {
				return id
			}
		}
		return ""
	}
	var out []Link
	for _, l := range f.Links {
		if l.Host == "" {
			out = append(out, l)
			continue
		}
		if l.To != "" {
			out = append(out, l) // resolved by an earlier pass
			continue
		}
		if id := lookup(l); id != "" {
			l.To = id
			if l.Kind == "" {
				l.Kind = "tcp"
			}
			if l.Reverse {
				l.From, l.To, l.Reverse = l.To, l.From, false
			}
			out = append(out, l)
			continue
		}
		if !internal(l) {
			id := externalIDFor(f, l.Host)
			c := f.candidate(id)
			if c == nil {
				f.Candidates = append(f.Candidates, Candidate{ID: id, Type: "external", Label: externalLabel(l.Host), Addresses: []string{l.Host},
					Evidence: append([]Evidence(nil), l.Evidence...)})
			} else {
				c.Addresses = uniq(append(c.Addresses, l.Host))
				c.Evidence = append(c.Evidence, l.Evidence...)
			}
			l.To = id
			l.Kind = "external"
			out = append(out, l)
			continue
		}
		// internal but unknown: keep the host so the reviewer can place it
		l.To = ""
		out = append(out, l)
	}
	f.Links = out
}

// externalName is the second-level label of a host: "stripe" for
// api.stripe.com.
func externalName(host string) string {
	parts := strings.Split(strings.ToLower(host), ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return parts[0]
}

// externalID names an external dependency by its second-level domain, as a
// valid id.
func externalID(host string) string {
	return model.SlugifyID(externalName(host))
}

// externalIDFor is externalID, unless that id already belongs to something
// of the system (a firewall called "bookstore" and the host
// api.bookstore.example): then the whole host makes the id.
func externalIDFor(f *Findings, host string) string {
	id := externalID(host)
	if c := f.candidate(id); c != nil && c.Type != "external" {
		return model.SlugifyID(host)
	}
	return id
}

func externalLabel(host string) string {
	id := externalName(host)
	switch id {
	case "github":
		return "GitHub"
	case "stripe":
		return "Stripe"
	case "openai":
		return "OpenAI"
	case "anthropic":
		return "Anthropic"
	case "googleapis":
		return "Google APIs"
	case "slack":
		return "Slack"
	case "sendgrid":
		return "SendGrid"
	case "twilio":
		return "Twilio"
	case "sentry":
		return "Sentry"
	case "resend":
		return "Resend"
	case "mailgun":
		return "Mailgun"
	}
	return strings.ToUpper(id[:1]) + id[1:]
}

// serviceAddresses lists every DNS name a Kubernetes Service answers to.
func serviceAddresses(name, ns string) []string {
	if ns == "" {
		return []string{name}
	}
	return []string{name, name + "." + ns, name + "." + ns + ".svc", name + "." + ns + ".svc.cluster.local"}
}
