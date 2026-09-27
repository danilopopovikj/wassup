package redisprobe

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// listAccess documents redis.list.
var listAccess = probe.Access{
	Kind:        "redis.list",
	Source:      "LLEN on one Redis list",
	Delivers:    "depth",
	SpecFields:  []string{"addr", "url", "key", "password_env", "db", "tls"},
	Needs:       "network access to the Redis port; a password in the environment variable named by password_env when AUTH is on",
	Implemented: true,
}

func init() {
	probe.Register(listAccess, func() probe.Probe { return &ListProbe{} })
}

// ListProbe is redis.list: the depth of a plain list used as a queue. Spec:
// addr or url, key (required), password_env, db, tls.
type ListProbe struct {
	h probe.Health
}

// Kind implements probe.Probe.
func (p *ListProbe) Kind() string { return listAccess.Kind }

// Validate implements probe.Probe.
func (p *ListProbe) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "key"); err != nil {
		return err
	}
	return validateOptions(spec, "url")
}

// Health implements probe.Probe.
func (p *ListProbe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *ListProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	opt, err := options(spec, "url")
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	key := probe.Str(spec, "key", "")
	client := redis.NewClient(opt)
	tgt := target(spec)
	every := tick(spec)
	go func() {
		defer client.Close()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			o := probe.Observation{Target: tgt, Probe: p.Kind(), At: time.Now()}
			rctx, cancel := context.WithTimeout(ctx, roundTimeout)
			depth, err := client.LLen(rctx, key).Result()
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				o.Err = "LLEN " + key + ": " + err.Error()
				p.h.Set(probe.HealthDegraded, o.Err)
			} else {
				o.Metrics = map[string]float64{"depth": float64(depth)}
				o.Detail = map[string]any{"key": key}
				p.h.Set(probe.HealthOK, "")
			}
			if !probe.Send(ctx, out, o) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}
