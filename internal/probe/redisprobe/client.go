// Package redisprobe implements the Redis-backed probes of wassup: redis.info
// for a cache box, celery.queue for a Celery queue on a Redis broker and
// redis.list for a plain list used as a queue. Every probe is read only: it
// runs INFO, LLEN and LINDEX and never writes a key.
package redisprobe

import (
	"crypto/tls"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// roundTimeout bounds one round of commands against the server.
const roundTimeout = 5 * time.Second

// defaultTick is used when the runtime did not inject one.
const defaultTick = 5 * time.Second

// options builds the client options from a spec. The address comes from
// "addr" (host:port) or "url" (redis:// or rediss://); the password only from
// the environment variable named by "password_env" (a password embedded in a
// URL is honoured by ParseURL but the spec should not carry one). "db"
// selects the logical database and "tls" enables TLS on a plain addr.
func options(spec map[string]any, urlKey string) (*redis.Options, error) {
	var opt *redis.Options
	if u := probe.Str(spec, urlKey, ""); u != "" {
		o, err := redis.ParseURL(u)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", urlKey, err)
		}
		opt = o
	} else if addr := probe.Str(spec, "addr", ""); addr != "" {
		opt = &redis.Options{Addr: addr}
	} else {
		return nil, fmt.Errorf("%q or %q is required", "addr", urlKey)
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
	return opt, nil
}

// validateOptions checks the spec shape without touching the environment or
// the network, so `wassup validate` works offline.
func validateOptions(spec map[string]any, urlKey string) error {
	u := probe.Str(spec, urlKey, "")
	addr := probe.Str(spec, "addr", "")
	if u == "" && addr == "" {
		return fmt.Errorf("%q or %q is required", "addr", urlKey)
	}
	if u != "" {
		if _, err := redis.ParseURL(u); err != nil {
			return fmt.Errorf("%s: %w", urlKey, err)
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
