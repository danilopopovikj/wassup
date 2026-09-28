package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

const kindWorkload = "k8s.workload"

func init() {
	probe.Register(probe.Access{
		Kind:     kindWorkload,
		Source:   "Kubernetes API (informers on pods, deployments, statefulsets, daemonsets, replicasets, events) and metrics.k8s.io PodMetrics",
		Delivers: "replicas_ready, replicas_desired, restarts, restart_window_s, cpu_pct, mem_pct, killed, evicted; CrashLoopBackOff, ImagePullBackOff, OOMKilled, Evicted; deploy and scale events",
		SpecFields: []string{
			"namespace (required)",
			"selector (label selector; required unless name and kind are set)",
			"name",
			"kind (Deployment|StatefulSet|DaemonSet)",
			"kubeconfig",
			"context",
		},
		Needs:       "get/list/watch on pods, replicasets, deployments, statefulsets, daemonsets and events in the namespace; get/list on pods.metrics.k8s.io",
		Implemented: true,
		Facets:      []string{facet.NameWorkload},
	}, func() probe.Probe { return &workloadProbe{base: base{kind: kindWorkload}} })
}

// workloadKinds are the accepted values of the kind spec field.
var workloadKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}

// restartEvent is one container restart the probe has seen or inferred.
type restartEvent struct {
	at    time.Time
	pod   string
	oom   bool
	limit string
}

// containerSeen is the last restart count observed for one container.
type containerSeen struct {
	count int32
	at    time.Time
}

// workloadProbe is k8s.workload.
type workloadProbe struct {
	base
	seen        map[string]containerSeen
	restarts    []restartEvent
	lastImage   string
	lastDesired float64
	haveDesired bool
	firstTick   bool
	emitted     map[string]bool
}

// Kind implements probe.Probe.
func (w *workloadProbe) Kind() string { return kindWorkload }

// Validate implements probe.Probe.
func (w *workloadProbe) Validate(spec map[string]any) error {
	if err := probe.RequireString(spec, "namespace"); err != nil {
		return err
	}
	name, kind := probe.Str(spec, "name", ""), probe.Str(spec, "kind", "")
	sel := probe.Str(spec, "selector", "")
	if sel == "" && (name == "" || kind == "") {
		return fmt.Errorf("\"selector\" is required unless both \"name\" and \"kind\" are set")
	}
	if kind != "" && !workloadKinds[kind] {
		return fmt.Errorf("\"kind\" must be Deployment, StatefulSet or DaemonSet, got %q", kind)
	}
	if sel != "" {
		if _, err := labels.Parse(sel); err != nil {
			return fmt.Errorf("\"selector\": %w", err)
		}
	}
	return nil
}

// workloadListers are the informer stores the probe reads.
type workloadListers struct {
	pods         corelisters.PodLister
	deployments  appslisters.DeploymentLister
	statefulsets appslisters.StatefulSetLister
	daemonsets   appslisters.DaemonSetLister
	replicasets  appslisters.ReplicaSetLister
	events       corelisters.EventLister
}

