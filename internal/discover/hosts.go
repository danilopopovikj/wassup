package discover

import (
	"net/url"
	"regexp"
	"strings"
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

func parseDSN(raw string) (hostRef, bool) {
	u, err := url.Parse(strings.TrimRight(raw, ".,;"))
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
// addresses; unknown public hosts become external candidates.
func resolveLinks(f *Findings) {
	addr := map[string]string{} // address -> candidate id
	for _, c := range f.Candidates {
		for _, a := range c.Addresses {
			addr[strings.ToLower(a)] = c.ID
		}
	}
	lookup := func(host string) string {
		h := strings.ToLower(host)
		if id, ok := addr[h]; ok {
			return id
		}
		if !isInternalHost(h) {
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
		if id := lookup(l.Host); id != "" {
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
		if !isInternalHost(l.Host) {
			// an external dependency: name it by its second-level domain
			id := externalID(l.Host)
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

func externalID(host string) string {
	parts := strings.Split(strings.ToLower(host), ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return parts[0]
}

func externalLabel(host string) string {
	id := externalID(host)
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
