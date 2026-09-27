# Writing a probe

A probe is one Go type implementing `probe.Probe` (see `probe.go`). Every
probe is read only, declares the minimum access it needs, and reports its own
health so a dead data source shows as "no data" rather than as a healthy
system. New infrastructure comes in as a probe plus a catalog type; the
renderer never changes.

## Contract

```go
type Probe interface {
    Kind() string                          // "k8s.workload"
    Validate(spec map[string]any) error    // called by `wassup validate`
    Start(ctx context.Context, spec map[string]any, out chan<- Observation) error
    Health() ProbeHealth                   // ok, degraded, failed + message
}
```

* Register in `init()` with `probe.Register(probe.Access{...}, factory)`.
  `Access` documents the kind, source, what it delivers, its spec fields and
  the credentials or RBAC it needs. It is rendered into the skill reference.
* The runtime builds one probe instance per binding and calls `Start` once.
  `Start` must return promptly and push observations from a goroutine until
  `ctx` is done. The runtime closes nothing; stop when `ctx` ends.
* The runtime injects the bound element id as `spec["_target"]` and the tick
  as `spec["_tick"]` (a `time.Duration`). Set `Observation.Target` to that id
  unless the probe deliberately reports for other ids too (a load balancer
  probe reports its per-target edges `lb->node-3`, named through its
  `targets` spec field). Set `Observation.Probe` to `Kind()`.
* Re-emit an observation at least every tick even when nothing changed, so
  the engine can tell stale data from an idle system. Informer-based probes
  read their store on each tick.
* Credentials come from env or the kubeconfig, never from `.wassup/`. Spec
  fields ending in `_env` name an environment variable holding the secret.
* On read errors: send `Observation{Target, Err: msg}` and set health to
  degraded (transient) or failed (misconfigured). Never fabricate a healthy
  observation.
* Everything a probe learns that the detail panel or `explain` may show goes
  in `Detail` (JSON-friendly values only).

## Metrics vocabulary

Metric keys are shared across probes so thresholds stay portable. Percentages
are 0–100, rates are per second unless the key says otherwise.

| Key | Meaning | Used by types |
| --- | --- | --- |
| `cpu_pct`, `mem_pct`, `disk_pct` | utilisation | node, workload, db, cache, storage, observability |
| `rate` | requests, transactions, jobs or operations per second | edges, lb, ingress |
| `error_rate`, `timeout_rate` | percent of requests | edges, ingress, external |
| `p95_ms`, `latency_ms` | latency | edges, external |
| `queued`, `waiters`, `pending` | units of work waiting at the destination | edges |
| `depth`, `oldest_age_s`, `consumers`, `growth_per_min` | queue shape | queue |
| `replicas_ready`, `replicas_desired`, `restarts`, `restart_window_s`, `killed`, `evicted` | pods | workload |
| `pods` | pods on a node | node |
| `connections`, `targets_healthy`, `targets_total`, `healthy` | load balancer | lb, lb edges |
| `connections_used`, `connections_max`, `active_connections` | database connections | db |
| `pool_used`, `pool_max` | connection pool | edges (`pg.pool`) |
| `lag_bytes`, `streaming`, `wal_retained_bytes` | replication | db, replication edges |
| `cert_days` | days until the certificate expires | ingress |
| `hit_rate`, `evictions`, `clients` | cache | cache |
| `active`, `succeeded`, `failed`, `running_s`, `p95_s` | jobs and tasks | job, workload |
| `ingest_rate` | observability intake | observability |
| `rules` | firewall rules | firewall |
| `rate_baseline` | the usual rate, when the source knows it | edges |
| `used_bytes`, `total_bytes`, `iops` | volumes | storage |
| `resolves` | 1 when DNS resolves to the expected target | dns |

## Conditions

Conditions are normalized in `internal/model/conditions.go`. Translate your
source's vocabulary into these; set `Since` whenever the source knows when it
started, and put the human-readable specifics (a rule name, a task name, an
error message) in `Detail`.

| Condition | Emitted by | Effect |
| --- | --- | --- |
| `CrashLoopBackOff`, `OOMKilled`, `ImagePullBackOff`, `Evicted` | `k8s.workload` | workload failing |
| `NotReady`, `MemoryPressure`, `DiskPressure`, `Rebooted` | `k8s.node` | node failing; reboot named in the label |
| `TargetUnhealthy`, `HealthCheckFailing` | `hcloud.lb` | edge blocked |
| `CertExpired`, `CertExpiring`, `CertRenewalFailed` | `k8s.ingress`, `cert.tls` | ingress failing / warn |
| `ReplicationBroken`, `SlotInactive` | `pg.stats`, `cnpg.cluster` | replication edge failing |
| `NoData` | `signoz.health` | observability failing, edges say "no data" |
| `Timeout` | `http.ping` | external failing |
| `JobFailed`, `JobRunning` | `k8s.cronjob` | job failing / processing |
| `TaskRunning`, `Migration`, `Backup`, `Vacuum` | `celery.queue`, `pg.stats`, `cnpg.cluster` | processing, `Detail` is the task name |
| `CacheFull` | `redis.info` | cache failing |
| `FirewallDenied`, `ConnectionRefused` | `hcloud.firewall`, `terraform.state` | blocked, `Detail` is the rule |
| `PoolExhausted` | `pg.pool` | waiting |
| `Switchover`, `Scaled` | `cnpg.cluster`, `k8s.workload` | informational |

## Events

Events are the change markers on the timeline. Kinds: `deploy`, `terraform`,
`node`, `cert`, `scale`, `switchover`, `eviction`, `job`. Fill `Target` with
the component id, `Summary` with a short sentence ("deploy of api b7e9f21"),
`Author` and `Ref` when known. Emit each event once; the binder dedupes on
time, kind, target and summary anyway.

## Testing

Table tests with recorded inputs are enough; do not require a live cluster.
Every scenario under `testdata/scenarios/` is a recorded set of observations
that a real probe would have produced, so check yours against them.
