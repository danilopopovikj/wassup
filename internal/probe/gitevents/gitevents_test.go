package gitevents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

const sample = "b7e9f21c0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f80\x1fDanilo\x1f2026-09-27T10:15:00+02:00\x1fapi: retry github calls\n" +
	"a1b2c3d4e5f60718293a4b5c6d7e8f9012345678\x1fCI Bot\x1f2026-09-27T08:00:00Z\x1fchore: bump deps \x1f with a separator\n" +
	"\n"

func TestParseLog(t *testing.T) {
	commits, err := ParseLog(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2", len(commits))
	}
	c := commits[0]
	if c.SHA != "b7e9f21c0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f80" || c.Short() != "b7e9f21" {
		t.Fatalf("sha = %q short = %q", c.SHA, c.Short())
	}
	if c.Author != "Danilo" || c.Subject != "api: retry github calls" {
		t.Fatalf("author/subject = %q/%q", c.Author, c.Subject)
	}
	want := time.Date(2026, 9, 27, 8, 15, 0, 0, time.UTC)
	if !c.At.Equal(want) {
		t.Fatalf("at = %v, want %v", c.At, want)
	}
	// A separator inside the subject stays in the subject.
	if commits[1].Subject != "chore: bump deps \x1f with a separator" {
		t.Fatalf("subject = %q", commits[1].Subject)
	}

	ev := Event("api", c)
	if ev.Kind != "deploy" || ev.Target != "api" || ev.Ref != "b7e9f21" || ev.Author != "Danilo" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Summary != "deploy of api b7e9f21" || !ev.At.Equal(want) {
		t.Fatalf("event = %+v", ev)
	}
}

func TestParseLogErrors(t *testing.T) {
	if _, err := ParseLog("abc\x1fonly two"); err == nil {
		t.Fatal("expected error for a short line")
	}
	if _, err := ParseLog("abc\x1fme\x1fyesterday\x1fsubject"); err == nil {
		t.Fatal("expected error for a bad date")
	}
	if c, err := ParseLog(""); err != nil || len(c) != 0 {
		t.Fatalf("empty output: %v %v", c, err)
	}
}

// fakeGit serves canned git output keyed by the joined arguments.
type fakeGit struct {
	calls   []string
	head    string
	recent  string
	failLog bool
}

func (f *fakeGit) run(_ context.Context, dir string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case args[0] == "rev-parse" && args[1] == "--show-toplevel":
		if dir == "/nope" {
			return "", errors.New("fatal: not a git repository")
		}
		return dir, nil
	case args[0] == "rev-parse":
		return "main\n", nil
	case args[0] == "log" && f.failLog:
		return "", errors.New("fatal: bad revision")
	case args[0] == "log" && args[1] == "-1":
		return f.head, nil
	case args[0] == "log":
		return f.recent, nil
	}
	return "", errors.New("unexpected " + strings.Join(args, " "))
}

func TestCheckEmitsNewCommitsOnce(t *testing.T) {
	fg := &fakeGit{head: strings.SplitN(sample, "\n", 2)[0] + "\n", recent: sample}
	g := &Git{run: fg.run, seen: map[string]time.Time{}}

	o := g.check(context.Background(), "api", ".", "main", "%H", 2*time.Hour)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if o.Metrics != nil {
		t.Fatalf("no metrics expected, got %v", o.Metrics)
	}
	if o.Detail["branch"] != "main" || o.Detail["head"] != "b7e9f21c0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f80" || o.Detail["last_commit_subject"] != "api: retry github calls" {
		t.Fatalf("detail = %v", o.Detail)
	}
	if len(o.Events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(o.Events), o.Events)
	}
	// Oldest first.
	if o.Events[0].Ref != "a1b2c3d" || o.Events[1].Ref != "b7e9f21" {
		t.Fatalf("event order = %s, %s", o.Events[0].Ref, o.Events[1].Ref)
	}
	if !strings.Contains(strings.Join(fg.calls, "|"), "--since=") {
		t.Fatalf("git log was not bounded by --since: %v", fg.calls)
	}

	// Second round: same commits, no new events, detail still there.
	o = g.check(context.Background(), "api", ".", "main", "%H", 2*time.Hour)
	if len(o.Events) != 0 {
		t.Fatalf("commits emitted twice: %+v", o.Events)
	}
	if o.Detail["head"] == nil {
		t.Fatal("detail lost on the second round")
	}

	// A new commit appears: exactly one event.
	fg.recent = "ffff000011112222333344445555666677778888\x1fDanilo\x1f2026-09-27T11:00:00Z\x1fapi: hotfix\n" + sample
	o = g.check(context.Background(), "api", ".", "main", "%H", 2*time.Hour)
	if len(o.Events) != 1 || o.Events[0].Ref != "ffff000" {
		t.Fatalf("events = %+v", o.Events)
	}
	if g.Health().State != probe.HealthOK {
		t.Fatalf("health = %+v", g.Health())
	}
}

func TestCheckGitError(t *testing.T) {
	fg := &fakeGit{failLog: true}
	g := &Git{run: fg.run, seen: map[string]time.Time{}}
	o := g.check(context.Background(), "api", ".", "main", "%H", time.Hour)
	if o.Err == "" || o.Target != "api" {
		t.Fatalf("expected Err, got %+v", o)
	}
	if g.Health().State != probe.HealthDegraded {
		t.Fatalf("health = %+v", g.Health())
	}
}

func TestStartNotARepo(t *testing.T) {
	fg := &fakeGit{}
	g := &Git{run: fg.run}
	err := g.Start(context.Background(), map[string]any{"repo": "/nope"}, make(chan probe.Observation))
	if err == nil {
		t.Fatal("expected error")
	}
	if h := g.Health(); h.State != probe.HealthFailed || !strings.Contains(h.Message, "not a git repository") {
		t.Fatalf("health = %+v", h)
	}
}

func TestValidate(t *testing.T) {
	g := &Git{}
	if err := g.Validate(map[string]any{"author_format": "initials"}); err == nil {
		t.Fatal("expected error")
	}
	if err := g.Validate(map[string]any{"lookback": "2 hours"}); err == nil {
		t.Fatal("expected error")
	}
	if err := g.Validate(map[string]any{"repo": ".", "branch": "main", "interval": "1m", "lookback": "4h", "author_format": "email"}); err != nil {
		t.Fatal(err)
	}
}

// TestRealRepo drives the probe against a throwaway repository when git is
// installed.
func TestRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Tester", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(dir+"/a.txt", []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "first deploy")

	g := &Git{}
	out := make(chan probe.Observation, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spec := map[string]any{"repo": dir, "_target": "api", "_tick": 50 * time.Millisecond, "author_format": "full"}
	if err := g.Start(ctx, spec, out); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-out:
		if o.Err != "" {
			t.Fatal(o.Err)
		}
		if o.Detail["branch"] != "main" || o.Detail["last_commit_subject"] != "first deploy" {
			t.Fatalf("detail = %v", o.Detail)
		}
		if len(o.Events) != 1 || o.Events[0].Author != "Tester <t@example.com>" || !strings.HasPrefix(o.Events[0].Summary, "deploy of api ") {
			t.Fatalf("events = %+v", o.Events)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no observation")
	}
	// Tick re-emits carry no events.
	select {
	case o := <-out:
		if len(o.Events) != 0 {
			t.Fatalf("re-emit carried events: %+v", o.Events)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no re-emit")
	}
}
