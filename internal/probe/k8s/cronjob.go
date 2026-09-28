package k8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	batchlisters "k8s.io/client-go/listers/batch/v1"

	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

const kindCronJob = "k8s.cronjob"

func init() {
	probe.Register(probe.Access{
		Kind:        kindCronJob,
		Source:      "Kubernetes API (cronjob and job informers)",
		Delivers:    "active, succeeded, failed (last 24h), running_s; JobFailed, JobRunning",
		SpecFields:  []string{"namespace (required)", "name (required)", "kubeconfig", "context"},
		Needs:       "get/list/watch on cronjobs and jobs in the namespace",
		Implemented: true,
		Facets:      []string{facet.NameScheduledJob},
		RBAC:        []probe.Rule{probe.Reads("batch", "cronjobs", "jobs")},
	}, func() probe.Probe { return &cronJobProbe{base: base{kind: kindCronJob}} })
}

// jobWindow is the window over which finished jobs are counted.
const jobWindow = 24 * time.Hour

// cronJobProbe is k8s.cronjob.
type cronJobProbe struct {
	base
}

// Kind implements probe.Probe.
func (j *cronJobProbe) Kind() string { return kindCronJob }

// Validate implements probe.Probe.
func (j *cronJobProbe) Validate(spec map[string]any) error {
	return probe.RequireString(spec, "namespace", "name")
}

// Start implements probe.Probe.
func (j *cronJobProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	if err := j.Validate(spec); err != nil {
		j.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	c, err := j.connect(spec)
	if err != nil {
		return err
	}
	ns, name := probe.Str(spec, "namespace", ""), probe.Str(spec, "name", "")
	target := targetOf(spec, name)

	f := c.informers().factory
	cjInf := f.Batch().V1().CronJobs().Informer()
	jobInf := f.Batch().V1().Jobs().Informer()
	cronjobs := f.Batch().V1().CronJobs().Lister()
	jobs := f.Batch().V1().Jobs().Lister()

	kick, notify := kicker()
	watch(ctx, cjInf, func(obj any) bool {
		cj, ok := obj.(*batchv1.CronJob)
		return ok && cj.Namespace == ns && cj.Name == name
	}, notify)
	watch(ctx, jobInf, func(obj any) bool {
		jb, ok := obj.(*batchv1.Job)
		return ok && jb.Namespace == ns && ownedBy(jb.OwnerReferences, "CronJob", name)
	}, notify)

	go func() {
		if err := j.await(ctx, c, out, target, cjInf, jobInf); err != nil {
			return
		}
		runLoop(ctx, tickOf(spec), kick, func() {
			o, err := observeCronJob(cronjobs, jobs, ns, name, target, time.Now())
			if err != nil {
				j.fail(ctx, out, target, err)
				return
			}
			j.emit(ctx, out, o)
		})
	}()
	return nil
}

// ownedBy reports whether an owner reference of the given kind and name exists.
func ownedBy(refs []metav1.OwnerReference, kind, name string) bool {
	for _, r := range refs {
		if r.Kind == kind && r.Name == name {
			return true
		}
	}
	return false
}

// jobCondition returns the job's Complete or Failed condition when true.
func jobCondition(jb *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range jb.Status.Conditions {
		c := &jb.Status.Conditions[i]
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

// observeCronJob reads the cronjob and its jobs once.
func observeCronJob(cronjobs batchlisters.CronJobLister, jobs batchlisters.JobLister, ns, name, target string, now time.Time) (probe.Observation, error) {
	cj, err := cronjobs.CronJobs(ns).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return probe.Observation{}, fmt.Errorf("cronjob %s/%s not found", ns, name)
		}
		return probe.Observation{}, err
	}
	all, err := jobs.Jobs(ns).List(labels.Everything())
	if err != nil {
		return probe.Observation{}, err
	}
	var owned []*batchv1.Job
	for _, jb := range all {
		if ownedBy(jb.OwnerReferences, "CronJob", cj.Name) {
			owned = append(owned, jb)
		}
	}
	sort.Slice(owned, func(i, j int) bool {
		return owned[i].CreationTimestamp.Time.Before(owned[j].CreationTimestamp.Time)
	})

	o := probe.Observation{Target: target, At: now, Metrics: map[string]float64{}, Detail: map[string]any{}}
	active, succeeded, failed := 0, 0, 0
	var oldestActive time.Time
	var lastSuccess, lastFailure *batchv1.Job
	var lastSuccessAt, lastFailureAt time.Time
	for _, jb := range owned {
		if done := jobCondition(jb, batchv1.JobComplete); done != nil {
			at := firstNonZero(metaTime(jb.Status.CompletionTime), done.LastTransitionTime.Time)
			if within(at, jobWindow, now) {
				succeeded++
			}
			if at.After(lastSuccessAt) {
				lastSuccess, lastSuccessAt = jb, at
			}
			continue
		}
		if fail := jobCondition(jb, batchv1.JobFailed); fail != nil {
			at := fail.LastTransitionTime.Time
			if within(at, jobWindow, now) {
				failed++
			}
			if at.After(lastFailureAt) {
				lastFailure, lastFailureAt = jb, at
			}
			continue
		}
		if jb.Status.Active > 0 || (jb.Status.StartTime != nil && jb.Status.CompletionTime == nil) {
			active++
			start := firstNonZero(metaTime(jb.Status.StartTime), jb.CreationTimestamp.Time)
			if oldestActive.IsZero() || start.Before(oldestActive) {
				oldestActive = start
			}
		}
	}
	job := facet.ScheduledJobFacet{Active: facet.NI(active), Succeeded: facet.NI(succeeded), Failed: facet.NI(failed)}
	if !oldestActive.IsZero() {
		job.Running = &facet.Task{ID: "cronjob/" + cj.Name, Name: fmt.Sprintf("%d active", active), Started: oldestActive}
	}
	// JobFailed when the most recent finished job failed.
	if lastFailure != nil && (lastSuccess == nil || lastFailureAt.After(lastSuccessAt)) {
		fail := jobCondition(lastFailure, batchv1.JobFailed)
		detail := lastFailure.Name
		if fail != nil {
			detail = strings.TrimSpace(strings.TrimSpace(fail.Reason) + ": " + strings.TrimSpace(fail.Message))
			detail = strings.TrimSuffix(strings.TrimPrefix(detail, ": "), ":")
		}
		job.LastFailure = &facet.Failure{Ref: "job/" + lastFailure.Name, At: lastFailureAt, Reason: detail}
	}
	facet.EmitScheduledJob(&o, job, now)
	if v, ok := o.Metrics["running_s"]; ok {
		o.Metrics["running_s"] = float64(int(v))
	}

	o.Detail["cron"] = cj.Spec.Schedule
	o.Detail["schedule"] = cronPhrase(cj.Spec.Schedule)
	if cj.Spec.TimeZone != nil && *cj.Spec.TimeZone != "" {
		o.Detail["timezone"] = *cj.Spec.TimeZone
	}
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		o.Detail["suspended"] = true
	}
	if lastSuccess != nil {
		o.Detail["last_success"] = lastSuccess.Name
		o.Detail["last_success_at"] = lastSuccessAt
	}
	if lastFailure != nil {
		o.Detail["last_failure"] = lastFailure.Name
		o.Detail["last_failure_at"] = lastFailureAt
	}
	if t := cj.Status.LastScheduleTime; t != nil {
		o.Detail["last_schedule"] = t.Time
	}
	o.Detail["jobs"] = len(owned)
	return o, nil
}

