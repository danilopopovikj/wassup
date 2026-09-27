// Package gitevents implements git.events, the probe that turns commits on
// a branch into deploy events on the timeline. It shells out to the git
// binary and needs nothing but read access to the working copy.
package gitevents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// Kind is the probe kind.
const Kind = "git.events"

const (
	defaultTick     = 5 * time.Second
	defaultInterval = 30 * time.Second
	defaultLookback = 2 * time.Hour
	gitTimeout      = 30 * time.Second
	shortSHA        = 7
)

// authorFormats maps the author_format spec values to git format
// placeholders.
var authorFormats = map[string]string{
	"":      "%an",
	"name":  "%an",
	"email": "%ae",
	"full":  "%an <%ae>",
}

func init() {
	probe.Register(probe.Access{
		Kind:        Kind,
		Source:      "git log of a local working copy",
		Delivers:    "deploy events for every commit on the branch within the lookback, plus the branch head in detail",
		SpecFields:  []string{"repo", "branch", "interval", "lookback", "author_format"},
		Needs:       "the git binary and read access to the repository; no credentials",
		Implemented: true,
	}, func() probe.Probe { return &Git{} })
}

// Commit is one parsed git log line.
type Commit struct {
	SHA     string
	Author  string
	At      time.Time
	Subject string
}

// Short returns the abbreviated SHA.
func (c Commit) Short() string {
	if len(c.SHA) > shortSHA {
		return c.SHA[:shortSHA]
	}
	return c.SHA
}

// runner executes git in a directory and returns stdout; tests replace it.
type runner func(ctx context.Context, dir string, args ...string) (string, error)

// Git is the git.events probe.
type Git struct {
	h   probe.Health
	run runner

	seen map[string]time.Time // SHAs already emitted, with their author date
}

// Kind implements probe.Probe.
func (g *Git) Kind() string { return Kind }

// Validate implements probe.Probe.
func (g *Git) Validate(spec map[string]any) error {
	for _, k := range []string{"interval", "lookback"} {
		if v, ok := spec[k].(string); ok && v != "" {
			if _, err := time.ParseDuration(v); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	if f := probe.Str(spec, "author_format", ""); f != "" {
		if _, ok := authorFormats[f]; !ok {
			return fmt.Errorf("author_format %q must be name, email or full", f)
		}
	}
	return nil
}

// Health implements probe.Probe.
func (g *Git) Health() probe.ProbeHealth { return g.h.Get() }

// Start implements probe.Probe. It fails when repo is not a git working
// copy, so a typo in the path shows as a failed probe rather than an idle
// timeline.
func (g *Git) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := g.Validate(spec); err != nil {
		g.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	if g.run == nil {
		g.run = runGit
	}
	if g.seen == nil {
		g.seen = map[string]time.Time{}
	}
	repo := probe.Str(spec, "repo", ".")
	target := probe.Str(spec, "_target", "")
	tick := defaultTick
	if d, ok := spec["_tick"].(time.Duration); ok && d > 0 {
		tick = d
	}
	interval := probe.Dur(spec, "interval", defaultInterval)
	if interval < tick {
		interval = tick
	}
	lookback := probe.Dur(spec, "lookback", defaultLookback)
	format := "%H%x1f" + authorFormats[probe.Str(spec, "author_format", "")] + "%x1f%aI%x1f%s"

	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	if _, err := g.run(cctx, repo, "rev-parse", "--show-toplevel"); err != nil {
		msg := fmt.Sprintf("%s is not a git repository: %v", repo, err)
		g.h.Set(probe.HealthFailed, msg)
		return errors.New(msg)
	}
	branch := probe.Str(spec, "branch", "")
	if branch == "" {
		b, err := g.run(cctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			msg := fmt.Sprintf("cannot resolve the current branch of %s: %v", repo, err)
			g.h.Set(probe.HealthFailed, msg)
			return errors.New(msg)
		}
		branch = strings.TrimSpace(b)
	}
	g.h.Set(probe.HealthOK, fmt.Sprintf("watching %s on %s", branch, repo))

	go g.loop(ctx, out, tick, interval, func(ctx context.Context) probe.Observation {
		return g.check(ctx, target, repo, branch, format, lookback)
	})
	return nil
}

// loop runs check every interval and re-emits the last observation (minus
// its events, which are emitted once) every tick until ctx is done.
func (g *Git) loop(ctx context.Context, out chan<- probe.Observation, tick, interval time.Duration, check func(context.Context) probe.Observation) {
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
			o.Events = nil
			if !probe.Send(ctx, out, o) {
				return
			}
		}
	}
}

