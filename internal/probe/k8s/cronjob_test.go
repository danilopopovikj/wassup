package k8s

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/danilopopovikj/wassup/internal/model"
)

func cronJobObj(name, schedule string) *batchv1.CronJob {
	return &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", UID: types.UID("cj-" + name)},
		Spec: batchv1.CronJobSpec{Schedule: schedule}}
}

func jobObj(name, owner string, created time.Time, cond *batchv1.JobCondition, active int32) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", CreationTimestamp: metav1.Time{Time: created},
		OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: owner}}},
		Status: batchv1.JobStatus{Active: active, StartTime: &metav1.Time{Time: created}}}
	if cond != nil {
		j.Status.Conditions = []batchv1.JobCondition{*cond}
		if cond.Type == batchv1.JobComplete {
			j.Status.CompletionTime = &cond.LastTransitionTime
		}
	}
	return j
}

func TestCronJobFailed(t *testing.T) {
	now := time.Now()
	failedAt := now.Add(-30 * time.Minute)
	objs := []runtime.Object{
		cronJobObj("docs-sync", "*/15 * * * *"),
		jobObj("docs-sync-1", "docs-sync", now.Add(-2*time.Hour), &batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: now.Add(-2 * time.Hour)}}, 0),
		jobObj("docs-sync-2", "docs-sync", now.Add(-40*time.Minute), &batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit", LastTransitionTime: metav1.Time{Time: failedAt}}, 0),
		jobObj("docs-sync-old", "docs-sync", now.Add(-30*time.Hour), &batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: now.Add(-30 * time.Hour)}}, 0),
		jobObj("other-1", "other", now.Add(-time.Minute), nil, 1),
	}
	c := newTestClients(objs, nil, nil)
	p := &cronJobProbe{base: base{kind: kindCronJob, clients: c}}
	out, _ := startProbe(t, p, testSpec("docs-sync", "namespace", "prod", "name", "docs-sync"))
	o := firstOK(t, out)
	if o.Metrics["active"] != 0 || o.Metrics["succeeded"] != 1 || o.Metrics["failed"] != 1 {
		t.Errorf("metrics = %v", o.Metrics)
	}
	if _, ok := o.Metrics["running_s"]; ok {
		t.Errorf("running_s must be omitted without active jobs")
	}
	jf, ok := model.HasCondition(o.Conditions, model.CondJobFailed)
	if !ok || jf.Ref != "job/docs-sync-2" || !jf.Since.Equal(failedAt) || jf.Detail != "BackoffLimitExceeded: Job has reached the specified backoff limit" {
		t.Errorf("JobFailed = %+v (ok %v)", jf, ok)
	}
	if o.Detail["schedule"] != "every 15 min" || o.Detail["cron"] != "*/15 * * * *" {
		t.Errorf("schedule detail = %v", o.Detail)
	}
	if o.Detail["last_success"] != "docs-sync-1" || o.Detail["last_failure"] != "docs-sync-2" {
		t.Errorf("last_* detail = %v", o.Detail)
	}
}

func TestCronJobRunning(t *testing.T) {
	started := time.Now().Add(-90 * time.Second)
	objs := []runtime.Object{
		cronJobObj("backup", "0 3 * * *"),
		jobObj("backup-1", "backup", started, nil, 1),
	}
	c := newTestClients(objs, nil, nil)
	p := &cronJobProbe{base: base{kind: kindCronJob, clients: c}}
	out, _ := startProbe(t, p, testSpec("backup", "namespace", "prod", "name", "backup"))
	o := firstOK(t, out)
	if o.Metrics["active"] != 1 || o.Metrics["running_s"] < 89 || o.Metrics["running_s"] > 100 {
		t.Errorf("metrics = %v", o.Metrics)
	}
	jr, ok := model.HasCondition(o.Conditions, model.CondJobRunning)
	if !ok || !jr.Since.Equal(started) {
		t.Errorf("JobRunning = %+v (ok %v)", jr, ok)
	}
	if o.Detail["schedule"] != "daily at 03:00" {
		t.Errorf("schedule = %v", o.Detail["schedule"])
	}
}

func TestCronPhrase(t *testing.T) {
	cases := map[string]string{
		"*/15 * * * *":                      "every 15 min",
		"*/1 * * * *":                       "every minute",
		"* * * * *":                         "every minute",
		"0 * * * *":                         "hourly",
		"30 * * * *":                        "hourly at :30",
		"0 */6 * * *":                       "every 6 h",
		"15 */2 * * *":                      "every 2 h at :15",
		"0 3 * * *":                         "daily at 03:00",
		"45 23 * * *":                       "daily at 23:45",
		"0 9 * * 1-5":                       "weekdays at 09:00",
		"0 3 * * 1":                         "weekly on Monday at 03:00",
		"0 3 * * MON":                       "weekly on Monday at 03:00",
		"0 3 1 * *":                         "monthly on day 1 at 03:00",
		"@hourly":                           "hourly",
		"@daily":                            "daily at 00:00",
		"@weekly":                           "weekly on Sunday at 00:00",
		"CRON_TZ=Europe/Belgrade 0 3 * * *": "daily at 03:00 Europe/Belgrade",
		"0 3 * 6 *":                         "0 3 * 6 *",
		"1,31 * * * *":                      "1,31 * * * *",
		"garbage":                           "garbage",
	}
	for in, want := range cases {
		if got := cronPhrase(in); got != want {
			t.Errorf("cronPhrase(%q) = %q, want %q", in, got, want)
		}
	}
}
