package hatchet

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// KindHealth is the probe kind of the engine health probe.
const KindHealth = "hatchet.health"

var healthAccess = probe.Access{
	Kind:        KindHealth,
	Source:      "the Hatchet API's /api/ready and /api/live endpoints and /api/v1/meta",
	Delivers:    "latency_ms, ready (1/0), live (1/0); NotReady while /api/ready is not 200; detail: version, url",
	SpecFields:  withFields(),
	Needs:       "HTTP access to the Hatchet API; the token named by token_env is sent when present but not required",
	Implemented: true,
}

func init() {
	probe.Register(healthAccess, func() probe.Probe { return &HealthProbe{} })
}

// HealthProbe is hatchet.health, bound to the engine workload. Spec: url
// (required), token_env, interval, timeout. tenant is accepted and ignored.
type HealthProbe struct {
	h probe.Health
}

// healthState is what one HealthProbe carries between polls.
type healthState struct {
	cfg  config
	c    *client
	seen firstSeen
}

// Kind implements probe.Probe.
func (p *HealthProbe) Kind() string { return KindHealth }

// Validate implements probe.Probe.
func (p *HealthProbe) Validate(spec map[string]any) error { return validateCommon(spec) }

// Health implements probe.Probe.
func (p *HealthProbe) Health() probe.ProbeHealth { return p.h.Get() }

// setup parses the spec into a state, or reports a misconfiguration.
func (p *HealthProbe) setup(spec map[string]any) (*healthState, error) {
	cfg, c, err := configure(spec, requirements{})
	if err != nil {
		return nil, err
	}
	return &healthState{cfg: cfg, c: c, seen: firstSeen{}}, nil
}

// Start implements probe.Probe.
func (p *HealthProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	st, err := p.setup(spec)
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	go run(ctx, out, st.cfg.tick, st.cfg.interval, func(ctx context.Context) probe.Observation {
		return p.poll(ctx, st)
	})
	return nil
}

// poll checks readiness and liveness once. A non-200 answer is data about
// the engine (ready 0, NotReady); only an unreachable API is a probe error.
func (p *HealthProbe) poll(ctx context.Context, st *healthState) probe.Observation {
	now := time.Now()
	ctx, cancel := context.WithTimeout(ctx, st.cfg.timeout*3)
	defer cancel()

	status, body, latency, err := st.c.do(ctx, "/api/ready", nil)
	if err != nil {
		return errObservation(&p.h, KindHealth, st.cfg.target, now, fmt.Errorf("GET /api/ready: %w", err))
	}
	o := probe.Observation{
		Target: st.cfg.target,
		Probe:  KindHealth,
		At:     now,
		Metrics: map[string]float64{
			"latency_ms": float64(latency) / float64(time.Millisecond),
		},
		Detail: map[string]any{"url": st.cfg.base, "ready_status": status},
	}
	live := map[string]bool{}
	if status == http.StatusOK {
		o.Metrics["ready"] = 1
	} else {
		o.Metrics["ready"] = 0
		live["ready"] = true
		detail := http.StatusText(status)
		if text := errorText(body); text != "" {
			detail = fmt.Sprintf("%d %s: %s", status, detail, text)
		} else {
			detail = fmt.Sprintf("%d %s", status, detail)
		}
		o.Conditions = append(o.Conditions, model.Condition{
			Kind:   model.CondNotReady,
			Ref:    "/api/ready",
			Since:  st.seen.mark("ready", now),
			Detail: detail,
		})
	}
	st.seen.keep(live)

	var problems []string
	if status, _, _, err := st.c.do(ctx, "/api/live", nil); err != nil {
		problems = append(problems, "GET /api/live: "+err.Error())
	} else {
		o.Detail["live_status"] = status
		if status == http.StatusOK {
			o.Metrics["live"] = 1
		} else {
			o.Metrics["live"] = 0
		}
	}
	if v, err := st.c.version(ctx); err == nil && v != "" {
		o.Detail["version"] = v
	}

	if len(problems) > 0 {
		p.h.Set(probe.HealthDegraded, joinProblems(problems))
	} else {
		p.h.Set(probe.HealthOK, "")
	}
	return o
}
