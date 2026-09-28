// Package facet is the typed contract between a data source and the rest of
// wassup. Every node type on the diagram has one facet named after it
// (node ↔ NodeFacet, backgroundworker ↔ BackgroundWorkerFacet, syncengine ↔ SyncEngineFacet)
// and every edge kind has one (TrafficFacet, ReplicationFacet). A probe
// reads its provider (Kubernetes, Hetzner, Postgres, Hatchet, Flower, a
// health endpoint) and fills the facet; the facet writes the canonical
// observation: the same metric keys, the same conditions, the same rules
// for "stuck", "exhausted", "expired" and "broken". The state engine, the
// labels and the UI only ever see that canonical form, so a new provider is
// a set of probes that fill the same structs and nothing else changes.
//
// Numbers a provider may not know are Num values: leave them unset and the
// key is omitted rather than written as zero.
package facet

import (
	"fmt"
	"sort"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// Num is an optional number: Set says whether the provider knew it.
type Num struct {
	V   float64
	Set bool
}

// N makes a set Num.
func N(v float64) Num { return Num{V: v, Set: true} }

// NI makes a set Num from an int.
func NI(v int) Num { return Num{V: float64(v), Set: true} }

// Names of every facet, matching the catalog type they serve.
const (
	NameNode             = "NodeFacet"
	NameWorkload         = "WorkloadFacet"
	NameBackgroundWorker = "BackgroundWorkerFacet"
	NameQueue            = "QueueFacet"
	NameScheduledJob     = "ScheduledJobFacet"
	NameLoadBalancer     = "LoadBalancerFacet"
	NameIngress          = "IngressFacet"
	NameCertificate      = "CertificateFacet"
	NameFirewall         = "FirewallFacet"
	NameDNS              = "DNSFacet"
	NameDatabase         = "DatabaseFacet"
	NameSyncEngine       = "SyncEngineFacet"
	NameCache            = "CacheFacet"
	NameStorage          = "StorageFacet"
	NameObservability    = "ObservabilityFacet"
	NameExternal         = "ExternalFacet"
	NameTraffic          = "TrafficFacet"
	NameReplication      = "ReplicationFacet"
)

// DefaultLongTask is when a running task counts as stuck.
const DefaultLongTask = 10 * time.Minute

// Keys the facets pass to a Since tracker, so a probe's first-seen tracker
// keeps a condition's start time stable between polls.
const (
	KeyPoolExhausted = "pool_exhausted"
	KeyNoWorkers     = "no_workers"
	KeyNotReady      = "not_ready"
	KeyNoData        = "no_data"
	KeyTimeout       = "timeout"
	KeyDenied        = "denied"
	KeyUnhealthy     = "unhealthy"
)

// SinceFunc returns a stable first-seen time for a key. Probes pass their
// own tracker; nil means "now".
type SinceFunc func(key string, now time.Time) time.Time

func sinceOf(fn SinceFunc, key string, now time.Time) time.Time {
	if fn == nil {
		return now
	}
	return fn(key, now)
}

func metrics(o *probe.Observation) map[string]float64 {
	if o.Metrics == nil {
		o.Metrics = map[string]float64{}
	}
	return o.Metrics
}

func put(o *probe.Observation, key string, n Num) {
	if n.Set {
		metrics(o)[key] = n.V
	}
}

func cond(o *probe.Observation, kind, ref string, since time.Time, detail string) {
	o.Conditions = append(o.Conditions, model.Condition{Kind: kind, Ref: ref, Since: since, Detail: detail})
}

// Task is one unit of work a provider reports as running.
type Task struct {
	ID      string
	Name    string
	Worker  string
	Started time.Time
}

// Instance is one pod, container or process in trouble.
type Instance struct {
	Ref    string // "pod/api-7d9f"
	Since  time.Time
	Detail string
}

// Failure is one failed run or attempt.
type Failure struct {
	Ref    string
	At     time.Time
	Reason string
}

// ---------------------------------------------------------------- node

// NodeFacet is a machine: a Kubernetes node, a VM, a bare metal host.
type NodeFacet struct {
	CPUPct, MemPct, DiskPct Num
	Pods                    Num // workloads or containers placed on it
	Killed                  Num // processes killed for memory in the window
	Ready                   bool
	NotReadySince           time.Time
	NotReadyDetail          string
	MemoryPressure          bool
	DiskPressure            bool
	PressureSince           time.Time
	// BootedAt, when within RebootWindow of now, marks the node as rebooted.
	BootedAt     time.Time
	RebootWindow time.Duration // default 2h
}

// EmitNode writes the node into an observation.
func EmitNode(o *probe.Observation, n NodeFacet, now time.Time) {
	put(o, "cpu_pct", n.CPUPct)
	put(o, "mem_pct", n.MemPct)
	put(o, "disk_pct", n.DiskPct)
	put(o, "pods", n.Pods)
	put(o, "killed", n.Killed)
	if !n.Ready {
		cond(o, model.CondNotReady, "node/"+o.Target, n.NotReadySince, n.NotReadyDetail)
	}
	if n.MemoryPressure {
		cond(o, model.CondMemoryPressure, "node/"+o.Target, n.PressureSince, "")
	}
	if n.DiskPressure {
		cond(o, model.CondDiskPressure, "node/"+o.Target, n.PressureSince, "")
	}
	win := n.RebootWindow
	if win <= 0 {
		win = 2 * time.Hour
	}
	if !n.BootedAt.IsZero() && now.Sub(n.BootedAt) <= win {
		cond(o, model.CondRebooted, "node/"+o.Target, n.BootedAt, "")
	}
}

// ---------------------------------------------------------------- workload

// WorkloadFacet is a running application: a Deployment, a service on a VM,
// an ECS service.
type WorkloadFacet struct {
	ReplicasReady, ReplicasDesired Num
	CPUPct, MemPct                 Num
	Restarts                       Num           // in RestartWindow
	RestartWindow                  time.Duration // default 5m
	Killed, Evicted                Num
	Crashing                       []Instance // crash looping
	OutOfMemory                    []Instance // killed for memory
	ImagePull                      []Instance // cannot pull the image
	EvictedInstances               []Instance
	Ready                          bool // false only when the provider knows it is not serving
	NotReadyDetail                 string
	NotReadySince                  time.Time
	Since                          SinceFunc
}

// EmitWorkload writes the workload into an observation.
func EmitWorkload(o *probe.Observation, w WorkloadFacet, now time.Time) {
	put(o, "replicas_ready", w.ReplicasReady)
	put(o, "replicas_desired", w.ReplicasDesired)
	put(o, "cpu_pct", w.CPUPct)
	put(o, "mem_pct", w.MemPct)
	put(o, "restarts", w.Restarts)
	if w.Restarts.Set {
		win := w.RestartWindow
		if win <= 0 {
			win = 5 * time.Minute
		}
		metrics(o)["restart_window_s"] = win.Seconds()
	}
	put(o, "killed", w.Killed)
	put(o, "evicted", w.Evicted)
	for _, i := range w.Crashing {
		cond(o, model.CondCrashLoopBackOff, i.Ref, i.Since, i.Detail)
	}
	for _, i := range w.OutOfMemory {
		cond(o, model.CondOOMKilled, i.Ref, i.Since, i.Detail)
	}
	for _, i := range w.ImagePull {
		cond(o, model.CondImagePullBackOff, i.Ref, i.Since, i.Detail)
	}
	for _, i := range w.EvictedInstances {
		cond(o, model.CondEvicted, i.Ref, i.Since, i.Detail)
	}
	if !w.Ready && w.NotReadyDetail != "" {
		since := w.NotReadySince
		if since.IsZero() {
			since = sinceOf(w.Since, KeyNotReady, now)
		}
		cond(o, model.CondNotReady, o.Target, since, w.NotReadyDetail)
	}
}

// ---------------------------------------------------------------- worker

// BackgroundWorkerFacet is a fleet of background workers, whatever runs
// it: Celery, Hatchet, Sidekiq, Temporal.
type BackgroundWorkerFacet struct {
	Online, Total       Num // workers
	SlotsUsed, SlotsMax Num // concurrency
	Active              Num // tasks running now
	Backlog             Num // work waiting for this fleet (queued + pending)
	Running             []Task
	TypicalDuration     time.Duration // p95 of recent task durations
	LongTask            time.Duration // stuck threshold, default 10m
	NotReadyDetail      string
	Since               SinceFunc
}

// EmitBackgroundWorker writes the fleet into an observation.
func EmitBackgroundWorker(o *probe.Observation, w BackgroundWorkerFacet, now time.Time) {
	put(o, "workers_online", w.Online)
	put(o, "workers_total", w.Total)
	if w.SlotsMax.Set && w.SlotsMax.V > 0 {
		put(o, "pool_used", w.SlotsUsed)
		put(o, "pool_max", w.SlotsMax)
	}
	put(o, "active", w.Active)
	put(o, "waiters", w.Backlog)
	if w.TypicalDuration > 0 {
		metrics(o)["p95_s"] = w.TypicalDuration.Seconds()
	}
	longest := emitTasks(o, w.Running, w.LongTask, now)
	if longest > 0 {
		metrics(o)["running_s"] = longest.Seconds()
	}
	if w.SlotsMax.Set && w.SlotsMax.V > 0 && w.SlotsUsed.Set && w.SlotsUsed.V >= w.SlotsMax.V && w.Backlog.Set && w.Backlog.V > 0 {
		cond(o, model.CondPoolExhausted, o.Target, sinceOf(w.Since, KeyPoolExhausted, now), fmt.Sprintf("all %d slots busy", int(w.SlotsMax.V)))
	}
	if w.Online.Set && w.Online.V == 0 {
		detail := w.NotReadyDetail
		if detail == "" {
			detail = "no workers online"
		}
		cond(o, model.CondNotReady, o.Target, sinceOf(w.Since, KeyNoWorkers, now), detail)
	}
}

// emitTasks raises TaskRunning for stuck tasks (oldest first, at most 5) and
// returns the longest running age.
func emitTasks(o *probe.Observation, tasks []Task, longTask time.Duration, now time.Time) time.Duration {
	if longTask <= 0 {
		longTask = DefaultLongTask
	}
	ts := append([]Task(nil), tasks...)
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].Started.Before(ts[j].Started) })
	var longest time.Duration
	stuck := 0
	for _, t := range ts {
		if t.Started.IsZero() {
			continue
		}
		age := now.Sub(t.Started)
		if age > longest {
			longest = age
		}
		if age >= longTask && stuck < 5 {
			stuck++
			cond(o, model.CondTaskRunning, t.ID, t.Started, t.Name)
		}
	}
	return longest
}

