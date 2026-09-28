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
* A probe that holds a connection releases it in order when `ctx` ends (for
  PostgreSQL: the terminate message, sent on a context of its own, never on
  the cancelled `ctx`) and implements `probe.Closer`, so the runtime waits
  for the goodbye before the process exits. A connection that is cut instead
  takes a port-forward or a tunnel down with it. Do not run a query on
  `ctx` itself: cancelling it halfway cuts the connection just the same.
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

## Facets: the typed contract of the whole system

Every node type on the diagram has a facet named after it, and every edge
kind has one. `internal/probe/facet` defines the structs and one `Emit*`
function each that writes the canonical observation (the metric keys below,
the conditions, the rules for "stuck", "exhausted", "expired", "broken").
A probe reads its provider and fills the facet; the engine, the labels and
the UI never see the provider. Kubernetes is one provider; a VM fleet, ECS,
Nomad or a managed database is another set of probes filling the same
structs. Declare the facets a probe fills in its `Access.Facets`;
`wassup validate` checks that a binding's probe fills a facet the component's
type accepts (`model.Catalog[type].Facets`), or an edge facet for edges.

| Node type | Facet | Fill it with | Providers today |
| --- | --- | --- | --- |
| `node` | `NodeFacet` | cpu/mem/disk, pods, ready, pressure, boot time | `k8s.node` |
| `workload` | `WorkloadFacet` | replicas, cpu/mem, restarts, crashing/OOM/image-pull instances | `k8s.workload`, `cnpg.instance` |
| `backgroundworker` | `BackgroundWorkerFacet` | workers online/total, slots, active, backlog, running tasks, typical duration | `hatchet.workers`, `celery.worker` |
| `queue` | `QueueFacet` | depth, pending, running, consumers, oldest, rates, growth | `hatchet.queue`, `celery.queue`, `amqp.queue`, `redis.list` |
| `scheduledjob` | `ScheduledJobFacet` | counts, last failure, current run, schedule | `k8s.cronjob`, `hatchet.workflow` |
| `loadbalancer` | `LoadBalancerFacet` | connections, rate, targets with health (per-target edges via `LoadBalancerEdges`) | `hcloud.lb` |
| `ingress` | `IngressFacet` (+ `CertificateFacet`) | rate, errors, hosts, certificate expiry and renewal | `k8s.ingress`, `cert.tls` |
| `firewall` | `FirewallFacet` | rules; on an edge, allowed or the denying rule | `hcloud.firewall`, `terraform.state` |
| `dns` | `DNSFacet` | resolves, addresses, expected target | `dns.record` |
| `database` | `DatabaseFacet` | cpu/mem/disk, connections, lock waiters, size, lag, WAL retained, backup/vacuum | `pg.stats`, `cnpg.cluster` |
| `syncengine` | `SyncEngineFacet` (+ `ReplicationFacet`) | ready, latency, shape handshake; slot, streaming, lag, WAL | `electric.sync`, `pg.stats` |
| `cache` | `CacheFacet` | memory, hit rate, evictions, clients, full | `redis.info` |
| `storage` | `StorageFacet` | used/total, iops, objects, last write | `k8s.pvc`, `s3.bucket` |
| `observability` | `ObservabilityFacet` | ingest rate, disk, no data since | `signoz.health` |
| `external` | `ExternalFacet` | latency, error and timeout rates, timing out since | `http.ping` |
| edge | `TrafficFacet` | rate, errors, latency, queued work, pool use, blocked or refused path | `signoz.edge`, `pg.pool`, `hcloud.firewall`, `terraform.state` |
| edge (`replication`) | `ReplicationFacet` | slot, streaming, lag, WAL retained | `pg.stats` with `replica:` |

Where the numbers come from, per backend:

- **Celery**: queue depth is `LLEN` on the Redis list or `messages_ready`
  from RabbitMQ; oldest age from the head message's `published_at`/`eta`
  (Redis) or `head_message_timestamp` (RabbitMQ); growth from a one-minute
  ring; workers, concurrency, running tasks and runtimes from Flower's
  `/api/workers` and `/api/tasks`; the deployment's replicas from
  `k8s.workload` on the same box.
- **Hatchet**: `queue-metrics` (queued, pending, running per queue and
  workflow), `worker` (slots), `workflow-runs` (running and failed tasks),
  `task-metrics` (counts), `workflows/crons` (schedules).
- **Postgres replication**: `pg_stat_replication` (state, lag as
  `pg_wal_lsn_diff`), `pg_replication_slots` (`active`, WAL retained as the
  distance from `restart_lsn` to the current LSN), keyed by the consumer's
  slot or application name; CNPG's operator status through `cnpg.cluster`.

To add a provider (ECS, Nomad, VMs, RDS, Sidekiq, Temporal): write probes
that read its API and fill the matching facets, call the `Emit*` functions,
register them with `Facets` set. Nothing else changes: the catalog, the
engine and the UI already know the shape. Only a genuinely new shape needs a
new type in `internal/model/catalog.go` with a facet named after it.

## Metrics vocabulary

Metric keys are shared across probes so thresholds stay portable. Percentages
are 0–100, rates are per second unless the key says otherwise.

| Key | Meaning | Used by types |
| --- | --- | --- |
| `cpu_pct`, `mem_pct`, `disk_pct` | utilisation | node, workload, db, cache, storage, observability |
| `rate` | requests, transactions, jobs or operations per second | edges, lb, ingress |
| `error_rate`, `timeout_rate` | percent of requests | edges, ingress, external |
| `p95_ms`, `latency_ms` | latency | edges, external, storage, syncengine |
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
| `used_bytes`, `total_bytes`, `iops`, `objects` | volumes and buckets | storage |
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
