// Package netprobe implements the network-facing probes of wassup: an HTTP
// ping for external dependencies (http.ping), a DNS record check
// (dns.record) and a TLS certificate check (cert.tls). None of them needs
// credentials; they observe the same public surface a user would hit.
package netprobe

import (
	"context"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

// defaultTick is used when the runtime did not inject spec["_tick"].
const defaultTick = 5 * time.Second

// tickOf returns the runtime tick injected into the spec, or the default.
func tickOf(spec map[string]any) time.Duration {
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		return d
	}
	return defaultTick
}

// targetOf returns the bound element id injected into the spec.
func targetOf(spec map[string]any) string {
	return probe.Str(spec, "_target", "")
}

// loop is the shared schedule of the netprobe probes: check runs once
// immediately and then every interval; the last observation is re-emitted
// every tick in between (with a fresh At) so the engine can tell a slow
// interval from a dead probe. It returns when ctx is done.
func loop(ctx context.Context, tick, interval time.Duration, out chan<- probe.Observation, check func(context.Context) probe.Observation) {
	if interval < tick {
		interval = tick
	}
	last := check(ctx)
	if !probe.Send(ctx, out, last) {
		return
	}
	checkT := time.NewTicker(interval)
	defer checkT.Stop()
	var tickC <-chan time.Time
	if interval > tick {
		tickT := time.NewTicker(tick)
		defer tickT.Stop()
		tickC = tickT.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-checkT.C:
			last = check(ctx)
			if !probe.Send(ctx, out, last) {
				return
			}
		case <-tickC:
			o := last
			o.At = time.Now()
			if !probe.Send(ctx, out, o) {
				return
			}
		}
	}
}