// ---------------------------------------------------------------- queue

// QueueFacet is a queue: Redis list, RabbitMQ queue, Hatchet queue, SQS.
type QueueFacet struct {
	Depth, Pending, Running Num
	Consumers               Num
	Oldest                  time.Duration
	HasOldest               bool
	Rate, PublishRate       Num
	GrowthPerMin            Num
	Running_                []Task
	TypicalDuration         time.Duration
	LongTask                time.Duration
}

// EmitQueue writes the queue into an observation.
func EmitQueue(o *probe.Observation, q QueueFacet, now time.Time) {
	if q.Depth.Set {
		metrics(o)["depth"] = q.Depth.V + q.Pending.V
	}
	put(o, "pending", q.Pending)
	put(o, "running", q.Running)
	put(o, "active", q.Running)
	put(o, "consumers", q.Consumers)
	if q.HasOldest {
		metrics(o)["oldest_age_s"] = q.Oldest.Seconds()
	}
	put(o, "rate", q.Rate)
	put(o, "publish_rate", q.PublishRate)
	put(o, "growth_per_min", q.GrowthPerMin)
	if q.TypicalDuration > 0 {
		metrics(o)["p95_s"] = q.TypicalDuration.Seconds()
	}
	if longest := emitTasks(o, q.Running_, q.LongTask, now); longest > 0 {
		metrics(o)["running_s"] = longest.Seconds()
	}
}