// Start implements probe.Probe.
func (w *workloadProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := w.Validate(spec); err != nil {
		w.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c, err := w.connect(spec)
	if err != nil {
		return err
	}
	ns := probe.Str(spec, "namespace", "")
	name, kind := probe.Str(spec, "name", ""), probe.Str(spec, "kind", "")
	target := targetOf(spec, firstNonEmpty(name, ns))
	sel := labels.Everything()
	if s := probe.Str(spec, "selector", ""); s != "" {
		sel, _ = labels.Parse(s)
	}
	w.seen = map[string]containerSeen{}
	w.emitted = map[string]bool{}
	w.firstTick = true

	inf := c.informers()
	f := inf.factory
	podInf := inf.pods
	depInf := f.Apps().V1().Deployments().Informer()
	stsInf := f.Apps().V1().StatefulSets().Informer()
	dsInf := f.Apps().V1().DaemonSets().Informer()
	rsInf := f.Apps().V1().ReplicaSets().Informer()
	evInf := f.Core().V1().Events().Informer()
	ls := workloadListers{
		pods:         f.Core().V1().Pods().Lister(),
		deployments:  f.Apps().V1().Deployments().Lister(),
		statefulsets: f.Apps().V1().StatefulSets().Lister(),
		daemonsets:   f.Apps().V1().DaemonSets().Lister(),
		replicasets:  f.Apps().V1().ReplicaSets().Lister(),
		events:       f.Core().V1().Events().Lister(),
	}

	kick, notify := kicker()
	watch(ctx, podInf, func(obj any) bool {
		p, ok := obj.(*corev1.Pod)
		if !ok || p.Namespace != ns {
			return false
		}
		if name != "" && kind != "" {
			return true // the owner's selector is only known at read time
		}
		return sel.Matches(labels.Set(p.Labels))
	}, notify)
	ownerMatch := func(obj any) bool {
		m, ok := obj.(metav1.Object)
		return ok && m.GetNamespace() == ns && (name == "" || m.GetName() == name)
	}
	watch(ctx, depInf, ownerMatch, notify)
	watch(ctx, stsInf, ownerMatch, notify)
	watch(ctx, dsInf, ownerMatch, notify)

	go func() {
		if err := w.syncOrFail(ctx, c, podInf, depInf, stsInf, dsInf, rsInf, evInf); err != nil {
			probe.Send(ctx, out, probe.Observation{Target: target, Probe: kindWorkload, At: time.Now(), Err: err.Error()})
			return
		}
		runLoop(ctx, tickOf(spec), kick, func() {
			o, err := w.observe(ctx, c, ls, ns, name, kind, sel, target)
			if err != nil {
				w.fail(ctx, out, target, err)
				return
			}
			w.emit(ctx, out, o)
		})
	}()
	return nil
}

// workloadTarget is what the probe resolved the spec to.
type workloadTarget struct {
	kind, name string
	selector   labels.Selector
	image      string
	desired    float64
	ready      float64
	haveStatus bool
	obj        metav1.Object
	template   *corev1.PodTemplateSpec
}

// resolve finds the workload object and its pod selector.
func resolve(ls workloadListers, ns, name, kind string, sel labels.Selector) (*workloadTarget, error) {
	if name != "" && kind != "" {
		t, err := lookup(ls, ns, name, kind)
		if err != nil {
			return nil, err
		}
		return t, nil
	}
	// Selector only: find the owner whose selector picks the same pods.
	pods, err := ls.pods.Pods(ns).List(sel)
	if err != nil {
		return nil, err
	}
	covers := func(s *metav1.LabelSelector, tmpl map[string]string) bool {
		ls, err := metav1.LabelSelectorAsSelector(s)
		if err != nil || ls.Empty() {
			return false
		}
		if len(pods) == 0 {
			return sel.Matches(labels.Set(tmpl))
		}
		for _, p := range pods {
			if !ls.Matches(labels.Set(p.Labels)) {
				return false
			}
		}
		return true
	}
	if deps, err := ls.deployments.Deployments(ns).List(labels.Everything()); err == nil {
		sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
		for _, d := range deps {
			if covers(d.Spec.Selector, d.Spec.Template.Labels) {
				return fromDeployment(d), nil
			}
		}
	}
	if sts, err := ls.statefulsets.StatefulSets(ns).List(labels.Everything()); err == nil {
		sort.Slice(sts, func(i, j int) bool { return sts[i].Name < sts[j].Name })
		for _, s := range sts {
			if covers(s.Spec.Selector, s.Spec.Template.Labels) {
				return fromStatefulSet(s), nil
			}
		}
	}
	if dss, err := ls.daemonsets.DaemonSets(ns).List(labels.Everything()); err == nil {
		sort.Slice(dss, func(i, j int) bool { return dss[i].Name < dss[j].Name })
		for _, d := range dss {
			if covers(d.Spec.Selector, d.Spec.Template.Labels) {
				return fromDaemonSet(d), nil
			}
		}
	}
	// No owner: report the bare pods.
	t := &workloadTarget{kind: "Pods", name: sel.String(), selector: sel}
	if len(pods) > 0 {
		t.image = firstImage(pods[0].Spec.Containers)
	}
	return t, nil
}

// lookup fetches a named workload from the listers.
func lookup(ls workloadListers, ns, name, kind string) (*workloadTarget, error) {
	switch kind {
	case "Deployment":
		d, err := ls.deployments.Deployments(ns).Get(name)
		if err != nil {
			return nil, describeNotFound(err, kind, ns, name)
		}
		return fromDeployment(d), nil
	case "StatefulSet":
		s, err := ls.statefulsets.StatefulSets(ns).Get(name)
		if err != nil {
			return nil, describeNotFound(err, kind, ns, name)
		}
		return fromStatefulSet(s), nil
	case "DaemonSet":
		d, err := ls.daemonsets.DaemonSets(ns).Get(name)
		if err != nil {
			return nil, describeNotFound(err, kind, ns, name)
		}
		return fromDaemonSet(d), nil
	}
	return nil, fmt.Errorf("unsupported kind %q", kind)
}

func describeNotFound(err error, kind, ns, name string) error {
	if errors.IsNotFound(err) {
		return fmt.Errorf("%s %s/%s not found", strings.ToLower(kind), ns, name)
	}
	return err
}

func fromDeployment(d *appsv1.Deployment) *workloadTarget {
	sel, _ := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	desired := float64(1)
	if d.Spec.Replicas != nil {
		desired = float64(*d.Spec.Replicas)
	}
	return &workloadTarget{kind: "Deployment", name: d.Name, selector: orEverything(sel), image: firstImage(d.Spec.Template.Spec.Containers),
		desired: desired, ready: float64(d.Status.ReadyReplicas), haveStatus: true, obj: d, template: &d.Spec.Template}
}

func fromStatefulSet(s *appsv1.StatefulSet) *workloadTarget {
	sel, _ := metav1.LabelSelectorAsSelector(s.Spec.Selector)
	desired := float64(1)
	if s.Spec.Replicas != nil {
		desired = float64(*s.Spec.Replicas)
	}
	return &workloadTarget{kind: "StatefulSet", name: s.Name, selector: orEverything(sel), image: firstImage(s.Spec.Template.Spec.Containers),
		desired: desired, ready: float64(s.Status.ReadyReplicas), haveStatus: true, obj: s, template: &s.Spec.Template}
}

func fromDaemonSet(d *appsv1.DaemonSet) *workloadTarget {
	sel, _ := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	return &workloadTarget{kind: "DaemonSet", name: d.Name, selector: orEverything(sel), image: firstImage(d.Spec.Template.Spec.Containers),
		desired: float64(d.Status.DesiredNumberScheduled), ready: float64(d.Status.NumberReady), haveStatus: true, obj: d, template: &d.Spec.Template}
}

func orEverything(s labels.Selector) labels.Selector {
	if s == nil {
		return labels.Everything()
	}
	return s
}

func firstImage(cs []corev1.Container) string {
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Image
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// observe reads the stores and metrics once and builds the observation.
func (w *workloadProbe) observe(ctx context.Context, c *Clients, ls workloadListers, ns, name, kind string, sel labels.Selector, target string) (probe.Observation, error) {
	now := time.Now()
	t, err := resolve(ls, ns, name, kind, sel)
	if err != nil {
		return probe.Observation{}, err
	}
	pods, err := ls.pods.Pods(ns).List(t.selector)
	if err != nil {
		return probe.Observation{}, err
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })

	o := probe.Observation{Target: target, At: now, Metrics: map[string]float64{}, Detail: map[string]any{}}
	// The facet writes the canonical metrics and conditions. Ready stays
	// true: the pods' own state is what the instance lists carry.
	wl := facet.WorkloadFacet{Ready: true, RestartWindow: restartWindow}
	var readyPods []string
	restartCounts := map[string]any{}
	evicted := 0
	ready := 0
	for _, p := range pods {
		if podReady(p) {
			ready++
			readyPods = append(readyPods, p.Name)
		}
		var total int32
		for _, cs := range allContainerStatuses(p) {
			total += cs.RestartCount
			w.trackRestarts(p, cs, now)
			containerInstances(&wl, p, cs, now)
		}
		restartCounts[p.Name] = total
		if p.Status.Reason == "Evicted" || p.Status.Phase == corev1.PodFailed && strings.Contains(p.Status.Message, "evict") {
			evicted++
			wl.EvictedInstances = append(wl.EvictedInstances, facet.Instance{Ref: "pod/" + p.Name, Since: evictedAt(p), Detail: p.Status.Message})
		}
	}
	w.pruneRestarts(pods, now)

	desired, readyN := t.desired, t.ready
	if !t.haveStatus {
		desired, readyN = float64(len(pods)), float64(ready)
	}
	wl.ReplicasReady, wl.ReplicasDesired = facet.N(readyN), facet.N(desired)
	wl.Restarts = facet.NI(len(w.restarts))
	killed := 0
	for _, r := range w.restarts {
		if r.oom {
			killed++
		}
	}
	if killed > 0 {
		wl.Killed = facet.NI(killed)
	}
	if evicted > 0 {
		wl.Evicted = facet.NI(evicted)
	}

	if metrics, err := podMetricsByName(ctx, c, ns); err == nil {
		cpu, mem, haveCPU, haveMem := podUsage(pods, metrics)
		if haveCPU {
			wl.CPUPct = facet.N(cpu)
		}
		if haveMem {
			wl.MemPct = facet.N(mem)
		}
	} else {
		o.Detail["metrics_error"] = err.Error()
	}
	facet.EmitWorkload(&o, wl, now)

	// Change markers.
	label := firstNonEmpty(t.name, target)
	if t.image != "" {
		if w.lastImage != "" && w.lastImage != t.image {
			w.addEvent(&o, model.Event{At: now, Kind: "deploy", Target: target, Summary: fmt.Sprintf("deploy of %s %s", target, imageTag(t.image)), Ref: imageTag(t.image)})
		} else if w.firstTick {
			if at := recentRollout(ls, t, now); !at.IsZero() {
				w.addEvent(&o, model.Event{At: at, Kind: "deploy", Target: target, Summary: fmt.Sprintf("deploy of %s %s", target, imageTag(t.image)), Ref: imageTag(t.image)})
			}
		}
		w.lastImage = t.image
	}
	if w.haveDesired && w.lastDesired != desired {
		w.addEvent(&o, model.Event{At: now, Kind: "scale", Target: target, Summary: fmt.Sprintf("scale of %s to %d", target, int(desired))})
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondScaled, Ref: t.kind + "/" + label, Since: now, Detail: fmt.Sprintf("%d -> %d", int(w.lastDesired), int(desired))})
	}
	w.lastDesired, w.haveDesired = desired, true
	w.firstTick = false

	o.Conditions = dedupeConditions(o.Conditions)
	o.Detail["kind"] = t.kind
	o.Detail["name"] = t.name
	o.Detail["namespace"] = ns
	o.Detail["selector"] = t.selector.String()
	if t.image != "" {
		o.Detail["image"] = t.image
	}
	o.Detail["pods"] = len(pods)
	o.Detail["ready_pods"] = readyPods
	o.Detail["restart_counts"] = restartCounts
	if ev := recentWarnings(ls.events, ns, pods, t, now); len(ev) > 0 {
		o.Detail["recent_events"] = ev
	}
	return o, nil
}