// cronPhrase renders a cron schedule as a short phrase: "every 15 min",
// "hourly at :30", "daily at 03:00", "weekdays at 09:00", "weekly on
// Monday at 03:00", "monthly on day 1 at 03:00". Schedules it cannot phrase
// come back unchanged.
func cronPhrase(spec string) string {
	s := strings.TrimSpace(spec)
	tz := ""
	if strings.HasPrefix(s, "CRON_TZ=") || strings.HasPrefix(s, "TZ=") {
		parts := strings.SplitN(s, " ", 2)
		if len(parts) == 2 {
			tz = " " + strings.TrimPrefix(strings.TrimPrefix(parts[0], "CRON_TZ="), "TZ=")
			s = strings.TrimSpace(parts[1])
		}
	}
	switch strings.ToLower(s) {
	case "@hourly":
		return "hourly"
	case "@daily", "@midnight":
		return "daily at 00:00" + tz
	case "@weekly":
		return "weekly on Sunday at 00:00" + tz
	case "@monthly":
		return "monthly on day 1 at 00:00" + tz
	case "@yearly", "@annually":
		return "yearly on 1 January at 00:00" + tz
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return spec
	}
	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]
	if month != "*" {
		return spec
	}
	// Sub-hourly: every N minutes.
	if strings.HasPrefix(minute, "*/") && hour == "*" && dom == "*" && dow == "*" {
		if n, err := strconv.Atoi(minute[2:]); err == nil && n > 0 {
			if n == 1 {
				return "every minute"
			}
			return fmt.Sprintf("every %d min", n)
		}
	}
	if minute == "*" && hour == "*" && dom == "*" && dow == "*" {
		return "every minute"
	}
	m, okM := atoi(minute)
	if !okM {
		return spec
	}
	// Every N hours.
	if strings.HasPrefix(hour, "*/") && dom == "*" && dow == "*" {
		if n, err := strconv.Atoi(hour[2:]); err == nil && n > 0 {
			at := ""
			if m != 0 {
				at = fmt.Sprintf(" at :%02d", m)
			}
			if n == 1 {
				return "hourly" + at
			}
			return fmt.Sprintf("every %d h%s", n, at)
		}
	}
	if hour == "*" && dom == "*" && dow == "*" {
		if m == 0 {
			return "hourly"
		}
		return fmt.Sprintf("hourly at :%02d", m)
	}
	h, okH := atoi(hour)
	if !okH {
		return spec
	}
	at := fmt.Sprintf("%02d:%02d", h, m) + tz
	switch {
	case dom == "*" && dow == "*":
		return "daily at " + at
	case dom == "*" && (dow == "1-5" || dow == "MON-FRI" || dow == "mon-fri"):
		return "weekdays at " + at
	case dom == "*" && (dow == "0,6" || dow == "6,0" || dow == "SAT,SUN" || dow == "sat,sun"):
		return "weekends at " + at
	case dom == "*":
		if d, ok := weekday(dow); ok {
			return fmt.Sprintf("weekly on %s at %s", d, at)
		}
	case dow == "*":
		if d, ok := atoi(dom); ok {
			return fmt.Sprintf("monthly on day %d at %s", d, at)
		}
	}
	return spec
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// weekday names a single cron day-of-week field.
func weekday(s string) (string, bool) {
	names := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	if n, ok := atoi(s); ok && n >= 0 && n <= 7 {
		return names[n%7], true
	}
	for i, n := range names {
		if strings.EqualFold(s, n[:3]) {
			return names[i], true
		}
	}
	return "", false
}
