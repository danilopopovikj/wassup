package redisprobe

import (
	"context"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

// listAccess documents redis.list.
var listAccess = probe.Access{
	Kind:       "redis.list",
	Source:     "LLEN on one Redis list",
	Delivers:   "depth; detail: key, via",
	SpecFields: []string{"addr", "url", "host", "port", "key", "user", "password_env", "db", "tls", "via", "kubeconfig", "context"},
	Needs: "network access to the Redis port, named by url, by addr (host:port) or by host and port; " +
		"a password in the environment variable named by password_env when AUTH is on (taken as it is, nothing to encode), and user for an ACL user" + viaNeeds,
	Implemented: true,
	Facets:      []string{facet.NameQueue},
	Tier:        probe.TierData,
}

func init() {
	probe.Register(listAccess, func() probe.Probe { return &ListProbe{} })
}

// ListProbe is redis.list: the depth of a plain list used as a queue. Spec:
// url, addr or host and port, or none of them with via; key (required),
// user, password_env, db, tls, via.
type ListProbe struct {
	h probe.Health
	probe.Lifetime
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
	l, err := connect(spec, "url")
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	key := probe.Str(spec, "key", "")
	srv := serverOf(l.opt, spec)
	tgt := target(spec)
	every := tick(spec)
	p.Go(func() {
		defer l.close()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			o := probe.Observation{Target: tgt, Probe: p.Kind(), At: time.Now()}
			rctx, cancel := probe.RoundContext(ctx, roundTimeout)
			depth, err := l.get().LLen(rctx, key).Result()
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				l.failed(err)
				o.Err = srv.explain("LLEN "+key, err)
				p.h.Set(probe.HealthDegraded, o.Err)
			} else {
				// The facet writes the canonical queue form: depth.
				facet.EmitQueue(&o, facet.QueueFacet{Depth: facet.NI(int(depth))}, o.At)
				o.Detail = map[string]any{"key": key}
				noteVia(o.Detail, spec)
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
	})
	return nil
}
