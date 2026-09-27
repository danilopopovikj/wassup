package terraform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// Kind is the probe kind.
const Kind = "terraform.state"

const (
	defaultTick     = 5 * time.Second
	defaultInterval = 5 * time.Minute
	watchPoll       = 10 * time.Second
	showTimeout     = 2 * time.Minute
)

func init() {
	probe.Register(probe.Access{
		Kind:        Kind,
		Source:      "terraform show -json in a working directory",
		Delivers:    "terraform events per added, removed or changed resource, the rules count of hcloud firewalls and FirewallDenied when no firewall allows inbound TCP to the edge's port",
		SpecFields:  []string{"dir", "interval", "watch", "resource", "port", "binary"},
		Needs:       "the terraform binary, an initialised working directory and read access to its state backend",
		Implemented: true,
	}, func() probe.Probe { return &Probe{} })
}

// shower runs `terraform show -json` in dir and returns the document; tests
// replace it.
type shower func(ctx context.Context, binary, dir string) ([]byte, error)

// Probe is the terraform.state probe.
type Probe struct {
	h    probe.Health
	show shower
	now  func() time.Time

	prev        []Resource // inventory of the previous successful run
	primed      bool
	deniedSince time.Time // when FirewallDenied was first observed
}

// Kind implements probe.Probe.
func (p *Probe) Kind() string { return Kind }

// Validate implements probe.Probe.
func (p *Probe) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "dir"); err != nil {
		return err
	}
	dir := probe.Str(spec, "dir", "")
	if fi, err := os.Stat(dir); err != nil {
		return fmt.Errorf("dir %q: %w", dir, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("dir %q is not a directory", dir)
	}
	if v, ok := spec["interval"].(string); ok && v != "" {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("interval: %w", err)
		}
	}
	if v, ok := spec["watch"]; ok {
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("watch %v must be true or false", v)
		}
	}
	if v, ok := spec["port"]; ok {
		if n, isNum := probe.Num(spec, "port"); !isNum || n < 1 || n > 65535 || n != float64(int(n)) {
			return fmt.Errorf("port %v must be a TCP port number", v)
		}
	}
	return nil
}

// Health implements probe.Probe.
func (p *Probe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *Probe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := p.Validate(spec); err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	if p.show == nil {
		p.show = runShow
	}
	cfg := configFrom(spec)
	p.h.Set(probe.HealthOK, "reading "+cfg.dir)
	go p.loop(ctx, cfg, out)
	return nil
}

// config is the parsed spec.
type config struct {
	target   string
	dir      string
	binary   string
	tick     time.Duration
	interval time.Duration
	watch    bool
	resource string
	port     int
}

func configFrom(spec map[string]any) config {
	c := config{
		target:   probe.Str(spec, "_target", ""),
		dir:      probe.Str(spec, "dir", ""),
		binary:   probe.Str(spec, "binary", "terraform"),
		tick:     defaultTick,
		watch:    true,
		resource: probe.Str(spec, "resource", ""),
	}
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		c.tick = d
	}
	c.interval = probe.Dur(spec, "interval", defaultInterval)
	if c.interval < c.tick {
		c.interval = c.tick
	}
	if w, ok := spec["watch"].(bool); ok {
		c.watch = w
	}
	if n, ok := probe.Num(spec, "port"); ok {
		c.port = int(n)
	}
	return c
}

// loop runs the state read every interval, immediately when a watched file
// changes, and re-emits the last observation (without its one-shot events)
// every tick, until ctx is done.
func (p *Probe) loop(ctx context.Context, cfg config, out chan<- probe.Observation) {
	files := ""
	if cfg.watch {
		files = watchFingerprint(cfg.dir)
	}
	last := p.check(ctx, cfg)
	if !probe.Send(ctx, out, last) {
		return
	}
	intervalT := time.NewTicker(cfg.interval)
	defer intervalT.Stop()
	tickT := time.NewTicker(cfg.tick)
	defer tickT.Stop()
	var watchC <-chan time.Time
	if cfg.watch {
		watchT := time.NewTicker(watchPoll)
		defer watchT.Stop()
		watchC = watchT.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-intervalT.C:
			last = p.check(ctx, cfg)
			if !probe.Send(ctx, out, last) {
				return
			}
		case <-watchC:
			fp := watchFingerprint(cfg.dir)
			if fp == files {
				continue
			}
			files = fp
			last = p.check(ctx, cfg)
			if !probe.Send(ctx, out, last) {
				return
			}
			intervalT.Reset(cfg.interval)
		case <-tickT.C:
			o := last
			o.At = p.clock()
			o.Events = nil
			if !probe.Send(ctx, out, o) {
				return
			}
		}
	}
}