// addEvent appends an event once per (kind, summary, minute).
func (w *workloadProbe) addEvent(o *probe.Observation, e model.Event) {
	key := e.Kind + "|" + e.Summary + "|" + e.At.Truncate(time.Minute).Format(time.RFC3339)
	if w.emitted[key] {
		return
	}
	w.emitted[key] = true
	o.Events = append(o.Events, e)
}

// trackRestarts updates the per-container restart bookkeeping. The first
// sighting of a container seeds one restart when its last termination is
// inside the window; later sightings add the restartCount delta.
func (w *workloadProbe) trackRestarts(p *corev1.Pod, cs corev1.ContainerStatus, now time.Time) {
	key := p.Namespace + "/" + p.Name + "/" + cs.Name
	term := cs.LastTerminationState.Terminated
	if term == nil && cs.State.Terminated != nil {
		term = cs.State.Terminated
	}
	prev, ok := w.seen[key]
	switch {
	case !ok:
		if term != nil && within(term.FinishedAt.Time, restartWindow, now) && cs.RestartCount > 0 {
			w.restarts = append(w.restarts, restartEvent{at: term.FinishedAt.Time, pod: p.Name, oom: term.Reason == "OOMKilled", limit: memLimit(containerSpec(p, cs.Name))})
		}
	case cs.RestartCount > prev.count:
		n := int(cs.RestartCount - prev.count)
		at := now
		if term != nil && !term.FinishedAt.IsZero() && term.FinishedAt.Time.After(prev.at) {
			at = term.FinishedAt.Time
		}
		for i := 0; i < n; i++ {
			w.restarts = append(w.restarts, restartEvent{at: at, pod: p.Name, oom: i == 0 && term != nil && term.Reason == "OOMKilled", limit: memLimit(containerSpec(p, cs.Name))})
		}
	}
	w.seen[key] = containerSeen{count: cs.RestartCount, at: now}
}

