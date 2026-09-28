// Package redisprobe implements the Redis-backed probes of wassup: redis.info
// for a cache box, celery.queue for a Celery queue on a Redis broker and
// redis.list for a plain list used as a queue. Every probe is read only: it
// runs INFO, LLEN and LINDEX and never writes a key. The client refuses any
// other command before it is sent (readOnlyHook).
package redisprobe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// roundTimeout bounds one round of commands against the server.
const roundTimeout = 5 * time.Second

// defaultTick is used when the runtime did not inject one.
const defaultTick = 5 * time.Second

// defaultPort is the port of a server the spec names no port for.
const defaultPort = 6379

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

// address checks how a spec names its server and returns the address of the
// forms that are one: a url in spec[urlKey], "addr" (host:port), or "host"
// with an optional "port". One of the three, so that two of them can never
// disagree.
func address(spec map[string]any, urlKey string) (url, addr string, err error) {
	url = probe.Str(spec, urlKey, "")
	addr = probe.Str(spec, "addr", "")
	host := probe.Str(spec, "host", "")
	var given []string
	for k, v := range map[string]string{urlKey: url, "addr": addr, "host": host} {
		if v != "" {
			given = append(given, k)
		}
	}
	sort.Strings(given)
	switch {
	case len(given) == 0:
		return "", "", fmt.Errorf("%q, %q or %q is required", urlKey, "addr", "host")
	case len(given) > 1:
		return "", "", fmt.Errorf("%s are both set; pick one: %s for a whole URL, addr for host:port, or host and port", strings.Join(given, " and "), urlKey)
	}
	port, hasPort, err := specPort(spec)
	switch {
	case err != nil:
		return "", "", err
	case hasPort && host == "":
		return "", "", fmt.Errorf("port goes with host; %s already names the port", given[0])
	case host != "" && !hasPort:
		port = defaultPort
	}
	if host != "" {
		addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
	return url, addr, nil
}

// options builds the client options from a spec. The address comes from
// "url" (redis:// or rediss://), "addr" (host:port) or "host" and "port";
// the password only from the environment variable named by "password_env",
// taken as it is, so nothing has to be encoded (a password embedded in a URL
// is honoured by ParseURL but the spec should not carry one). "user" names
// the ACL user, "db" selects the logical database and "tls" enables TLS on a
// plain address.
func options(spec map[string]any, urlKey string) (*redis.Options, error) {
	u, addr, err := address(spec, urlKey)
	if err != nil {
		return nil, err
	}
	opt := &redis.Options{Addr: addr}
	if u != "" {
		if opt, err = redis.ParseURL(u); err != nil {
			return nil, fmt.Errorf("%s: %w", urlKey, err)
		}
	}
	if user := probe.Str(spec, "user", ""); user != "" {
		opt.Username = user
	}
	if env := probe.Str(spec, "password_env", ""); env != "" {
		pw, ok := os.LookupEnv(env)
		if !ok {
			return nil, fmt.Errorf("environment variable %s (password_env) is not set", env)
		}
		opt.Password = pw
	}
	if db, ok := probe.Num(spec, "db"); ok {
		opt.DB = int(db)
	}
	if on, ok := spec["tls"].(bool); ok && on && opt.TLSConfig == nil {
		opt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	opt.DialTimeout = roundTimeout
	opt.ReadTimeout = roundTimeout
	opt.WriteTimeout = roundTimeout
	opt.PoolSize = 2
	opt.MaxRetries = 1
	// One dial per attempt. The driver's default is five, a tenth of a
	// second apart: a port nothing listens on would take a second to say so.
	opt.DialerRetries = 1
	return opt, nil
}

// newClient builds the client of a probe. It is the only place a client is
// made, so every one carries the hook that refuses what does not read.
func newClient(opt *redis.Options) *redis.Client {
	c := redis.NewClient(opt)
	c.AddHook(readOnlyHook{})
	return c
}

// readOnlyHook refuses every command that is not on the list of reads
// before it is sent. The probes only call reads; the hook is what keeps a
// command added later from reaching the server.
type readOnlyHook struct{}

// DialHook implements redis.Hook.
func (readOnlyHook) DialHook(next redis.DialHook) redis.DialHook { return next }

// ProcessHook implements redis.Hook.
func (readOnlyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := refuse(cmd); err != nil {
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// ProcessPipelineHook implements redis.Hook. One command that does not read
// refuses the whole pipeline, a transaction included.
func (readOnlyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if err := refuse(cmd); err != nil {
				for _, c := range cmds {
					c.SetErr(err)
				}
				return err
			}
		}
		return next(ctx, cmds)
	}
}

// refuse returns the error of a command that is not a read. The list is
// what the probes read (INFO, LLEN, LINDEX) and what the driver says to set
// up a connection, which touches no key.
func refuse(cmd redis.Cmder) error {
	name := strings.ToLower(cmd.Name())
	switch name {
	case "info", "llen", "lindex":
		return nil
	case "hello", "auth", "select", "ping", "quit":
		return nil
	case "client":
		if args := cmd.Args(); len(args) > 1 {
			switch sub := strings.ToLower(fmt.Sprint(args[1])); sub {
			case "setinfo", "setname", "maint_notifications":
				return nil
			default:
				name += " " + sub
			}
		}
	}
	return fmt.Errorf("%w: redis command %s was not sent", probe.ErrReadOnly, name)
}

// validateOptions checks the spec shape without touching the environment or
// the network, so `wassup validate` works offline.
func validateOptions(spec map[string]any, urlKey string) error {
	u, _, err := address(spec, urlKey)
	if err != nil {
		return err
	}
	if u != "" {
		if _, err := redis.ParseURL(u); err != nil {
			return fmt.Errorf("%s: %w", urlKey, err)
		}
	}
	for _, k := range []string{"host", "user", "password_env"} {
		if v, ok := spec[k]; ok {
			if _, isStr := v.(string); !isStr {
				return fmt.Errorf("%s must be a string, got %T", k, v)
			}
		}
	}
	if v, ok := spec["db"]; ok {
		if _, isNum := probe.Num(spec, "db"); !isNum {
			return fmt.Errorf("db must be a number, got %T", v)
		}
	}
	if v, ok := spec["tls"]; ok {
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("tls must be a boolean, got %T", v)
		}
	}
	return nil
}

// tick returns the injected tick or the default.
func tick(spec map[string]any) time.Duration {
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		return d
	}
	return defaultTick
}

// target returns the bound element id.
func target(spec map[string]any) string { return probe.Str(spec, "_target", "") }

// firstSeen remembers when a condition was first observed by this probe so
// Since is stable across ticks and cleared when the condition goes away.
type firstSeen map[string]time.Time

// mark returns the time the key was first seen, recording now if new.
func (f firstSeen) mark(key string, now time.Time) time.Time {
	if t, ok := f[key]; ok {
		return t
	}
	f[key] = now
	return now
}

// keep drops every key not in live.
func (f firstSeen) keep(live map[string]bool) {
	for k := range f {
		if !live[k] {
			delete(f, k)
		}
	}
}
