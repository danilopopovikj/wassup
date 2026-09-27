package k8s

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
)

// logLine is one log line with the pod and container it came from.
type logLine struct {
	at   time.Time
	seq  int
	text string
}

// PodLogs returns the merged log lines of every pod matching the label
// selector, each prefixed "<pod>/<container> ". The kubelet timestamps are
// used to sort the lines across pods and then stripped. For containers that
// have restarted, the previous container's logs are read too, so a crash
// loop shows the failing run; when the current logs cannot be read (a
// container that has not started yet) the previous ones are tried instead.
func PodLogs(ctx context.Context, c *Clients, namespace, selector string, since time.Duration, tail int64) ([]string, error) {
	if c == nil || c.Core == nil {
		return nil, fmt.Errorf("no kubernetes client")
	}
	if _, err := labels.Parse(selector); err != nil {
		return nil, fmt.Errorf("selector %q: %w", selector, err)
	}
	pods, err := c.Core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	var lines []logLine
	seq := 0
	add := func(pod, container string, rc io.ReadCloser) {
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		prefix := pod + "/" + container + " "
		for sc.Scan() {
			at, text := splitTimestamp(sc.Text())
			seq++
			lines = append(lines, logLine{at: at, seq: seq, text: prefix + text})
		}
	}
	var lastErr error
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, ct := range pod.Spec.Containers {
			opts := &corev1.PodLogOptions{Container: ct.Name, Timestamps: true}
			if since > 0 {
				s := int64(since.Seconds())
				opts.SinceSeconds = &s
			}
			if tail > 0 {
				t := tail
				opts.TailLines = &t
			}
			var status *corev1.ContainerStatus
			for j := range pod.Status.ContainerStatuses {
				if pod.Status.ContainerStatuses[j].Name == ct.Name {
					status = &pod.Status.ContainerStatuses[j]
				}
			}
			rc, err := c.Core.CoreV1().Pods(namespace).GetLogs(pod.Name, opts).Stream(ctx)
			if err != nil {
				lastErr = err
				prev := *opts
				prev.Previous = true
				if rc, err := c.Core.CoreV1().Pods(namespace).GetLogs(pod.Name, &prev).Stream(ctx); err == nil {
					add(pod.Name, ct.Name+"(previous)", rc)
				}
				continue
			}
			add(pod.Name, ct.Name, rc)
			if status != nil && status.RestartCount > 0 && crashLooping(status) {
				prev := *opts
				prev.Previous = true
				if rc, err := c.Core.CoreV1().Pods(namespace).GetLogs(pod.Name, &prev).Stream(ctx); err == nil {
					add(pod.Name, ct.Name+"(previous)", rc)
				}
			}
		}
	}
	if len(lines) == 0 && lastErr != nil {
		return nil, lastErr
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].at.IsZero() || lines[j].at.IsZero() || lines[i].at.Equal(lines[j].at) {
			return lines[i].seq < lines[j].seq
		}
		return lines[i].at.Before(lines[j].at)
	})
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.text)
	}
	return out, nil
}

// crashLooping reports whether a container is waiting in a back-off or was
// last terminated with an error.
func crashLooping(cs *corev1.ContainerStatus) bool {
	if cs.State.Waiting != nil && strings.Contains(cs.State.Waiting.Reason, "BackOff") {
		return true
	}
	if t := cs.LastTerminationState.Terminated; t != nil && (t.ExitCode != 0 || t.Reason == "OOMKilled" || t.Reason == "Error") {
		return true
	}
	return false
}

// splitTimestamp strips the kubelet's leading RFC3339 timestamp from a log
// line and returns it, or a zero time when the line carries none.
func splitTimestamp(line string) (time.Time, string) {
	i := strings.IndexByte(line, ' ')
	if i <= 0 {
		return time.Time{}, line
	}
	t, err := time.Parse(time.RFC3339Nano, line[:i])
	if err != nil {
		return time.Time{}, line
	}
	return t, line[i+1:]
}

// Events returns the Kubernetes events of the last `since` touching an
// object, formatted "HH:MM <Type> <Reason> <Message>" and sorted by time.
// involvedName is an object name, or a label selector such as "app=api", in
// which case the events of every matching pod are returned.
func Events(ctx context.Context, c *Clients, namespace, involvedName string, since time.Duration) ([]string, error) {
	if c == nil || c.Core == nil {
		return nil, fmt.Errorf("no kubernetes client")
	}
	now := time.Now()
	var list *corev1.EventList
	var err error
	if strings.ContainsAny(involvedName, "=!,() ") {
		if _, perr := labels.Parse(involvedName); perr != nil {
			return nil, fmt.Errorf("selector %q: %w", involvedName, perr)
		}
		pods, perr := c.Core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: involvedName})
		if perr != nil {
			return nil, perr
		}
		names := map[string]bool{}
		for _, p := range pods.Items {
			names[p.Name] = true
		}
		all, lerr := c.Core.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		list = &corev1.EventList{}
		for _, e := range all.Items {
			if names[e.InvolvedObject.Name] {
				list.Items = append(list.Items, e)
			}
		}
	} else {
		list, err = c.Core.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("involvedObject.name", involvedName).String(),
		})
		if err != nil {
			return nil, err
		}
	}
	picked := make([]*corev1.Event, 0, len(list.Items))
	for i := range list.Items {
		e := &list.Items[i]
		if since > 0 && !within(eventTime(e), since, now) {
			continue
		}
		picked = append(picked, e)
	}
	sort.SliceStable(picked, func(i, j int) bool { return eventTime(picked[i]).Before(eventTime(picked[j])) })
	out := make([]string, 0, len(picked))
	for _, e := range picked {
		out = append(out, formatEvent(e))
	}
	return out, nil
}
