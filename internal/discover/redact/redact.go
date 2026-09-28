// Package redact decides what discovery may keep of a value it read from a
// pod spec, a manifest, a ConfigMap or a command line. Discovery writes its
// evidence to disk and prints it, so nothing that could be a credential may
// survive: it keeps the names of variables and the hosts they point at, and
// drops everything else.
//
// The package has no dependency on the rest of wassup so that both the
// repository scanner and the cluster inventory can share one set of rules.
package redact

import (
	"regexp"
	"strings"
)

// Placeholder stands in for a $(VAR) reference that cannot be resolved, or
// that names a credential. It keeps a URL parseable; a host that contains it
// is not a host and is dropped.
const Placeholder = "unresolved-variable"

// Mask replaces a credential inside a command line.
const Mask = "***"

// credentialWords are the fragments that make a name look like a credential.
var credentialWords = []string{"pass", "pwd", "secret", "token", "key", "dsn", "credential", "auth"}

// CredentialName reports whether the name of a variable, a flag or a key
// suggests that its value is a credential. It errs on the side of yes: a
// wrongly hidden value costs a line of evidence, a wrongly kept one is a leak.
func CredentialName(name string) bool {
	n := strings.ToLower(name)
	for _, w := range credentialWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

var (
	// schemeRe finds where a URL may start.
	schemeRe = regexp.MustCompile(`(?:jdbc:)?[A-Za-z][A-Za-z0-9+.-]*://`)
	// hostRe is a DNS name, a compose service name or an IPv4 address.
	hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	// portRe is a port number.
	portRe = regexp.MustCompile(`^[0-9]{1,5}$`)
	// plainRe is a short setting such as a bucket name, a region or a slot.
	plainRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// dbIndexRe is the database number at the end of a redis URL.
	dbIndexRe = regexp.MustCompile(`^/[0-9]{1,2}$`)
	// keywordHostRe and keywordPortRe read "host=db port=5432 password=..." strings.
	keywordHostRe = regexp.MustCompile(`(?i)(?:^|[\s;])(?:host|server|hostname)\s*=\s*([^\s;]+)`)
	keywordPortRe = regexp.MustCompile(`(?i)(?:^|[\s;])port\s*=\s*([0-9]{1,5})`)
	// assignRe finds name=value and "name": "value" pairs.
	// The value may be quoted and then holds spaces.
	assignRe = regexp.MustCompile(`(["']?)([A-Za-z0-9_.-]+)(["']?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s"',;&}]+)`)
	// flagRe finds "--flag value" pairs.
	flagRe = regexp.MustCompile(`(^|\s)(--?[A-Za-z0-9][A-Za-z0-9_.-]*)(\s+)("[^"]*"|'[^']*'|[^\s-][^\s]*)`)
	// schemeOnlyRe is the scheme of a URL, alone.
	schemeOnlyRe = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)
	// wordRe is the name of a variable or a flag.
	wordRe = regexp.MustCompile(`^-{0,2}[A-Za-z0-9_.]+(-[A-Za-z0-9_.]+)*$`)
	// expandRe finds $(VAR) references, the form Kubernetes interpolates.
	expandRe = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_.-]*)\)`)
)

// addressWords mark a variable that names where something listens.
var addressWords = []string{"HOST", "HOSTNAME", "SERVER", "SERVERS", "ENDPOINT", "ADDR", "ADDRESS", "URL", "URI", "BROKER", "BROKERS"}

// settingWords mark the few plain settings discovery needs to propose a
// binding: which bucket, in which region, on which replication slot.
var settingWords = []string{"BUCKET", "REGION", "SLOT"}

// nameHas reports whether one of the words is a part of the name, where
// parts are separated by anything that is not a letter or a digit. PGHOST
// counts as a HOST.
func nameHas(name string, words []string) bool {
	parts := strings.FieldsFunc(strings.ToUpper(name), func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	for _, p := range parts {
		for _, w := range words {
			if p == w || w == "HOST" && strings.HasSuffix(p, "HOST") {
				return true
			}
		}
	}
	return false
}

// secretWords are the fragments of a name whose value is a secret and
// nothing but a secret, unless the name also says it is an address
// (TOKEN_ENDPOINT, KEYCLOAK_URL).
var secretWords = []string{"pass", "pwd", "secret", "token", "key", "credential"}

// EnvValue returns what may be stored of the literal value of a variable:
//
//   - for a URL, the scheme and the host (and the port): never the user, the
//     password, the path or the query, since any of them can carry a secret;
//   - for a "host=... password=..." string, the host, written tcp://host;
//   - for a variable named like an address, a bare host or host:port;
//   - for a bucket, a region or a replication slot, the plain word;
//   - nothing otherwise, and nothing at all for a variable named like a
//     password, a token or a key. A DSN is named like a credential because
//     it holds one; its host is kept, as the host of any URL is.
//
// A value is one setting: when it holds a URL, everything from where the
// URL starts is that URL, blanks and quotes included, because a password
// may hold them.
//
// The result of EnvValue passed through EnvValue again is unchanged.
func EnvValue(name, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	address := nameHas(name, addressWords) || strings.Contains(strings.ToLower(name), "dsn")
	if !address {
		lower := strings.ToLower(name)
		for _, w := range secretWords {
			if strings.Contains(lower, w) {
				return ""
			}
		}
	}
	if starts := urlStarts(value); len(starts) > 0 {
		return URLHost(value[starts[0][0]:])
	}
	if m := keywordHostRe.FindStringSubmatch(value); m != nil {
		host := strings.Trim(m[1], `"'`)
		if !validHost(host) {
			return ""
		}
		if p := keywordPortRe.FindStringSubmatch(value); p != nil {
			host += ":" + p[1]
		}
		// Written as a URL so that the result reads the same when it is
		// reduced again; tcp says only that the protocol is not known.
		return "tcp://" + strings.ToLower(host)
	}
	if address {
		var out []string
		for _, part := range strings.Split(value, ",") {
			h, ok := hostPort(strings.TrimSpace(part))
			if !ok {
				return ""
			}
			out = append(out, h)
		}
		return strings.Join(out, ",")
	}
	if !CredentialName(name) && nameHas(name, settingWords) && plainRe.MatchString(value) {
		return value
	}
	return ""
}