// ---------------------------------------------------------------- job

// ScheduledJobFacet is a scheduled or one-off unit of work: a CronJob, a
// workflow run on a schedule.
type ScheduledJobFacet struct {
	Active, Succeeded, Failed, Queued Num
	LastFailure                       *Failure
	Running                           *Task
	Schedule                          string // plain words: "every 15 min", "cron 0 * * * *"
}

// EmitScheduledJob writes the job into an observation.
func EmitScheduledJob(o *probe.Observation, j ScheduledJobFacet, now time.Time) {
	put(o, "active", j.Active)
	put(o, "succeeded", j.Succeeded)
	put(o, "failed", j.Failed)
	if j.Queued.Set && j.Queued.V > 0 {
		put(o, "queued", j.Queued)
	}
	if j.Running != nil {
		if !j.Running.Started.IsZero() {
			metrics(o)["running_s"] = max(now.Sub(j.Running.Started), 0).Seconds()
		}
		cond(o, model.CondJobRunning, j.Running.ID, j.Running.Started, j.Running.Name)
	}
	if j.LastFailure != nil {
		cond(o, model.CondJobFailed, j.LastFailure.Ref, j.LastFailure.At, j.LastFailure.Reason)
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

// ---------------------------------------------------------------- load balancer

// Target is one backend of a load balancer.
type Target struct {
	ID      string // the component id the target stands for
	Healthy bool
	Detail  string
	Since   time.Time
}

// LoadBalancerFacet is a load balancer: Hetzner, AWS, a cloud LB.
type LoadBalancerFacet struct {
	Connections Num
	Rate        Num
	Targets     []Target
	// TargetsHealthy and TargetsTotal override the counts derived from
	// Targets when the provider reports them without naming the targets.
	TargetsHealthy, TargetsTotal Num
}

// EmitLoadBalancer writes the balancer into an observation.
func EmitLoadBalancer(o *probe.Observation, lb LoadBalancerFacet, now time.Time) {
	put(o, "connections", lb.Connections)
	put(o, "rate", lb.Rate)
	healthy, total := lb.TargetsHealthy, lb.TargetsTotal
	if len(lb.Targets) > 0 && !total.Set {
		h := 0
		for _, t := range lb.Targets {
			if t.Healthy {
				h++
			}
		}
		healthy, total = NI(h), NI(len(lb.Targets))
	}
	put(o, "targets_healthy", healthy)
	put(o, "targets_total", total)
	if total.Set && healthy.Set && healthy.V == 0 && total.V > 0 {
		cond(o, model.CondTargetUnhealthy, o.Target, now, "no healthy targets")
	}
}

// LoadBalancerEdges writes one observation per named target for the edge
// from the balancer to it: healthy 1/0 and HealthCheckFailing when not.
func LoadBalancerEdges(lbID, probeKind string, targets []Target, at time.Time) []probe.Observation {
	var out []probe.Observation
	for _, t := range targets {
		if t.ID == "" {
			continue
		}
		o := probe.Observation{Target: model.EdgeID(lbID, t.ID), Probe: probeKind, At: at, Metrics: map[string]float64{}}
		if t.Healthy {
			o.Metrics["healthy"] = 1
		} else {
			o.Metrics["healthy"] = 0
			since := t.Since
			if since.IsZero() {
				since = at
			}
			cond(&o, model.CondHealthCheckFailing, "target/"+t.ID, since, t.Detail)
		}
		out = append(out, o)
	}
	return out
}

// ---------------------------------------------------------------- certificate

// CertificateFacet is a TLS certificate and its renewal.
type CertificateFacet struct {
	Known         bool
	NotAfter      time.Time
	RenewalFailed *Failure
	WarnDays      float64 // default 14
}

// EmitCertificate writes cert_days and the certificate conditions.
func EmitCertificate(o *probe.Observation, c CertificateFacet, now time.Time) {
	if !c.Known || c.NotAfter.IsZero() {
		return
	}
	days := c.NotAfter.Sub(now).Hours() / 24
	if days < 0 {
		days = 0
	}
	metrics(o)["cert_days"] = float64(int(days))
	warn := c.WarnDays
	if warn <= 0 {
		warn = 14
	}
	switch {
	case !now.Before(c.NotAfter):
		cond(o, model.CondCertExpired, "certificate/"+o.Target, c.NotAfter, "")
	case days <= warn:
		cond(o, model.CondCertExpiring, "certificate/"+o.Target, time.Time{}, fmt.Sprintf("%d days left", int(days)))
	}
	if c.RenewalFailed != nil {
		cond(o, model.CondCertRenewalFailed, c.RenewalFailed.Ref, c.RenewalFailed.At, c.RenewalFailed.Reason)
	}
}

// ---------------------------------------------------------------- ingress

// IngressFacet is the HTTP entry of a system: an ingress controller, a
// reverse proxy, an API gateway.
type IngressFacet struct {
	Rate, ErrorRate Num
	Certificate     CertificateFacet
	Hosts           []string
}

// EmitIngress writes the entry into an observation.
func EmitIngress(o *probe.Observation, in IngressFacet, now time.Time) {
	put(o, "rate", in.Rate)
	put(o, "error_rate", in.ErrorRate)
	EmitCertificate(o, in.Certificate, now)
	if len(in.Hosts) > 0 {
		if o.Detail == nil {
			o.Detail = map[string]any{}
		}
		if _, ok := o.Detail["hosts"]; !ok {
			o.Detail["hosts"] = in.Hosts
		}
	}
}

// ---------------------------------------------------------------- firewall

// FirewallFacet is a firewall or security group. Bound to a component it
// reports its rules; bound to an edge it reports whether the path is allowed.
type FirewallFacet struct {
	Rules Num
	// Allowed is nil when the probe was not asked about a path.
	Allowed      *bool
	DeniedDetail string // the rule or rule set that blocks the path
	DeniedSince  time.Time
	Since        SinceFunc
}

// EmitFirewall writes the firewall into an observation.
func EmitFirewall(o *probe.Observation, f FirewallFacet, now time.Time) {
	put(o, "rules", f.Rules)
	if f.Allowed == nil {
		return
	}
	if *f.Allowed {
		metrics(o)["allowed"] = 1
		return
	}
	metrics(o)["allowed"] = 0
	since := f.DeniedSince
	if since.IsZero() {
		since = sinceOf(f.Since, KeyDenied, now)
	}
	cond(o, model.CondFirewallDenied, "rule/"+f.DeniedDetail, since, f.DeniedDetail)
}

// ---------------------------------------------------------------- dns

// DNSFacet is a DNS record.
type DNSFacet struct {
	Resolves  bool
	Addresses []string
	Expected  string
	Matched   bool
	Reason    string
}

// EmitDNS writes the record into an observation.
func EmitDNS(o *probe.Observation, d DNSFacet, now time.Time) {
	if d.Resolves {
		metrics(o)["resolves"] = 1
	} else {
		metrics(o)["resolves"] = 0
	}
	metrics(o)["addresses"] = float64(len(d.Addresses))
	if o.Detail == nil {
		o.Detail = map[string]any{}
	}
	o.Detail["addresses"] = d.Addresses
	if d.Expected != "" {
		o.Detail["expect"] = d.Expected
		o.Detail["matched"] = d.Matched
	}
	if d.Reason != "" {
		o.Detail["reason"] = d.Reason
	}
}

// ---------------------------------------------------------------- database

// DatabaseFacet is a database server or cluster member.
type DatabaseFacet struct {
	CPUPct, MemPct, DiskPct         Num
	Rate                            Num // transactions per second
	ConnectionsUsed, ConnectionsMax Num
	ActiveConnections, LockWaiters  Num
	UsedBytes, TotalBytes           Num
	LagBytes                        Num    // worst replica lag seen from this primary
	WALRetained                     Num    // WAL held for inactive slots
	Role                            string // primary, replica
	Ready                           bool
	NotReadyDetail                  string
	NotReadySince                   time.Time
	Backup, Vacuum, Migration       *Task
}

// EmitDatabase writes the database into an observation.
func EmitDatabase(o *probe.Observation, d DatabaseFacet, now time.Time) {
	put(o, "cpu_pct", d.CPUPct)
	put(o, "mem_pct", d.MemPct)
	put(o, "disk_pct", d.DiskPct)
	put(o, "rate", d.Rate)
	put(o, "connections_used", d.ConnectionsUsed)
	put(o, "connections_max", d.ConnectionsMax)
	put(o, "active_connections", d.ActiveConnections)
	put(o, "waiters", d.LockWaiters)
	put(o, "used_bytes", d.UsedBytes)
	put(o, "total_bytes", d.TotalBytes)
	if !d.DiskPct.Set && d.UsedBytes.Set && d.TotalBytes.Set && d.TotalBytes.V > 0 {
		metrics(o)["disk_pct"] = 100 * d.UsedBytes.V / d.TotalBytes.V
	}
	put(o, "lag_bytes", d.LagBytes)
	put(o, "wal_retained_bytes", d.WALRetained)
	if d.Role != "" {
		if o.Detail == nil {
			o.Detail = map[string]any{}
		}
		o.Detail["role"] = d.Role
	}
	if !d.Ready && d.NotReadyDetail != "" {
		cond(o, model.CondNotReady, o.Target, d.NotReadySince, d.NotReadyDetail)
	}
	for kind, t := range map[string]*Task{model.CondBackup: d.Backup, model.CondVacuum: d.Vacuum, model.CondMigration: d.Migration} {
		if t != nil {
			cond(o, kind, t.ID, t.Started, t.Name)
		}
	}
}

// ---------------------------------------------------------------- replication

// ReplicationFacet is a consumer of the primary's replication stream as seen
// from the primary: a replica, a sync engine, a CDC connector. The same
// facet serves the consumer component and the edge from the primary to it.
type ReplicationFacet struct {
	Slot        string
	Streaming   bool
	Lag         Num
	WALRetained Num
	BrokenSince time.Time
	Detail      string
	SlotDetail  string
}

// EmitReplication writes the stream into an observation.
func EmitReplication(o *probe.Observation, r ReplicationFacet, now time.Time) {
	if r.Streaming {
		metrics(o)["streaming"] = 1
	} else {
		metrics(o)["streaming"] = 0
	}
	put(o, "lag_bytes", r.Lag)
	put(o, "wal_retained_bytes", r.WALRetained)
	if !r.Streaming {
		ref := "slot/" + r.Slot
		detail := r.Detail
		if detail == "" {
			detail = "replication slot " + r.Slot + " inactive"
		}
		cond(o, model.CondReplicationBroken, ref, r.BrokenSince, detail)
		if r.WALRetained.Set {
			cond(o, model.CondSlotInactive, ref, r.BrokenSince, r.SlotDetail)
		}
	}
}

// ---------------------------------------------------------------- sync

// SyncEngineFacet is a sync engine that serves clients from the
// replication stream: Electric SQL. Its replication side is a
// ReplicationFacet.
type SyncEngineFacet struct {
	Ready          bool
	NotReadyDetail string
	Latency        Num // ms
	ShapeLatency   Num // ms
	UpToDate       Num // 1 when a live poll confirmed the stream is current
	Busy           bool
	Unreachable    bool
	UnreachDetail  string
	Since          SinceFunc
}

// EmitSyncEngine writes the sync engine into an observation.
func EmitSyncEngine(o *probe.Observation, s SyncEngineFacet, now time.Time) {
	if s.Ready {
		metrics(o)["ready"] = 1
	} else {
		metrics(o)["ready"] = 0
	}
	put(o, "latency_ms", s.Latency)
	put(o, "shape_ms", s.ShapeLatency)
	put(o, "up_to_date", s.UpToDate)
	if s.Busy {
		metrics(o)["busy"] = 1
	}
	if s.Unreachable {
		cond(o, model.CondConnectionRefused, o.Target, sinceOf(s.Since, KeyTimeout, now), s.UnreachDetail)
		return
	}
	if !s.Ready {
		cond(o, model.CondNotReady, o.Target, sinceOf(s.Since, KeyNotReady, now), s.NotReadyDetail)
	}
}

// ---------------------------------------------------------------- cache

// CacheFacet is a cache: Redis, Valkey, Memcached.
type CacheFacet struct {
	MemPct    Num
	HitRate   Num // percent
	Evictions Num // per minute
	Clients   Num
	Full      bool
	FullSince time.Time
	Policy    string
}

// EmitCache writes the cache into an observation.
func EmitCache(o *probe.Observation, c CacheFacet, now time.Time) {
	put(o, "mem_pct", c.MemPct)
	put(o, "hit_rate", c.HitRate)
	put(o, "evictions", c.Evictions)
	put(o, "clients", c.Clients)
	if c.Full {
		cond(o, model.CondCacheFull, "policy/"+c.Policy, c.FullSince, c.Policy)
	}
}

// ---------------------------------------------------------------- storage

// StorageFacet is a volume or a bucket. A volume knows its capacity; a
// bucket usually does not (TotalBytes then comes from a configured quota, or
// stays unset) but knows how many objects it holds and when it was last
// written. NotReadyDetail is set when the store answers but cannot serve the
// volume or bucket (it does not exist, is still provisioning); Unreachable
// when nothing answers at all.
type StorageFacet struct {
	UsedBytes, TotalBytes Num
	IOPS                  Num
	Objects               Num
	Latency               Num // ms, the store's answer time
	LastWrite             time.Time

	NotReadyDetail string
	Unreachable    bool
	UnreachDetail  string
	Since          SinceFunc
}

// EmitStorage writes the volume or bucket into an observation.
func EmitStorage(o *probe.Observation, s StorageFacet, now time.Time) {
	put(o, "used_bytes", s.UsedBytes)
	put(o, "total_bytes", s.TotalBytes)
	put(o, "iops", s.IOPS)
	put(o, "objects", s.Objects)
	put(o, "latency_ms", s.Latency)
	if s.UsedBytes.Set && s.TotalBytes.Set && s.TotalBytes.V > 0 {
		metrics(o)["disk_pct"] = 100 * s.UsedBytes.V / s.TotalBytes.V
	}
	if !s.LastWrite.IsZero() {
		if o.Detail == nil {
			o.Detail = map[string]any{}
		}
		o.Detail["last_write"] = s.LastWrite.UTC().Format(time.RFC3339)
	}
	if s.Unreachable {
		cond(o, model.CondConnectionRefused, o.Target, sinceOf(s.Since, KeyTimeout, now), s.UnreachDetail)
		return
	}
	if s.NotReadyDetail != "" {
		cond(o, model.CondNotReady, o.Target, sinceOf(s.Since, KeyNotReady, now), s.NotReadyDetail)
	}
}

// ---------------------------------------------------------------- observability

// ObservabilityFacet is the source of traffic data: SigNoz, a collector,
// Prometheus.
type ObservabilityFacet struct {
	IngestRate  Num
	DiskPct     Num
	NoData      bool
	NoDataSince time.Time
	Since       SinceFunc
}

// EmitObservability writes the telemetry source into an observation.
func EmitObservability(o *probe.Observation, t ObservabilityFacet, now time.Time) {
	put(o, "ingest_rate", t.IngestRate)
	put(o, "disk_pct", t.DiskPct)
	if t.NoData || (t.IngestRate.Set && t.IngestRate.V == 0) {
		since := t.NoDataSince
		if since.IsZero() {
			since = sinceOf(t.Since, KeyNoData, now)
		}
		cond(o, model.CondNoData, o.Target, since, "")
	}
}

// ---------------------------------------------------------------- external

// ExternalFacet is a dependency outside your control: an API, a SaaS.
type ExternalFacet struct {
	LatencyMS   Num
	ErrorRate   Num // percent over a window
	TimeoutRate Num // percent over a window
	Status      Num // last HTTP status
	// TimingOut is set after consecutive failures; Since is the first one.
	TimingOut      bool
	TimingOutSince time.Time
	Detail         string
}

// EmitExternal writes the dependency into an observation.
func EmitExternal(o *probe.Observation, e ExternalFacet, now time.Time) {
	put(o, "latency_ms", e.LatencyMS)
	put(o, "error_rate", e.ErrorRate)
	put(o, "timeout_rate", e.TimeoutRate)
	put(o, "status", e.Status)
	if e.TimingOut {
		cond(o, model.CondTimeout, o.Target, e.TimingOutSince, e.Detail)
	}
}

// ---------------------------------------------------------------- traffic

// TrafficFacet is an edge that carries requests, transactions, jobs or
// operations: http, grpc, sql, queue, cache, tcp, external.
type TrafficFacet struct {
	Rate         Num // per second, in the edge kind's unit
	ErrorRate    Num // percent
	TimeoutRate  Num // percent
	P95MS        Num
	Queued       Num // work waiting at the destination (pool waiters, pending connections)
	PoolUsed     Num
	PoolMax      Num
	HitRate      Num // cache edges
	RateBaseline Num // the usual rate, when the source knows it
	// Blocked names the rule that stops the path, when a firewall does.
	Blocked      bool
	BlockedRule  string
	BlockedSince time.Time
	// Refused is set when connections to the destination are refused.
	Refused       bool
	RefusedDetail string
	RefusedSince  time.Time
	Since         SinceFunc
}

// EmitTraffic writes the edge into an observation.
func EmitTraffic(o *probe.Observation, t TrafficFacet, now time.Time) {
	put(o, "rate", t.Rate)
	put(o, "error_rate", t.ErrorRate)
	put(o, "timeout_rate", t.TimeoutRate)
	put(o, "p95_ms", t.P95MS)
	put(o, "queued", t.Queued)
	put(o, "waiters", t.Queued)
	put(o, "pool_used", t.PoolUsed)
	put(o, "pool_max", t.PoolMax)
	put(o, "hit_rate", t.HitRate)
	put(o, "rate_baseline", t.RateBaseline)
	if t.PoolMax.Set && t.PoolMax.V > 0 && t.PoolUsed.Set && t.PoolUsed.V >= t.PoolMax.V && t.Queued.Set && t.Queued.V > 0 {
		cond(o, model.CondPoolExhausted, o.Target, sinceOf(t.Since, KeyPoolExhausted, now), fmt.Sprintf("pool of %d busy, %d waiting", int(t.PoolMax.V), int(t.Queued.V)))
	}
	if t.Blocked {
		since := t.BlockedSince
		if since.IsZero() {
			since = sinceOf(t.Since, KeyDenied, now)
		}
		cond(o, model.CondFirewallDenied, "rule/"+t.BlockedRule, since, t.BlockedRule)
	}
	if t.Refused {
		since := t.RefusedSince
		if since.IsZero() {
			since = sinceOf(t.Since, KeyTimeout, now)
		}
		cond(o, model.CondConnectionRefused, o.Target, since, t.RefusedDetail)
	}
}