// pruneRestarts drops restarts outside the window and forgets pods that are
// gone.
func (w *workloadProbe) pruneRestarts(pods []*corev1.Pod, now time.Time) {
	kept := w.restarts[:0]
	for _, r := range w.restarts {
		if within(r.at, restartWindow, now) {
			kept = append(kept, r)
		}
	}
	w.restarts = kept
	live := map[string]bool{}
	for _, p := range pods {
		live[p.Namespace+"/"+p.Name] = true
	}
	for key := range w.seen {
		if i := strings.LastIndex(key, "/"); i > 0 && !live[key[:i]] {
			delete(w.seen, key)
		}
	}
}

// containerInstances adds one container status to the facet's instance
// lists: crash looping, unable to pull its image, killed for memory.
func containerInstances(wl *facet.WorkloadFacet, p *corev1.Pod, cs corev1.ContainerStatus, now time.Time) {
	ref := "pod/" + p.Name
	term := cs.LastTerminationState.Terminated
	if w := cs.State.Waiting; w != nil {
		since := podStart(p)
		if term != nil && !term.FinishedAt.IsZero() {
			since = term.FinishedAt.Time
		}
		switch w.Reason {
		case "CrashLoopBackOff":
			wl.Crashing = append(wl.Crashing, facet.Instance{Ref: ref, Since: since, Detail: w.Message})
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
			wl.ImagePull = append(wl.ImagePull, facet.Instance{Ref: ref, Since: since, Detail: w.Message})
		}
	}
	if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
		term = t
	}
	if term != nil && term.Reason == "OOMKilled" && within(term.FinishedAt.Time, restartWindow, now) {
		detail := "limit " + memLimit(containerSpec(p, cs.Name))
		if strings.TrimSpace(detail) == "limit" {
			detail = "no memory limit"
		}
		wl.OutOfMemory = append(wl.OutOfMemory, facet.Instance{Ref: ref, Since: term.FinishedAt.Time, Detail: detail})
	}
}

