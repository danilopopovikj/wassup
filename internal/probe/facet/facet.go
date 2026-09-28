// Package facet is the typed contract between a data source and the rest of
// wassup. A background worker fleet is a WorkerPool whether Celery, Hatchet,
// Sidekiq or Temporal runs it; a queue is a Queue whether it lives in Redis,
// RabbitMQ or Hatchet's Postgres; a logical replication consumer is a
// Replication; a scheduled unit of work is a Job. A probe fills the struct
// it knows how to fill, and the facet writes the canonical observation:
// the same metric keys, the same conditions, the same thresholds for
// "stuck", "exhausted" and "broken". The state engine and the UI only ever
// see that canonical form, so adding a backend never touches them.
package facet

import (
	"fmt"
	"sort"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// Task is one unit of work a backend reports as running.
type Task struct {
	ID      string
	Name    string
	Worker  string
	Started time.Time
}

// WorkerPool is what any background worker fleet must report. Zero values
// mean "unknown" and are omitted, never written as 0, except Online and
// Total which are always meaningful once Known is true.
type WorkerPool struct {
	// Known is true when the source answered at all.
	Known bool
	// Online and Total workers (processes, pods, registered workers).
	Online, Total int
	// SlotsUsed and SlotsMax are concurrency: busy slots over all slots.
	// Leave SlotsMax 0 when the backend has no notion of slots.
	SlotsUsed, SlotsMax int
	// Active tasks running now; HasActive says the source could count them.
	Active    int
	HasActive bool
	// Backlog is work waiting for this pool (queued + pending). Set when the
	// backend knows it; the engine uses it to read "all N slots busy, M queued".
	Backlog    int
	HasBacklog bool
	// Running lists running tasks; the facet derives the longest one and the
	// TaskRunning conditions from it.
	Running []Task
	// TypicalDuration is the p95 of recent task durations, when known.
	TypicalDuration time.Duration
	// LongTask is how long a task may run before it counts as stuck.
	// Zero means the default (10 minutes).
	LongTask time.Duration
	// NotReadyDetail replaces the generic "no workers online" wording.
	NotReadyDetail string
	// Since returns a stable first-seen time for a condition key; pass a
	// probe's own tracker so Since does not move on every poll. Optional.
	Since func(key string, now time.Time) time.Time
}

// Queue is what any queue must report.
type Queue struct {
	Known bool
	// Depth is work waiting; Pending is work held back (rate limits,
	// concurrency keys), Running is work taken by consumers.
	Depth, Pending, Running int
	HasPending, HasRunning  bool
	// Consumers attached to the queue.
	Consumers    int
	HasConsumers bool
	// Oldest is the age of the oldest waiting item.
	Oldest    time.Duration
	HasOldest bool
	// Rate is items leaving per second; PublishRate items arriving per second.
	Rate, PublishRate       float64
	HasRate, HasPublishRate bool
	// GrowthPerMin is the depth change over the last minute.
	GrowthPerMin    float64
	HasGrowth       bool
	Running_        []Task // running tasks, when the queue source knows them
	TypicalDuration time.Duration
	LongTask        time.Duration
	Since           func(key string, now time.Time) time.Time
}

// Replication is a logical or physical replication consumer as seen from
// the primary: a replica, a sync engine, a CDC connector.
type Replication struct {
	Known bool
	// Slot or application name the consumer uses.
	Slot string
	// Streaming is true while the consumer is connected and applying.
	Streaming bool
	// Lag in bytes between the primary and the consumer.
	Lag    int64
	HasLag bool
	// WALRetained is what the primary must keep because of this consumer.
	WALRetained    int64
	HasWALRetained bool
	// BrokenSince is when the consumer stopped streaming; zero if unknown.
	BrokenSince time.Time
	Detail      string
	// SlotDetail annotates the inactive slot ("retains 40 GB of WAL").
	SlotDetail string
}

// Job is a scheduled or one-off unit of work: a cron job, a workflow.
type Job struct {
	Known bool
	// Counts over the reporting window.
	Active, Succeeded, Failed, Queued int
	// LastFailure describes the most recent failed run, if the latest
	// finished run failed.
	LastFailure *Failure
	// Running describes the current run, if any.
	Running *Task
	// Schedule in plain words ("every 15 min", "cron 0 * * * *").
	Schedule string
}

// Failure is one failed run.
type Failure struct {
	Ref    string
	At     time.Time
	Reason string
}

// DefaultLongTask is when a running task counts as stuck.
const DefaultLongTask = 10 * time.Minute

// Keys the facets pass to WorkerPool.Since, so a probe's first-seen tracker
// can keep them alive between polls.
const (
	KeyPoolExhausted = "pool_exhausted"
	KeyNoWorkers     = "no_workers"
)

func sinceOf(fn func(string, time.Time) time.Time, key string, now time.Time) time.Time {
	if fn == nil {
		return now
	}
	return fn(key, now)
}

// EmitWorkerPool writes the pool into an observation.
func EmitWorkerPool(o *probe.Observation, p WorkerPool, now time.Time) {
	if !p.Known {
		return
	}
	if o.Metrics == nil {
		o.Metrics = map[string]float64{}
	}
	o.Metrics["workers_online"] = float64(p.Online)
	o.Metrics["workers_total"] = float64(p.Total)
	if p.SlotsMax > 0 {
		o.Metrics["pool_used"] = float64(p.SlotsUsed)
		o.Metrics["pool_max"] = float64(p.SlotsMax)
	}
	if p.HasActive {
		o.Metrics["active"] = float64(p.Active)
	}
	if p.HasBacklog {
		o.Metrics["waiters"] = float64(p.Backlog)
	}
	if p.TypicalDuration > 0 {
		o.Metrics["p95_s"] = p.TypicalDuration.Seconds()
	}
	longTask := p.LongTask
	if longTask <= 0 {
		longTask = DefaultLongTask
	}
	tasks := append([]Task(nil), p.Running...)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Started.Before(tasks[j].Started) })
	if len(tasks) > 0 && !tasks[0].Started.IsZero() {
		o.Metrics["running_s"] = now.Sub(tasks[0].Started).Seconds()
	}
	stuck := 0
	for _, t := range tasks {
		if t.Started.IsZero() || now.Sub(t.Started) < longTask {
			continue
		}
		if stuck >= 5 {
			break
		}
		stuck++
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondTaskRunning, Ref: t.ID, Since: t.Started, Detail: t.Name})
	}
	if p.SlotsMax > 0 && p.SlotsUsed >= p.SlotsMax && p.HasBacklog && p.Backlog > 0 {
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondPoolExhausted, Ref: o.Target,
			Since: sinceOf(p.Since, KeyPoolExhausted, now), Detail: fmt.Sprintf("all %d slots busy", p.SlotsMax)})
	}
	if p.Online == 0 {
		detail := p.NotReadyDetail
		if detail == "" {
			detail = "no workers online"
		}
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondNotReady, Ref: o.Target,
			Since: sinceOf(p.Since, KeyNoWorkers, now), Detail: detail})
	}
}

