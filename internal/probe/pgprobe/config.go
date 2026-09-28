package pgprobe

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// connFields are the spec fields that name a connection piece by piece, the
// alternative to one DSN in dsn_env. They exist so that nobody builds a URL
// by hand: a password with an @ or a slash in it has to be encoded in a URL
// and is taken as it is here.
var connFields = []string{"host", "port", "user", "database", "sslmode", "password_env"}

// sslModes are the values of sslmode, as libpq names them.
var sslModes = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}

// defaultPort is the port of a server the spec names no port for.
const defaultPort = 5432

// fieldsSet lists the connection fields a spec carries.
func fieldsSet(spec map[string]any) []string {
	var out []string
	for _, k := range connFields {
		if v, ok := spec[k]; ok && v != nil && v != "" {
			out = append(out, k)
		}
	}
	return out
}

// tunnelled reports whether the spec names a tunnel wassup opens itself.
func tunnelled(spec map[string]any) bool {
	_, _, ok := probe.SplitVia(probe.Str(spec, "via", ""))
	return ok
}

// specPort reads port as a number or as digits in a string. ok is false when
// the spec has none.
func specPort(spec map[string]any) (port int, ok bool, err error) {
	v, has := spec["port"]
	if !has || v == nil || v == "" {
		return 0, false, nil
	}
	switch x := v.(type) {
	case string:
		port, err = strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, true, fmt.Errorf("port must be a number, got %q", x)
		}
	default:
		n, isNum := probe.Num(spec, "port")
		if !isNum || n != float64(int(n)) {
			return 0, true, fmt.Errorf("port must be a number, got %v", v)
		}
		port = int(n)
	}
	if port < 1 || port > 65535 {
		return 0, true, fmt.Errorf("port %d is not between 1 and 65535", port)
	}
	return port, true, nil
}

// validateConn checks how a spec names its connection: by dsnKey or by the
// connection fields, never both. It reads neither the environment nor the
// network, so `wassup validate` works where the credentials are not.
func validateConn(spec map[string]any, dsnKey string) error {
	fields := fieldsSet(spec)
	dsn := probe.Str(spec, dsnKey, "")
	switch {
	case dsn != "" && len(fields) > 0:
		return fmt.Errorf("%s and %s are both set; pick one: %s for a whole connection string, or host, port, user, database, sslmode and password_env",
			dsnKey, strings.Join(fields, ", "), dsnKey)
	case dsn != "":
		return nil
	case len(fields) == 0:
		return fmt.Errorf("%q is required, or the connection as host, port, user, database, sslmode and password_env", dsnKey)
	}
	for _, k := range []string{"host", "user", "database", "sslmode", "password_env"} {
		if v, ok := spec[k]; ok {
			if _, isStr := v.(string); !isStr {
				return fmt.Errorf("%s must be a string, got %T", k, v)
			}
		}
	}
	if probe.Str(spec, "user", "") == "" {
		return fmt.Errorf("%q is required when the connection is given by its fields", "user")
	}
	if probe.Str(spec, "host", "") == "" && !tunnelled(spec) {
		return fmt.Errorf("%q is required, unless via names a tunnel (k8s.service/<namespace>/<service>:<port>), which gives the address", "host")
	}
	if _, _, err := specPort(spec); err != nil {
		return err
	}
	if m := probe.Str(spec, "sslmode", ""); m != "" && !sslModes[m] {
		modes := make([]string, 0, len(sslModes))
		for k := range sslModes {
			modes = append(modes, k)
		}
		sort.Strings(modes)
		return fmt.Errorf("sslmode %q is not one of %s", m, strings.Join(modes, ", "))
	}
	return nil
}

// quote writes a value of a keyword/value connection string: in single
// quotes, with a backslash before a quote or a backslash.
func quote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// tunnelName is the name a server behind a tunnel goes by, for the TLS
// handshake and for messages, when the spec names no host: the Service as
// the cluster resolves it, or the pod.
func tunnelName(via string) (host string, port int) {
	scheme, target, ok := probe.SplitVia(via)
	if !ok {
		return "", 0
	}
	ns, rest, _ := strings.Cut(target, "/")
	name, portText, _ := strings.Cut(rest, ":")
	port, _ = strconv.Atoi(portText)
	if strings.HasSuffix(scheme, ".service") {
		return name + "." + ns + ".svc", port
	}
	return name, port
}

// source says where a connection's settings came from, for the messages that
// tell a person what to check.
type source struct {
	// dsnEnv is the variable that holds the DSN, passwordEnv the one that
	// holds the password; one of them is empty.
	dsnEnv, passwordEnv string
}

// password names where the password comes from.
func (s source) password() string {
	if s.passwordEnv != "" {
		return "the password in " + s.passwordEnv
	}
	if s.dsnEnv != "" {
		return "the password in the DSN of " + s.dsnEnv
	}
	return "the password"
}

// configFor builds the connection settings of a spec: from the DSN in the
// variable spec[dsnKey] names, or from the connection fields. defaultDB is
// the database of a spec that gives fields and names none.
//
// With fields the password never passes through a connection string: the
// string is parsed without one and the password is set on the result, as it
// was read from the environment.
func configFor(spec map[string]any, dsnKey, defaultDB string) (*pgx.ConnConfig, source, error) {
	if err := validateConn(spec, dsnKey); err != nil {
		return nil, source{}, err
	}
	if env := probe.Str(spec, dsnKey, ""); env != "" {
		dsn, ok := os.LookupEnv(env)
		if !ok || dsn == "" {
			return nil, source{}, fmt.Errorf("environment variable %s (%s) is not set", env, dsnKey)
		}
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			return nil, source{}, fmt.Errorf("%s: invalid DSN: %w", env, err)
		}
		return cfg, source{dsnEnv: env}, nil
	}

	src := source{passwordEnv: probe.Str(spec, "password_env", "")}
	password, hasPassword := "", false
	if src.passwordEnv != "" {
		password, hasPassword = os.LookupEnv(src.passwordEnv)
		if !hasPassword || password == "" {
			return nil, source{}, fmt.Errorf("environment variable %s (password_env) is not set", src.passwordEnv)
		}
	}
	host := probe.Str(spec, "host", "")
	port, hasPort, _ := specPort(spec)
	if viaHost, viaPort := tunnelName(probe.Str(spec, "via", "")); host == "" {
		host = viaHost
		if !hasPort && viaPort > 0 {
			port, hasPort = viaPort, true
		}
	}
	if !hasPort {
		port = defaultPort
	}
	kv := []string{"host=" + quote(host), "port=" + strconv.Itoa(port), "user=" + quote(probe.Str(spec, "user", ""))}
	if db := probe.Str(spec, "database", defaultDB); db != "" {
		kv = append(kv, "dbname="+quote(db))
	}
	if m := probe.Str(spec, "sslmode", ""); m != "" {
		kv = append(kv, "sslmode="+m)
	}
	cfg, err := pgx.ParseConfig(strings.Join(kv, " "))
	if err != nil {
		return nil, source{}, fmt.Errorf("connection fields: %w", err)
	}
	if hasPassword {
		cfg.Password = password
	}
	return cfg, src, nil
}
