package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestPodLogsPrefixesLines(t *testing.T) {
	crash := apiPod("api-x", true, 3, &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}, &corev1.ContainerStateTerminated{ExitCode: 1})
	c := newTestClients([]runtime.Object{apiPod("api-a", true, 0, nil, nil), crash}, nil, nil)
	lines, err := PodLogs(context.Background(), c, "prod", "app=api", time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	// The fake streams "fake logs" for every request: one per container,
	// plus the previous container of the crash-looping pod.
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	want := map[string]bool{"api-a/api fake logs": true, "api-x/api fake logs": true, "api-x/api(previous) fake logs": true}
	for _, l := range lines {
		if !want[l] {
			t.Errorf("unexpected line %q", l)
		}
	}
	if _, err := PodLogs(context.Background(), c, "prod", "app in (", time.Hour, 0); err == nil {
		t.Errorf("bad selector must fail")
	}
}

func TestSplitTimestamp(t *testing.T) {
	at, text := splitTimestamp("2026-09-27T10:00:00.123456789Z Error: migration failed")
	if at.IsZero() || text != "Error: migration failed" {
		t.Errorf("got %v %q", at, text)
	}
	at, text = splitTimestamp("no timestamp here")
	if !at.IsZero() || text != "no timestamp here" {
		t.Errorf("got %v %q", at, text)
	}
}

func TestEvents(t *testing.T) {
	now := time.Now()
	ev := func(name, obj, reason, msg string, at time.Time) *corev1.Event {
		return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod"}, Type: corev1.EventTypeWarning, Reason: reason, Message: msg,
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: obj, Namespace: "prod"}, LastTimestamp: metav1.Time{Time: at}}
	}
	objs := []runtime.Object{
		apiPod("api-1", true, 0, nil, nil),
		ev("e1", "api-1", "BackOff", "Back-off restarting failed container", now.Add(-2*time.Minute)),
		ev("e2", "api-1", "Unhealthy", "Readiness probe failed", now.Add(-time.Minute)),
		ev("e3", "api-1", "Old", "too old", now.Add(-3*time.Hour)),
		ev("e4", "other", "Other", "another pod", now.Add(-time.Minute)),
	}
	c := newTestClients(objs, nil, nil)
	lines, err := Events(context.Background(), c, "prod", "app=api", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "Warning BackOff Back-off restarting failed container") || !strings.Contains(lines[1], "Unhealthy") {
		t.Errorf("lines = %q", lines)
	}
	if len(lines[0]) < 6 || lines[0][2] != ':' {
		t.Errorf("line must start with HH:MM: %q", lines[0])
	}
}