// check reads the branch head and the commits within lookback, and returns
// the observation with a deploy event per commit not yet reported.
func (g *Git) check(ctx context.Context, target, repo, branch, format string, lookback time.Duration) probe.Observation {
	now := time.Now()
	o := probe.Observation{Target: target, Probe: Kind, At: now}
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	headOut, err := g.run(cctx, repo, "log", "-1", "--format="+format, branch, "--")
	if err != nil {
		o.Err = fmt.Sprintf("git log %s: %v", branch, err)
		g.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	heads, err := ParseLog(headOut)
	if err != nil {
		o.Err = fmt.Sprintf("git log %s: %v", branch, err)
		g.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	o.Detail = map[string]any{"repo": repo, "branch": branch}
	if len(heads) > 0 {
		o.Detail["head"] = heads[0].SHA
		o.Detail["last_commit_subject"] = heads[0].Subject
		o.Detail["last_commit_at"] = heads[0].At.Format(time.RFC3339)
	}

	since := now.Add(-lookback)
	recentOut, err := g.run(cctx, repo, "log", "--format="+format, "--since="+since.Format(time.RFC3339), branch, "--")
	if err != nil {
		o.Err = fmt.Sprintf("git log --since %s: %v", branch, err)
		g.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	recent, err := ParseLog(recentOut)
	if err != nil {
		o.Err = fmt.Sprintf("git log --since %s: %v", branch, err)
		g.h.Set(probe.HealthDegraded, o.Err)
		return o
	}
	o.Detail["commits_in_lookback"] = len(recent)

	// git log lists newest first; emit oldest first so the timeline fills
	// in order.
	listed := make(map[string]bool, len(recent))
	for i := len(recent) - 1; i >= 0; i-- {
		c := recent[i]
		listed[c.SHA] = true
		if _, done := g.seen[c.SHA]; done {
			continue
		}
		g.seen[c.SHA] = c.At
		o.Events = append(o.Events, Event(target, c))
	}
	// Forget SHAs that git no longer lists and that fell out of the lookback,
	// so the set does not grow forever on a busy branch.
	for sha, at := range g.seen {
		if !listed[sha] && at.Before(since) {
			delete(g.seen, sha)
		}
	}
	g.h.Set(probe.HealthOK, fmt.Sprintf("%s at %s", branch, shortOf(o.Detail["head"])))
	return o
}

// Event builds the deploy event for a commit on target.
func Event(target string, c Commit) model.Event {
	return model.Event{
		At:      c.At,
		Kind:    "deploy",
		Target:  target,
		Summary: fmt.Sprintf("deploy of %s %s", target, c.Short()),
		Author:  c.Author,
		Ref:     c.Short(),
	}
}

// ParseLog parses the output of git log with the format
// %H%x1f<author>%x1f%aI%x1f%s: one commit per line, fields separated by the
// ASCII unit separator. Blank lines are skipped.
func ParseLog(out string) ([]Commit, error) {
	var commits []Commit
	for n, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 4)
		if len(parts) != 4 {
			return nil, fmt.Errorf("line %d: expected 4 fields, got %d", n+1, len(parts))
		}
		at, err := time.Parse(time.RFC3339, parts[2])
		if err != nil {
			return nil, fmt.Errorf("line %d: author date %q: %w", n+1, parts[2], err)
		}
		commits = append(commits, Commit{
			SHA:     parts[0],
			Author:  parts[1],
			At:      at,
			Subject: parts[3],
		})
	}
	return commits, nil
}

// runGit executes git in dir and returns its stdout, with stderr folded into
// the error.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return string(out), nil
}

func shortOf(v any) string {
	s, _ := v.(string)
	if len(s) > shortSHA {
		return s[:shortSHA]
	}
	return s
}