// EmitQueue writes the queue into an observation.
func EmitQueue(o *probe.Observation, q Queue, now time.Time) {
	if !q.Known {
		return
	}
	if o.Metrics == nil {
		o.Metrics = map[string]float64{}
	}
	o.Metrics["depth"] = float64(q.Depth + q.Pending)
	if q.HasPending {
		o.Metrics["pending"] = float64(q.Pending)
	}
	if q.HasRunning {
		o.Metrics["running"] = float64(q.Running)
		o.Metrics["active"] = float64(q.Running)
	}
	if q.HasConsumers {
		o.Metrics["consumers"] = float64(q.Consumers)
	}
	if q.HasOldest {
		o.Metrics["oldest_age_s"] = q.Oldest.Seconds()
	}
	if q.HasRate {
		o.Metrics["rate"] = q.Rate
	}
	if q.HasPublishRate {
		o.Metrics["publish_rate"] = q.PublishRate
	}
	if q.HasGrowth {
		o.Metrics["growth_per_min"] = q.GrowthPerMin
	}
	if q.TypicalDuration > 0 {
		o.Metrics["p95_s"] = q.TypicalDuration.Seconds()
	}
	longTask := q.LongTask
	if longTask <= 0 {
		longTask = DefaultLongTask
	}
	tasks := append([]Task(nil), q.Running_...)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Started.Before(tasks[j].Started) })
	if len(tasks) > 0 && !tasks[0].Started.IsZero() {
		o.Metrics["running_s"] = now.Sub(tasks[0].Started).Seconds()
	}
	stuck := 0
	for _, t := range tasks {
		if t.Started.IsZero() || now.Sub(t.Started) < longTask || stuck >= 5 {
			continue
		}
		stuck++
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondTaskRunning, Ref: t.ID, Since: t.Started, Detail: t.Name})
	}
}

// EmitReplication writes the consumer's replication state into an
// observation; the same call serves the consumer component and the edge
// from the primary to it.
func EmitReplication(o *probe.Observation, r Replication, now time.Time) {
	if !r.Known {
		return
	}
	if o.Metrics == nil {
		o.Metrics = map[string]float64{}
	}
	if r.Streaming {
		o.Metrics["streaming"] = 1
	} else {
		o.Metrics["streaming"] = 0
	}
	if r.HasLag {
		o.Metrics["lag_bytes"] = float64(r.Lag)
	}
	if r.HasWALRetained {
		o.Metrics["wal_retained_bytes"] = float64(r.WALRetained)
	}
	if !r.Streaming {
		ref := "slot/" + r.Slot
		detail := r.Detail
		if detail == "" {
			detail = "replication slot " + r.Slot + " inactive"
		}
		since := r.BrokenSince
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondReplicationBroken, Ref: ref, Since: since, Detail: detail})
		if r.HasWALRetained {
			o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondSlotInactive, Ref: ref, Since: since, Detail: r.SlotDetail})
		}
	}
}

// EmitJob writes a job into an observation.
func EmitJob(o *probe.Observation, j Job, now time.Time) {
	if !j.Known {
		return
	}
	if o.Metrics == nil {
		o.Metrics = map[string]float64{}
	}
	o.Metrics["active"] = float64(j.Active)
	o.Metrics["succeeded"] = float64(j.Succeeded)
	o.Metrics["failed"] = float64(j.Failed)
	if j.Queued > 0 {
		o.Metrics["queued"] = float64(j.Queued)
	}
	if j.Running != nil {
		if !j.Running.Started.IsZero() {
			o.Metrics["running_s"] = now.Sub(j.Running.Started).Seconds()
		}
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondJobRunning, Ref: j.Running.ID, Since: j.Running.Started, Detail: j.Running.Name})
	}
	if j.LastFailure != nil {
		o.Conditions = append(o.Conditions, model.Condition{Kind: model.CondJobFailed, Ref: j.LastFailure.Ref, Since: j.LastFailure.At, Detail: j.LastFailure.Reason})
	}
	if j.Schedule != "" {
		if o.Detail == nil {
			o.Detail = map[string]any{}
		}
		if _, ok := o.Detail["schedule"]; !ok {
			o.Detail["schedule"] = j.Schedule
		}
	}
}