// evictedAt is the best-known time of an eviction.
func evictedAt(p *corev1.Pod) time.Time {
	var latest time.Time
	for _, c := range p.Status.Conditions {
		if c.LastTransitionTime.Time.After(latest) {
			latest = c.LastTransitionTime.Time
		}
	}
	return firstNonZero(latest, podStart(p))
}

// dedupeConditions keeps the first of each (kind, ref).
func dedupeConditions(conds []model.Condition) []model.Condition {
	if len(conds) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]model.Condition, 0, len(conds))
	for _, c := range conds {
		k := c.Kind + "|" + c.Ref
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	return out
}

// recentRollout returns when the current pod template of a Deployment was
// created, when that is within the lookback; it lets a probe that starts
// after a deploy still place the marker. Other kinds fall back to the
// object's creation time when it is recent.
func recentRollout(ls workloadListers, t *workloadTarget, now time.Time) time.Time {
	if t.obj == nil {
		return time.Time{}
	}
	if d, ok := t.obj.(*appsv1.Deployment); ok && ls.replicasets != nil {
		rss, err := ls.replicasets.ReplicaSets(d.Namespace).List(t.selector)
		if err == nil {
			rev := d.Annotations["deployment.kubernetes.io/revision"]
			var newest time.Time
			for _, rs := range rss {
				owned := false
				for _, o := range rs.OwnerReferences {
					if o.UID == d.UID {
						owned = true
					}
				}
				if !owned {
					continue
				}
				if rev != "" && rs.Annotations["deployment.kubernetes.io/revision"] == rev {
					newest = rs.CreationTimestamp.Time
					break
				}
				if rs.CreationTimestamp.Time.After(newest) {
					newest = rs.CreationTimestamp.Time
				}
			}
			if within(newest, lookback, now) {
				return newest
			}
			return time.Time{}
		}
	}
	if created := t.obj.GetCreationTimestamp().Time; within(created, lookback, now) {
		return created
	}
	return time.Time{}
}

// recentWarnings lists the last warning events touching the workload's pods
// or the workload itself within the last 30 minutes.
func recentWarnings(ev corelisters.EventLister, ns string, pods []*corev1.Pod, t *workloadTarget, now time.Time) []string {
	if ev == nil {
		return nil
	}
	list, err := ev.Events(ns).List(labels.Everything())
	if err != nil {
		return nil
	}
	names := map[string]bool{}
	for _, p := range pods {
		names[p.Name] = true
	}
	if t.name != "" {
		names[t.name] = true
	}
	var picked []*corev1.Event
	for _, e := range list {
		if e.Type != corev1.EventTypeWarning || !names[e.InvolvedObject.Name] {
			continue
		}
		if !within(eventTime(e), 30*time.Minute, now) {
			continue
		}
		picked = append(picked, e)
	}
	sort.Slice(picked, func(i, j int) bool { return eventTime(picked[i]).Before(eventTime(picked[j])) })
	if len(picked) > 10 {
		picked = picked[len(picked)-10:]
	}
	out := make([]string, 0, len(picked))
	for _, e := range picked {
		out = append(out, formatEvent(e))
	}
	return out
}

// eventTime is the most recent time an event carries.
func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	if !e.FirstTimestamp.IsZero() {
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

// formatEvent renders an event as "HH:MM <Type> <Reason> <Message>".
func formatEvent(e *corev1.Event) string {
	return fmt.Sprintf("%s %s %s %s", eventTime(e).Local().Format("15:04"), e.Type, e.Reason, strings.TrimSpace(e.Message))
}