// urlEnd are the characters a URL written in a text stops at.
const urlEnd = " \t\r\n\"'<>`"

// urlStarts finds where URLs may start in a text: wherever a scheme is
// followed by "://", also behind a prefix such as git:: or jdbc:. Whether
// one of them is a URL of its own or a part of the URL before it (a
// password may well hold "://") is for the caller to say.
func urlStarts(s string) [][]int {
	return schemeRe.FindAllStringIndex(s, -1)
}

// urlSpans finds the URLs in a text and returns where each starts and
// ends. A URL ends at a blank or a quote, unless what follows still belongs
// to its user and password: when the first word holds no @ and a later one
// does, before the next URL starts, the URL runs to the end of that word.
// A password with a blank in it is cut whole that way, at the price of
// taking an address that follows a URL for a part of it.
func urlSpans(s string) [][2]int {
	starts := urlStarts(s)
	var out [][2]int
	for n, st := range starts {
		if len(out) > 0 && st[0] < out[len(out)-1][1] {
			continue // inside the URL before it
		}
		limit := len(s)
		if n+1 < len(starts) {
			limit = starts[n+1][0]
		}
		wordEnd := func(from int) int {
			if i := strings.IndexAny(s[from:], urlEnd); i >= 0 {
				return from + i
			}
			return len(s)
		}
		end := wordEnd(st[1])
		if !strings.Contains(s[st[0]:end], "@") && end < limit {
			if at := strings.LastIndex(s[end:limit], "@"); at >= 0 {
				end = wordEnd(end + at)
			}
		}
		out = append(out, [2]int{st[0], end})
	}
	return out
}

// URLHost reduces a URL to scheme://host[:port]. It returns "" when no
// host can be read for certain. An @ after the first slash, question mark
// or hash can be read two ways: as a part of the path or the query, or as
// the end of a password that holds one of those characters unescaped. One
// reading keeps the start of the password as the host, the other keeps the
// end of whatever held the @, so such a URL yields nothing. A redis URL
// keeps its database number, which tells one queue from another and cannot
// hold a secret.
func URLHost(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" {
		return ""
	}
	scheme = strings.TrimPrefix(strings.ToLower(scheme), "jdbc:")
	if !schemeOnlyRe.MatchString(scheme) {
		return ""
	}
	tail := ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest, tail = rest[:i], rest[i:]
	}
	if strings.Contains(tail, "@") {
		return ""
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	// several hosts (mongodb, kafka): the first names the system
	if i := strings.Index(rest, ","); i >= 0 {
		rest = rest[:i]
	}
	hp, ok := hostPort(rest)
	if !ok {
		return ""
	}
	out := scheme + "://" + hp
	if scheme == "redis" || scheme == "rediss" {
		if i := strings.IndexAny(tail, "?#"); i >= 0 {
			tail = tail[:i]
		}
		if dbIndexRe.MatchString(tail) {
			out += tail
		}
	}
	return out
}

