# Component catalog

Fifteen building block types plus `custom`. Each type declares its lane, the
gauges drawn inside its box, the **facet** a probe must fill to feed it
(`internal/probe/facet`; the facet is named after the type: `node` ↔
`NodeFacet`, `backgroundworker` ↔ `BackgroundWorkerFacet`), and the probes
that ship for it. Use only these types. The older spellings `lb`, `db`, `job`,
`worker` and `sync` are still accepted and normalized.

| Type | Facet | Lane | Gauges shown in the box | Shipped probes | Notes |
| --- | --- | --- | --- | --- | --- |
| `dns` | `DNSFacet`, `CertificateFacet` | edge | none | `dns.record` | Optional; shows the hostname and whether it resolves to the LB. Entry point. |
| `firewall` | `FirewallFacet` | edge | rules count | `hcloud.firewall`, `terraform.state` | Drawn as a gate on the path; a blocked edge names the rule. Entry point. |
| `loadbalancer` | `LoadBalancerFacet` | edge | connections, req/s, healthy targets | `hcloud.lb` | Shows which nodes are in rotation. Entry point. |
| `ingress` | `IngressFacet`, `CertificateFacet` | edge | req/s, error rate, cert days left | `k8s.ingress`, `cert.tls` | Behind an `lb` it is a hop, not an entry. |
| `node` | `NodeFacet` | compute | CPU, RAM, disk, pod count | `k8s.node`, `kubelet.stats` | Container for workloads placed by `runs_on`; lists the hosted workloads with their states. |
| `workload` | `WorkloadFacet` | compute | replicas ready/desired, CPU, RAM, restarts | `k8s.workload`, `hatchet.health` | Deployment, StatefulSet or DaemonSet; one box per workload, not per pod. |
| `backgroundworker` | `BackgroundWorkerFacet`, `WorkloadFacet` | compute | workers online/total, slots used/max, ready, CPU, RAM | `hatchet.workers`, `celery.worker`, `k8s.workload` | A fleet of background workers, whatever runs it. Every slot busy with work queued reads "waiting, all 24 slots busy, 850 queued"; a task past 10× the usual duration reads as stuck. |
| `scheduledjob` | `ScheduledJobFacet` | compute | running, succeeded, failed last 24h | `k8s.cronjob`, `hatchet.workflow` | CronJobs, one-off jobs and Hatchet workflows. Entry point. |
| `queue` | `QueueFacet` | compute | depth, oldest age, consumers | `celery.queue`, `amqp.queue`, `redis.list`, `hatchet.queue` | Depth is drawn as a growing pile. Celery on Redis binds `celery.queue`; Celery on RabbitMQ binds `amqp.queue` (management API); Hatchet queues bind `hatchet.queue`. |
| `cache` | `CacheFacet` | data | memory used/max, hit rate, evictions | `redis.info` | Full memory with a low hit rate flips it to failing. |
| `database` | `DatabaseFacet`, `ReplicationFacet` | data | CPU, RAM, disk, connections used/max, replication lag | `cnpg.cluster`, `cnpg.instance`, `pg.stats` | `roles: {primary, replicas}` creates one instance component each; replication edges go between them. |
| `storage` | `StorageFacet` | data | used/total, IOPS | `k8s.pvc`, `s3.bucket` | Volumes, buckets. |
| `syncengine` | `SyncEngineFacet`, `ReplicationFacet` | data | replication lag, WAL retained, latency | `electric.sync`, `pg.stats` | A sync engine following the database's replication stream (Electric SQL). Bind `pg.stats` with `replica: <slot name>` on the component and on its replication edge, so an inactive slot shows as failing and the retained WAL as a trend on the primary. |
| `observability` | `ObservabilityFacet` | side | ingest rate, retention disk | `signoz.health` | The source of traffic data; when it fails, edges show "no data", not idle. |
| `external` | `ExternalFacet` | side | latency, error rate, timeouts | `signoz.edge`, `http.ping` | GitHub, LLM providers, payment APIs, anything outside your control. |
| `custom` | any | compute | none | any | Free icon, no gauges. |

Common fields on every component: `id`, `type`, `label`, `group`, `lane`
(override), `icon`, `runs_on` (workload to nodes), `roles` (db only),
`notes` (free text for the detail panel), `owner` (a Slack handle shown on
failing).

Groups: `cloud`, `region`, `cluster`, `namespace`, `zone`. Groups draw as
containers with a title and can be collapsed; a collapsed group shows the
worst state of its children.

Edges: `kind` in `http`, `grpc`, `tcp`, `sql`, `replication`, `queue`,
`cache`, `external`. A probe bound to an edge fills `TrafficFacet` (rate,
errors, latency, queued work, pool use, a blocked or refused path) or, for
`replication`, `ReplicationFacet`. All edge kinds share the six states but
label their rate in their own unit: req/s for http, grpc and external, tx/s for sql, jobs/s for
queue, ops/s for cache, conn/s for tcp, bytes lag for replication.

Lanes: `edge` (top), `compute`, `data` (bottom), `side` (right column for
observability and externals). A component's lane comes from its type and can
be overridden with `lane:`.

Gauges are one shape everywhere: a bar with the value and its unit, amber at
80 percent, red at 90 percent (per-type or per-component overrides live in
`thresholds.yaml`). A gauge with no data draws dotted. Countdown gauges (cert
days) warn at 14 days by default.

## Thresholds (`thresholds.yaml`)

```yaml
version: 1
defaults:
  gauge_amber_pct: 80
  gauge_red_pct: 90
  disk_failing_pct: 95
  error_rate_pct: 5
  timeout_rate_pct: 1
  queue_depth: 10
  lag_bytes: 67108864
  cert_warn_days: 14
  hit_rate_min_pct: 50
  stale_ticks: 3
  stuck_task_factor: 10
  rate_spike_factor: 1.8
  trend_window_min: 30
components:
  exports-queue:
    queue_depth: 50
```

## Mapping common stacks

**Hatchet** (background workflows on Postgres): the engine is a `workload`
(`k8s.workload` plus `hatchet.health`), each Hatchet queue or workflow with a
backlog is a `queue` (`hatchet.queue`, filter with `queue:` or `workflow:`),
the worker deployment is a `backgroundworker` (`k8s.workload` plus `hatchet.workers`,
which adds slots, long tasks and the backlog behind full slots), every
workflow you care about is a `scheduledjob` (`hatchet.workflow`), and Hatchet's
Postgres is a `database` on `pg.stats`. Edges: api → hatchet queue (`queue`), queue →
workers (`queue`), workers → your database (`sql`), engine → hatchet db (`sql`).

**Celery**: each queue is a `queue` (`celery.queue` on a Redis broker or
`amqp.queue` on RabbitMQ), the worker deployment is a `backgroundworker`
(`k8s.workload` plus `celery.worker` through Flower for online workers,
concurrency and stuck tasks).

**Object storage** (AWS S3, Hetzner Object Storage, MinIO, Spaces, R2): each
bucket the system writes to is a `storage` component on `s3.bucket` (`bucket`,
`endpoint` for anything but AWS, `region`, `access_key_env`, `secret_key_env`;
`quota_bytes` when a fill gauge is wanted, `prefix` to size one folder of a
big bucket). Edges: the workload or worker that writes to it (`tcp`).

**Electric SQL**: one `syncengine` component (`electric.sync` for health and the
shape handshake, `pg.stats` with `replica: electric_slot_default` for the
slot), a `replication` edge from the primary to it bound with the same
`pg.stats` replica spec, and an `http` edge from the ingress to it for the
shape traffic.