func (p *Probe) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// check runs terraform show once and builds the observation.
func (p *Probe) check(ctx context.Context, cfg config) probe.Observation {
	o := probe.Observation{Target: cfg.target, Probe: Kind, At: p.clock()}
	sctx, cancel := context.WithTimeout(ctx, showTimeout)
	defer cancel()
	data, err := p.show(sctx, cfg.binary, cfg.dir)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			o.Err = fmt.Sprintf("terraform binary %q not found in PATH", cfg.binary)
			p.h.Set(probe.HealthFailed, o.Err)
			return o
		}
		o.Err = fmt.Sprintf("terraform show in %s: %v", cfg.dir, err)
		p.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	st, err := ParseShow(data)
	if err != nil {
		if errors.Is(err, ErrNoState) {
			o.Err = fmt.Sprintf("%s has %v", cfg.dir, err)
			p.h.Set(probe.HealthFailed, o.Err)
			return o
		}
		o.Err = err.Error()
		p.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	p.observe(&o, cfg, st)
	p.h.Set(probe.HealthOK, fmt.Sprintf("%d resources in %s", len(st.Resources), cfg.dir))
	return o
}

// observe fills o from a parsed state: the diff events against the previous
// run, the firewall metrics and the port check.
func (p *Probe) observe(o *probe.Observation, cfg config, st *State) {
	matched := st.Filter(cfg.resource)
	o.Detail = map[string]any{
		"dir":               cfg.dir,
		"terraform_version": st.TerraformVersion,
		"resources":         len(st.Resources),
	}
	if cfg.resource != "" {
		o.Detail["resource"] = cfg.resource
		o.Detail["matched"] = len(matched)
	}

	// Events: the first run only records the inventory; there is no earlier
	// state to compare against, and replaying the whole state as "applied
	// now" would flood the timeline.
	if p.primed {
		added, removed, changed := Diff(p.prev, matched)
		var all []string
		for _, set := range []struct {
			addrs []string
			verb  string
		}{{added, "added"}, {removed, "removed"}, {changed, ""}} {
			for _, addr := range set.addrs {
				all = append(all, addr)
				summary := "terraform apply: " + addr
				if set.verb != "" {
					summary += " " + set.verb
				}
				o.Events = append(o.Events, model.Event{
					At:      o.At,
					Kind:    "terraform",
					Target:  cfg.target,
					Summary: summary,
					Ref:     addr,
				})
			}
		}
		sort.Strings(all)
		o.Detail["changed"] = all
	}
	p.prev = matched
	p.primed = true

	// Firewalls: the ones under the filter, or every firewall in the state
	// when the binding is not narrowed to one.
	fws := Firewalls(matched)
	if len(fws) > 0 || isFirewallBinding(cfg, matched) {
		o.Detail["firewalls"] = fws
	}
	if isFirewallBinding(cfg, matched) {
		o.Metrics = map[string]float64{"rules": float64(RuleCount(fws))}
	}
	if cfg.port > 0 {
		o.Detail["port"] = cfg.port
		allowed, denying := AllowsInboundTCP(fws, cfg.port)
		o.Detail["port_allowed"] = allowed
		if allowed {
			p.deniedSince = time.Time{}
		} else {
			if p.deniedSince.IsZero() {
				p.deniedSince = o.At
			}
			names := make([]string, 0, len(denying))
			refs := make([]string, 0, len(denying))
			for _, fw := range denying {
				names = append(names, fw.Name)
				refs = append(refs, fw.Address)
			}
			o.Conditions = append(o.Conditions, model.Condition{
				Kind:   model.CondFirewallDenied,
				Ref:    strings.Join(refs, ","),
				Since:  p.deniedSince,
				Detail: strings.Join(names, ","),
			})
		}
	}
}

// isFirewallBinding reports whether the bound element is a firewall: the
// resource filter names an hcloud_firewall or the target id says so.
func isFirewallBinding(cfg config, matched []Resource) bool {
	if cfg.resource != "" {
		for _, r := range matched {
			if r.Type == firewallType {
				return true
			}
		}
		return false
	}
	id := strings.ToLower(cfg.target)
	return strings.Contains(id, "firewall") || strings.Contains(id, "fw")
}

// runShow executes `terraform show -json` in dir.
func runShow(ctx context.Context, binary, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, "show", "-json", "-no-color")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, err
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, firstLine(msg))
		}
		return nil, err
	}
	return out, nil
}

// watchFingerprint hashes the path, size and mtime of every *.tf and
// terraform.tfstate file under dir (skipping .terraform), so a change in
// any of them shows as a different string.
func watchFingerprint(dir string) string {
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".terraform" || (d.Name() == ".git" && path != dir) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".tf") && d.Name() != "terraform.tfstate" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", path, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "╷") && !strings.HasPrefix(l, "│") {
			return strings.TrimPrefix(l, "Error: ")
		}
	}
	return s
}