// hostPort validates "host" or "host:port" and returns it in lower case.
func hostPort(s string) (string, bool) {
	s = strings.TrimRight(s, ".")
	if strings.HasPrefix(s, "[") { // [::1]:5432
		end := strings.Index(s, "]")
		if end < 0 {
			return "", false
		}
		addr, port := s[1:end], strings.TrimPrefix(s[end+1:], ":")
		if strings.Trim(addr, "0123456789abcdefABCDEF:") != "" || addr == "" {
			return "", false
		}
		if port != "" && !portRe.MatchString(port) {
			return "", false
		}
		return strings.ToLower(s), true
	}
	host, port, hasPort := strings.Cut(s, ":")
	if !validHost(host) {
		return "", false
	}
	if hasPort && !portRe.MatchString(port) {
		return "", false
	}
	return strings.ToLower(s), true
}

// validHost reports whether s can be a host name. A host that holds the
// placeholder of an unresolved variable is not one.
func validHost(s string) bool {
	return len(s) <= 253 && hostRe.MatchString(s) && !strings.Contains(s, Placeholder)
}

// Text makes a free text (a command line, a note) safe to store: every URL
// is reduced to its scheme and host, and the value that follows a name which
// looks like a credential is masked, in the forms --flag=value, --flag value,
// NAME=value and "name": "value".
func Text(s string) string {
	if s == "" {
		return s
	}
	if spans := urlSpans(s); len(spans) > 0 {
		var b strings.Builder
		last := 0
		for _, sp := range spans {
			b.WriteString(s[last:sp[0]])
			if h := URLHost(s[sp[0]:sp[1]]); h != "" {
				b.WriteString(h)
			} else {
				b.WriteString(Mask)
			}
			last = sp[1]
		}
		b.WriteString(s[last:])
		s = b.String()
	}
	s = assignRe.ReplaceAllStringFunc(s, func(m string) string {
		p := assignRe.FindStringSubmatch(m)
		if !CredentialName(p[2]) {
			return m
		}
		return p[1] + p[2] + p[3] + Mask
	})
	s = flagRe.ReplaceAllStringFunc(s, func(m string) string {
		p := flagRe.FindStringSubmatch(m)
		if !CredentialName(p[2]) {
			return m
		}
		return p[1] + p[2] + p[3] + Mask
	})
	return s
}

// Args makes the arguments of a command safe to store. It works on the
// argument list rather than on the joined line because an argument may hold
// blanks: the whole argument after a credential flag is masked, the whole
// remainder of a NAME=value argument, and an argument that is a URL is
// reduced as one URL, whatever its password holds.
func Args(args []string) []string {
	out := make([]string, 0, len(args))
	maskNext := false
	url := func(u string) string {
		if h := URLHost(u); h != "" {
			return h
		}
		return Mask
	}
	for _, a := range args {
		if maskNext {
			maskNext = false
			out = append(out, Mask)
			continue
		}
		if strings.HasPrefix(a, "-") && !strings.ContainsAny(a, "=: ") && CredentialName(a) {
			maskNext = true
			out = append(out, a)
			continue
		}
		// NAME=value as one argument. The name must be a plain word; a URL
		// with a query is not an assignment.
		if name, _, ok := strings.Cut(a, "="); ok && wordRe.MatchString(name) && CredentialName(name) {
			out = append(out, name+"="+Mask)
			continue
		}
		// An argument that is a URL, behind a prefix or not (--url=...,
		// git::https://...): all that follows the scheme is that URL.
		if st := urlStarts(a); len(st) > 0 && !strings.ContainsAny(a[:st[0][0]], " \t") {
			out = append(out, Text(a[:st[0][0]])+url(a[st[0][0]:]))
			continue
		}
		out = append(out, Text(a))
	}
	return out
}

// Expand resolves the $(VAR) references Kubernetes interpolates in a
// container's environment and arguments, so that a URL written as
// scheme://$(USER):$(PASSWORD)@host can be read for its host. A reference to
// a credential, or to a variable lookup does not know, becomes Placeholder:
// the text stays parseable and the secret never enters it.
func Expand(value string, lookup func(name string) (string, bool)) string {
	for depth := 0; depth < 4 && strings.Contains(value, "$("); depth++ {
		next := expandRe.ReplaceAllStringFunc(value, func(m string) string {
			name := expandRe.FindStringSubmatch(m)[1]
			if CredentialName(name) || lookup == nil {
				return Placeholder
			}
			v, ok := lookup(name)
			if !ok || v == "" {
				return Placeholder
			}
			return v
		})
		if next == value {
			break
		}
		value = next
	}
	return value
}
