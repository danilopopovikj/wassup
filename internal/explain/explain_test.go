package explain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/scenario"
)

func TestExplainFitsAndNamesThings(t *testing.T) {
	run, err := scenario.Load("../../testdata/scenarios/01-deploy-crash-loop")
	if err != nil {
		t.Fatal(err)
	}
	snap := run.Play()
	logs := func(ctx context.Context, comp model.Component, spec model.ProbeSpec, since time.Duration, tail int) ([]string, error) {
		var out []string
		for i := 0; i < 300; i++ {
			out = append(out, "api-7d9f/api django.db.utils.ProgrammingError: relation \"invoices\" does not exist")
		}
		return out, nil
	}
	src := Source{Cfg: run.Config, Snapshot: snap, Frames: run.Frames, Events: run.Binder.Events(), Now: snap.GeneratedAt, Logs: logs}
	ref, _ := model.ParseRef("wassup://workload/api")
	res, err := Explain(context.Background(), src, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) > MaxLines {
		t.Errorf("explain is %d lines, max %d", len(res.Lines), MaxLines)
	}
	text := strings.Join(res.Lines, "\n")
	for _, want := range []string{"failing, 1 of 3 replicas, 6 restarts in 5 min", "CrashLoopBackOff", "binding: k8s.workload", "deploy of api b7e9f21", "lens story", "logs (last"} {
		if !strings.Contains(text, want) {
			t.Errorf("explain lacks %q", want)
		}
	}
	// A scrubbed ref explains the history frame and says so.
	ref.At = snap.GeneratedAt.Add(-time.Minute)
	res, err = Explain(context.Background(), src, ref)
	if err != nil || !res.Historical || !strings.Contains(strings.Join(res.Lines, "\n"), "history frame") {
		t.Errorf("historical explain: %v historical=%v", err, res.Historical)
	}
	// An edge ref works too.
	eref, _ := model.ParseRef("ingress->api")
	res, err = Explain(context.Background(), src, eref)
	if err != nil || !strings.Contains(res.Lines[1], "http edge") {
		t.Errorf("edge explain: %v %v", err, res.Lines[:2])
	}
}
