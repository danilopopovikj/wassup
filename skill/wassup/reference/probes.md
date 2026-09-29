# Probes

A probe is a Go type implementing one interface; the TUI, the CLI and the
tests all go through it. Every probe is read only, declares the minimum access
it needs, and reports its own health so a dead data source shows as "no data"
rather than as a healthy system. `wassup probes` prints this table for the
build you have; `wassup probes --json` gives the same as data.

## Conventions

- One entry per component or edge id in `bindings.yaml`, each a list of probes.
  The `probe` key names the kind; every other key is that probe's spec.
- Spec fields ending in `_env` name the environment variable that holds the
  credential (`password_env: SHOP_DB_PASSWORD`, `token_env: HCLOUD_TOKEN`).
  The values live in `.wassup/local.env`, which git ignores and wassup
  reads on every run, or in the environment; never in any other file of
  `.wassup/`. Kubernetes access uses the kubeconfig context (`--kubeconfig`,
  `--context`, or `WASSUP_KUBECONFIG` / `WASSUP_CONTEXT` in `local.env`,
  else `KUBECONFIG`).
- A connection is named by its parts, so nobody builds a URL or encodes a
  password: `host`, `port`, `user`, `database`, `sslmode`, `password_env`
  on the PostgreSQL probes, `host`, `port`, `user`, `db`, `tls`,
  `password_env` on the Redis probes. `dsn_env`, `url` and `addr` still
  work; naming the parts and a DSN at once is an error.
- Every probe has a tier, which is how far it reaches: 0 needs the
  kubeconfig and the network, 1 a token or a key for an API, 2 a connection
  to a data store. `wassup probe --tier <n>` runs the bindings up to a tier.
- Every metric is a ratio or a rate where possible so thresholds are portable.
- Rates come from a probe that counts: `k8s.scrape` reads the counters the
  pods keep themselves, `signoz.edge` the ones SigNoz collects, `pg.stats`
  the transactions of a database, `pg.pool` the ones of a pool. The label
  of a component says its own rate, in the unit of its type; where it
  reports none, the busiest edge that touches it stands in. An edge and a
  component that nothing counts read `no rate measured`, never `idle`.
- Probes emit change markers (deploys, scale changes, node reboots, terraform
  applies, cert renewals, CNPG switchovers); those are the ticks on the timeline.
- A `spec only` probe validates its spec but reports failed health, so the
  bound element draws as unbound. Bind something else meanwhile (an
  `http.ping` on the health endpoint, a `k8s.workload` on the collector).
- A probe that fails, times out or may not read its source delivers nothing:
  the element reads `no data, <reason>`, never `failing`. `failing` is only
  ever concluded from data that was read.

## Read only

wassup reads. It has no way to change a cluster or a database, and that
does not depend on the account it runs as:

| Source | What is sent | What holds it to that |
| --- | --- | --- |
| Kubernetes | `get`, `list`, `watch`, and the request that opens a port-forward | the transport of every client refuses any other request before it is sent |
| PostgreSQL | `SELECT` and `SHOW`, one statement at a time | every round runs in a read-only transaction, so the server refuses a write too |
| pgbouncer | `SHOW` | any other command is refused before it is sent |
| Redis | `INFO`, `LLEN`, `LINDEX` | any other command is refused before it is sent |
| HTTP APIs | `GET` and `HEAD` | any other method is refused before it is sent |
| SigNoz | `GET`, the `POST` that signs in and the `POST` that runs a query | any other request is refused before it is sent, a `POST` to any other path included |

The account is the second lock, and the one that holds whatever runs with
it. Give wassup one that can only read: the ClusterRole below, a PostgreSQL
role with `pg_monitor` and nothing else, a Redis user with
`+info +llen +lindex`, read-only API tokens.

One request reads and still has an effect, and it is off by default:
`electric.sync` with `table` (below).

## Reaching into the cluster: `via`

The probes of PostgreSQL, Redis, Celery, Hatchet and Electric take `via`. With
`via: k8s.service/<namespace>/<name>:<port>` (or
`k8s.pod/<namespace>/<name>:<port>`) wassup opens its own port-forward
through the kubeconfig, connects through it and closes it when it stops: no
`kubectl port-forward` in a second terminal. The DSN then only says who
connects to which database; its host and port are not dialled (its host is
still the name a TLS certificate is checked against). `kubeconfig` and
`context` in the spec override the defaults. This needs `create` on
`pods/portforward`.

For Redis, Celery, Hatchet and Electric the address may be left out with
`via`: the Service is the address. Bindings with the same `via` share one
port-forward, so five Hatchet bindings open one.

```yaml
jobs:
  - probe: hatchet.queue
    via: k8s.service/bookstore/hatchet-api:8080
    token_env: HATCHET_CLIENT_TOKEN
cache:
  - probe: redis.info
    via: k8s.service/bookstore/redis:6379
    password_env: REDIS_PASSWORD
```

Any other value of `via` (`via: bastion`) is a label for the detail panel
and opens nothing: the DSN must then be reachable from where wassup runs.
When you do run `kubectl port-forward` yourself, wassup closes its
connections with a goodbye, so the tunnel survives one probe run after
another.

## The role `pg.stats` connects as

Any role that can connect works, the application's own role included. What
PostgreSQL shows depends on the role:

| | with `pg_read_all_stats` or `pg_monitor` | without |
| --- | --- | --- |
| connections | counted | counted |
| active connections, lock waiters, waiting queries | read | omitted (hidden by PostgreSQL) |
| replica state and lag | from `pg_stat_replication` | state hidden; lag from `pg_replication_slots` |
| slot activity, WAL retained | from `pg_replication_slots` | the same |

Slot activity always comes from `pg_replication_slots.active`. A state
PostgreSQL hides is never read as "not streaming". When the lag comes from
the slot the detail says so (`lag_source: pg_replication_slots`).

`replica:` names one replica on a replication edge. It matches the slot
name (`electric_slot_default`), the `application_name`, or the instance of
a CloudNativePG cluster: instance `bookstore-db-2` has the slot
`_cnpg_bookstore_db_2`.

## `electric.sync` and `table`

Without `table` the probe only calls `/v1/health`, which costs Electric
nothing. **With `table` it requests a shape for the whole table, and
Electric runs a snapshot query against the database to build it.** On a
large table that is a full read of the table on the primary. When no
client of yours syncs that table yet, Electric also changes the database to
serve the shape: it adds the table to its publication and sets the table's
replica identity to full (unless Electric runs with
`ELECTRIC_MANUAL_TABLE_PUBLISHING=true`). wassup sends a GET; the change is
Electric's. Set `table` only on a small table your application already
syncs, never on a large one, and never by default; `wassup discover` does
not propose it.

## Kubernetes RBAC

`wassup access` prints the account for the probes that are bound in
`bindings.yaml`, and nothing more: a ServiceAccount, a ClusterRole with
`get`, `list`, `watch` on what those probes read, and the commands that
write a kubeconfig with a token that expires. It contacts nothing and runs
nothing; the user applies it with the kubeconfig they have.

With every Kubernetes probe bound the role covers: pods, nodes, events,
persistentvolumeclaims, deployments, statefulsets, daemonsets, replicasets,
cronjobs, jobs, ingresses, `postgresql.cnpg.io/clusters` and `backups`,
`cert-manager.io/certificates`, and `metrics.k8s.io` pods and nodes.

Never part of it:

- `nodes/proxy`. `k8s.node` and `k8s.pvc` read disk usage through it, and
  it reaches every endpoint of the kubelet. Without it disk usage is not
  known, and everything else is.
- `pods/exec` and `pods/attach`. wassup has no use for them.
- secrets (below).

Only when asked for:

- `pods/log`, with `wassup access --logs`: for `wassup logs` and the log
  lines in `wassup explain`.
- `create` on `pods/portforward`, with `get` on services and `get`, `list`
  on pods: for `via: k8s.service/...` or `via: k8s.pod/...` on `pg.stats`
  and `pg.pool`. `wassup access` grants it in the namespaces a `via`
  reaches into, and in no other.
- `get` on secrets: only for a `k8s.ingress` binding with
  `read_tls_secret: true`. A TLS secret holds the private key next to the
  certificate, so `k8s.ingress` does not read it by default: it takes the
  expiry from the cert-manager Certificate, else from a TLS handshake with
  the ingress host.

wassup never reads Secrets for discovery. `wassup discover` stores the names
of environment variables and the hosts they resolve to, never the value of a
variable that looks like a credential, and no URL with its user and password.

## Testing a binding

```sh
wassup validate            # spec shape, ids, catalog fit
wassup probe               # runs every binding; exit code 3 when something is unbound
wassup probe --tier 0      # runs the bindings that need the kubeconfig only
wassup probe <id>          # runs the bindings of one element, prints every value
wassup explain <id>        # shows the binding and the last observation
```

`wassup probe` takes two samples of every probe (one tick apart), so rates
that need two readings are there, and closes every connection in order
before it exits. While it runs it prints a line per probe as its first
round comes in (`[12/47] ok k8s.workload api 1.2s`), so a run that waits
says what it waits for. A probe that failed is not waited for a second
time. Per element it prints:

- `bound`: at least one of its probes delivered data in this run. Probes of
  the element that failed are listed under it with the reason, and so is
  what a probe says about its own reading (`note:`), such as a role that
  may not see replication detail and the grant that would let it.
- `later`: the bindings of the element belong to a tier that did not run.
  It is not counted as unbound.
- `UNBOUND` with `no data, <probe>: <reason>`: none of its probes delivered.
  A failed probe never prints as `idle`.
- `no rate measured` instead of `idle`: nothing bound to the element or its
  edges reports a rate, so idle is not known. `idle` means a rate was read
  and it is zero. The diagram says the same.

With `--json` every element carries `probe_results`: per probe its `status`
(`ok`, `error`, `failed` to start, `silent` within the timeout), its `error`
text and the `metrics` it returned. Verify a binding from that, not from the
label alone. `--samples 1` is faster and shows no rates; `--timeout` bounds
the whole run.

`wassup probe <id>` takes a component id, an edge (`api->db`) or a
`wassup://` ref. It starts the bindings of that element alone and prints
the metrics of every probe, the conditions and the detail. Use it while you
fix one binding.

## `s3.bucket` and `list_objects`

By default `s3.bucket` asks whether the bucket is there (one HEAD request)
and reads nothing else: reachable, latency. The size and the number of
objects are then not known, and the box says so. `list_objects: true` sizes
the bucket, **which lists every object under `prefix`, one request per
thousand objects, every round**. `max_objects` bounds it; past the bound
the listing is skipped and the probe says what to set. Ask the user before
you turn it on for a bucket you do not know the size of.

## Rates: `k8s.scrape` and `signoz.edge`

Both read a counter and report how fast it grows: `rate` per second, and
`error_rate`, the percentage of it that the series matching `errors` make
up. `metric` is the name of the counter, `match` and `errors` map a label
to a regular expression that has to match the whole value.

`k8s.scrape` reads the counter where it is kept: on the metrics page of
every ready pod of a workload, with a GET through the API server. It fits a
router or an ingress controller, which counts the requests per service it
passes on. A page can be a few hundred kilobytes; it is read every
`interval` (15 s) and shared by the bindings that read it.

```yaml
ingress->api:
  - probe: k8s.scrape
    namespace: ingress
    selector: app.kubernetes.io/name=traefik
    port: 9100
    metric: traefik_service_requests_total
    match: { service: "bookstore-api-8000@kubernetes" }
    errors: { code: "5.." }
```

A counter that grew between two readings is a rate however short the time
between them. One that stood still is a rate of 0 only once it has been
watched for the whole `window`: two readings a few seconds apart on a
service that answers a request every ten seconds find nothing between them
more often than not. Until then the binding reports no rate and says so in
`quiet_note`, so `wassup probe` on a quiet edge reads `no rate measured`
rather than `idle`. `span_s` is how many seconds the rate was taken over.
The detail also carries `rate_by_node` and `error_rate_by_node`, the rate of
the pods on each machine; a machine with a pod that was not read, or was
read only once so far, is left out rather than reading low.

`signoz.edge` reads the counters SigNoz collects, such as the calls a
service makes to a database (`signoz_db_latency_count`) or to an address
outside (`signoz_external_call_latency_count`), and the transactions of a
database instance. All the bindings of one metric are answered by one
query, sent every `interval` (1 min) over `window` (5 min), so ten bindings
cost SigNoz's database what one costs.

```yaml
api->github:
  - probe: signoz.edge
    url: https://signoz.bookstore.example
    user_env: SIGNOZ_USER          # or token_env, for an API key
    password_env: SIGNOZ_PASSWORD
    metric: signoz_external_call_latency_count
    match: { service.name: api, address: 'api\.github\.com.*' }
    errors: { http.status_code: "5.." }
```

A service that is called now and then has no series in a window in which
nobody called it. `signoz.edge` then reads the last day (`known`, 24 h;
`known: 0s` leaves it out): a series that was counted within it is known,
its counter did not move, and the rate is 0, which the diagram says as
`idle`. The day is read in steps of 24 minutes, every ten minutes, and only
while the window holds nothing of a binding.

A binding that matches no series in the day either reports no rate, and the
element reads `no rate measured`: `match` names a value that does not exist,
or nothing was ever counted. `wassup probe <id>` prints `match_note` and,
under `label_values`, up to 20 values of each label of the `match`, the
busiest first: the values SigNoz holds for `signoz.edge`, the values on the
metrics pages for `k8s.scrape`.

What is called a few times an hour reads `idle` most of the time over five
minutes. Give such an edge a longer `window` (`window: 1h`) and its label
says what a person would count: `flowing, 12 req/h`. A rate below one a
second is said by the minute, one below one a minute by the hour.

Only what is instrumented is counted: a service that sends no traces has no
calls in SigNoz, and its edges stay at `no rate measured`.

`pg.stats` leaves its own transactions out of the rate of a database: each
of its rounds ends with a commit, and a database nobody else uses reads
idle.

## Catalog

| Probe | Tier | Facets | Source | Delivers | Spec fields | Needs | Status |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `amqp.queue` | 1 | QueueFacet | the RabbitMQ management API (GET /api/queues/<vhost>/<queue>) | depth (messages ready), unacked, consumers, rate (deliver/get per second, ack rate as fallback), publish_rate, growth_per_min over the last minute, oldest_age_s when the queue exposes head_message_timestamp; detail: state, memory, node, vhost | `management_url`, `queue`, `vhost`, `user`, `password_env`, `interval`, `timeout` | a management user with the monitoring tag; the password in the environment variable named by password_env (default RABBITMQ_PASSWORD) | shipped |
| `celery.queue` | 2 | QueueFacet | the Celery queue list on a Redis broker (LLEN, LINDEX) and, when flower_url is set, Flower's REST API | depth, oldest_age_s (when the oldest envelope carries published_at or eta), growth_per_min; with Flower: consumers, running_s, p95_s and TaskRunning for tasks older than long_task; detail: queue, oldest_task, via | `broker`, `host`, `port`, `queue`, `user`, `password_env`, `db`, `tls`, `flower_url`, `oldest`, `long_task`, `via`, `kubeconfig`, `context` | network access to the broker, named by broker (a redis:// URL) or by host and port with db and tls; its password in the environment variable named by password_env (taken as it is, nothing to encode); optionally, access to Flower. With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to the server, which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); bindings with the same via share one. The address may then be left out; one that is set is the name in the messages and in the certificate, not what is dialled. via leads to the broker; Flower is read at flower_url as it is | shipped |
| `celery.worker` | 0 | BackgroundWorkerFacet | Flower's REST API (/api/workers with and without status=true, /api/tasks?state=SUCCESS) | workers_online, workers_total, active, pool_max, pool_used, running_s, p95_s (when at least 10 finished tasks are known), queues; TaskRunning for tasks older than long_task, NotReady when no worker is online; detail: flower_url, via, workers, queues, long_tasks | `flower_url`, `name`, `long_task`, `interval`, `timeout`, `via`, `kubeconfig`, `context` | HTTP access to Flower; no credentials. With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to Flower, which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); bindings with the same via share one. flower_url may then be left out and reads http://<name>.<namespace>.svc:<port>; one that is set keeps its scheme and its path, and its host is the Host header and the name of the certificate, not the address that is dialled | shipped |
| `cert.tls` | 0 | CertificateFacet | a TLS handshake with the endpoint | cert_days until the leaf certificate expires, CertExpiring and CertExpired, subject, issuer and SANs | `addr`, `servername`, `interval` | TCP access to host:port; no credentials | shipped |
| `cnpg.cluster` | 0 | DatabaseFacet, ReplicationFacet | CloudNativePG Cluster (postgresql.cnpg.io/v1) and Backup objects through the dynamic client, plus the pod informer | replicas_ready, replicas_desired; ReplicationBroken, Backup, Switchover; switchover events | `namespace (required)`, `cluster (required)`, `kubeconfig`, `context` | get/list on clusters.postgresql.cnpg.io and backups.postgresql.cnpg.io; list/watch on pods in the namespace | shipped |
| `cnpg.instance` | 0 | DatabaseFacet, WorkloadFacet | the instance pod of a CloudNativePG cluster (pod informer) and metrics.k8s.io PodMetrics | cpu_pct, mem_pct; NotReady | `namespace (required)`, `cluster (required)`, `role (primary|replica) or instance (pod name)`, `kubeconfig`, `context` | list/watch on pods in the namespace; get/list on pods.metrics.k8s.io | shipped |
| `dns.record` | 0 | DNSFacet | the system resolver | resolves (1/0), addresses count, the CNAME and whether the record matches the expected target | `host`, `expect`, `interval` | DNS resolution from the machine running wassup; no credentials | shipped |
| `electric.sync` | 1 | SyncEngineFacet | the Electric SQL HTTP API: GET /v1/health and, when table is set, a /v1/shape handshake plus one short live poll | latency_ms of the health request, ready 1/0; with table: shape_ms, up_to_date 1/0, columns, busy on 429; NotReady while Electric waits for Postgres or the shape is unavailable, ConnectionRefused when the service cannot be reached; detail: url, via, status, table | `url`, `secret_env`, `table`, `interval`, `timeout`, `via`, `kubeconfig`, `context` | HTTP access to Electric; the ELECTRIC_SECRET in the environment variable named by secret_env when the service requires one. With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to Electric, which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); bindings with the same via share one. url may then be left out and reads http://<name>.<namespace>.svc:<port>; a url that is set keeps its scheme and its path, and its host is the Host header and the name of the certificate, not the address that is dialled | shipped |
| `fixture` | 0 |  | a recorded fixture directory | whatever was recorded | `dir`, `speed` | read access to the directory | shipped |
| `git.events` | 0 |  | git log of a local working copy | deploy events for every commit on the branch within the lookback, plus the branch head in detail | `repo`, `branch`, `interval`, `lookback`, `author_format` | the git binary and read access to the repository; no credentials | shipped |
| `hatchet.health` | 1 | WorkloadFacet | the Hatchet API's /api/ready and /api/live endpoints and /api/v1/meta | latency_ms, ready (1/0), live (1/0); NotReady while /api/ready is not 200; detail: version, url, via | `url`, `token_env`, `tenant`, `interval`, `timeout`, `via`, `kubeconfig`, `context` | HTTP access to the Hatchet API; the token named by token_env is sent when present but not required. With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to the API, which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); bindings with the same via share one. url may then be left out and reads http://<name>.<namespace>.svc:<port>; a url that is set keeps its scheme and its path, and its host is the Host header and the name of the certificate, not the address that is dialled | shipped |
| `hatchet.queue` | 1 | QueueFacet | the Hatchet REST API: tenant task-stats (queue-metrics, then step-run-queue-metrics, on servers without it), the worker list and, without task-stats, the queued task runs | depth (queued, + pending where the server reports it), pending, running, active, growth_per_min, consumers (active workers), oldest_age_s (age of the oldest queued task); detail: queues, tasks (task-stats), total, source, the queue or workflow filter, oldest_scope, url, via | `url`, `token_env`, `tenant`, `interval`, `timeout`, `via`, `kubeconfig`, `context`, `queue`, `workflow` | a Hatchet API token in the environment variable named by token_env (default HATCHET_CLIENT_TOKEN); the tenant id from the spec or from the token. With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to the API, which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); bindings with the same via share one. url may then be left out and reads http://<name>.<namespace>.svc:<port>; a url that is set keeps its scheme and its path, and its host is the Host header and the name of the certificate, not the address that is dialled | shipped |
| `hatchet.workers` | 1 | BackgroundWorkerFacet | the Hatchet REST API: the worker list, running and completed task runs and the tenant task-stats (queue-metrics, then step-run-queue-metrics, on servers without it) | workers_online, workers_total, pool_used, pool_max (slots), active (running tasks), waiters (queued tasks, + pending where the server reports it), running_s (longest running task), p95_s (task duration over the last hour); TaskRunning past long_task, PoolExhausted, NotReady when listed workers are all inactive (an empty list concludes nothing: workers_note says so); detail: workers, long tasks, queue_source, workers_note, url, via | `url`, `token_env`, `tenant`, `interval`, `timeout`, `via`, `kubeconfig`, `context`, `name`, `long_task` | a Hatchet API token in the environment variable named by token_env (default HATCHET_CLIENT_TOKEN); the tenant id from the spec or from the token. With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to the API, which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); bindings with the same via share one. url may then be left out and reads http://<name>.<namespace>.svc:<port>; a url that is set keeps its scheme and its path, and its host is the Host header and the name of the certificate, not the address that is dialled | shipped |
| `hatchet.workflow` | 1 | ScheduledJobFacet | the Hatchet REST API: the workflow list, per-workflow task metrics (read beside the polls, at most once a minute, with a timeout of their own), the latest workflow runs and the cron triggers | succeeded, failed, active, queued, cancelled over the window that answered, rate (finished runs per second over it), running_s; JobFailed when the latest finished run failed, JobRunning while a run is running; a job event per failed run; detail: window (the window the counts cover, 24h falling back to 6h and 1h when counting takes too long), counts_at, latest_status, last_run, last_success, last_failure, schedule, cron, workflow_id, url, via | `url`, `token_env`, `tenant`, `interval`, `timeout`, `via`, `kubeconfig`, `context`, `workflow`, `window` | a Hatchet API token in the environment variable named by token_env (default HATCHET_CLIENT_TOKEN); the tenant id from the spec or from the token. With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to the API, which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); bindings with the same via share one. url may then be left out and reads http://<name>.<namespace>.svc:<port>; a url that is set keeps its scheme and its path, and its host is the Host header and the name of the certificate, not the address that is dialled | shipped |
| `hcloud.firewall` | 1 | FirewallFacet, TrafficFacet | the Hetzner Cloud API firewall resource | rules (count); on an edge with port set: allowed (1/0) and FirewallDenied when no inbound rule lets the port through; detail: rules (direction, protocol, port, source ips, description), applied_to. No events: terraform.state owns change markers | `name`, `id`, `token_env`, `port`, `protocol`, `interval`, `endpoint` | a read-only Cloud API token in the environment variable named by token_env (default HCLOUD_TOKEN) | shipped |
| `hcloud.lb` | 1 | LoadBalancerFacet | the Hetzner Cloud API: load balancer targets' health and the load balancer metrics endpoint | connections, rate (requests per second), targets_healthy, targets_total (each server or IP once, however many targets name it), TargetUnhealthy when no target is healthy; per entry of targets an observation for the edge <lb>-><component> with healthy (1/0) and HealthCheckFailing; detail: services, targets, algorithm, location | `name`, `id`, `token_env`, `targets`, `interval`, `endpoint` | a read-only Cloud API token in the environment variable named by token_env (default HCLOUD_TOKEN) | shipped |
| `http.ping` | 0 | ExternalFacet, WorkloadFacet, ObservabilityFacet | an HTTP endpoint | latency_ms, error_rate and timeout_rate over the last 10 attempts, the last status, Timeout after 3 consecutive failures | `url`, `method`, `timeout`, `interval`, `expect_status` | outbound HTTPS to the endpoint; no credentials | shipped |
| `k8s.cronjob` | 0 | ScheduledJobFacet | Kubernetes API (cronjob and job informers) | active, succeeded, failed (last 24h), running_s; JobFailed, JobRunning | `namespace (required)`, `name (required)`, `kubeconfig`, `context` | get/list/watch on cronjobs and jobs in the namespace | shipped |
| `k8s.ingress` | 0 | IngressFacet, CertificateFacet | Kubernetes API (ingress informer); the certificate's expiry from cert-manager Certificates when installed, else from a TLS handshake with the ingress host; the TLS secrets only when read_tls_secret is true | cert_days; CertExpired, CertExpiring, CertRenewalFailed; cert (renewal) events | `namespace (required)`, `name (required)`, `read_tls_secret`, `kubeconfig`, `context` | get/list/watch on ingresses; list on certificates.cert-manager.io (optional); TCP access to the ingress hosts on port 443 (optional); get on the TLS secrets in the namespace only when read_tls_secret is true | shipped |
| `k8s.node` | 0 | NodeFacet | Kubernetes API (node, pod and event informers), metrics.k8s.io NodeMetrics and the kubelet /stats/summary through the API server proxy | cpu_pct, mem_pct, disk_pct, pods, killed; NotReady, MemoryPressure, DiskPressure, Rebooted; node (reboot) events | `name (required)`, `kubeconfig`, `context` | get/list/watch on nodes, pods and events; get on nodes.metrics.k8s.io; get on nodes/proxy for disk usage (optional) | shipped |
| `k8s.pvc` | 0 | StorageFacet | Kubernetes API (persistentvolumeclaim and pod informers) and the kubelet /stats/summary of the node mounting the claim | used_bytes, total_bytes, disk_pct (disk_pct only when the kubelet stats are readable) | `namespace (required)`, `name (required)`, `kubeconfig`, `context` | get/list/watch on persistentvolumeclaims and pods in the namespace; get on nodes/proxy for usage (optional) | shipped |
| `k8s.scrape` | 0 | TrafficFacet, IngressFacet | a counter on the Prometheus metrics page of every ready pod of a workload, read with a GET through the API server (pod informer, pods/proxy) | rate (per second, the counter's growth over the window summed over the pods; a counter that did not move is a rate of 0 only once it was watched for the whole window) and error_rate (the percentage of it that the series matching errors make up); detail: the pods read, the series matched, the window, span_s, rate_by_node and error_rate_by_node (per node, from the pods on it), label_values when match finds no series | `namespace (required)`, `selector (required)`, `port (required)`, `metric (required)`, `match`, `errors`, `path`, `scheme`, `interval`, `window`, `kubeconfig`, `context` | list/watch on pods and get on pods/proxy in the namespace, which reaches every port of its pods with a GET; the pods serve their metrics without credentials on that port | shipped |
| `k8s.workload` | 0 | WorkloadFacet | Kubernetes API (informers on pods, deployments, statefulsets, daemonsets, replicasets, events) and metrics.k8s.io PodMetrics | replicas_ready, replicas_desired, restarts, restart_window_s, cpu_pct, mem_pct, killed, evicted; CrashLoopBackOff, ImagePullBackOff, OOMKilled, Evicted; deploy and scale events; detail: placement (per node: pods, ready, restarts, and cpu_pct and mem_pct when the usage of its pods was read) | `namespace (required)`, `selector (label selector; required unless name and kind are set)`, `name`, `kind (Deployment|StatefulSet|DaemonSet)`, `kubeconfig`, `context` | get/list/watch on pods, replicasets, deployments, statefulsets, daemonsets and events in the namespace; get/list on pods.metrics.k8s.io | shipped |
| `kubelet.stats` | 0 | NodeFacet | the kubelet summary API (/stats/summary) of one node, through the API server proxy | cpu_pct, mem_pct, disk_pct of the node root and image filesystems, iops and used_bytes per volume, finer than metrics-server and without it; detail: per-filesystem capacity, inode pressure | `node`, `kubeconfig`, `context` | RBAC get on nodes/proxy for the kubeconfig's identity | spec only, not implemented yet |
| `pg.pool` | 2 |  | pgbouncer's admin console (SHOW POOLS, CONFIG, DATABASES, STATS) | pool_used, pool_max, waiters, queued, rate; PoolExhausted when every server connection is busy and clients wait; detail: pools table, maxwait, default_pool_size, max_client_conn | `dsn_env`, `host`, `port`, `user`, `database`, `sslmode`, `password_env`, `pool`, `via`, `interval`, `kubeconfig`, `context` | a connection to database "pgbouncer" as a stats_users or admin_users role: either a DSN in the environment variable named by dsn_env, or host, port, user and sslmode with the password in the variable named by password_env (taken as it is, nothing to encode; database defaults to pgbouncer). With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward, which needs create on pods/portforward; host and port may then be left out | shipped |
| `pg.stats` | 2 |  | pg_stat_activity, pg_stat_database, pg_stat_replication, pg_replication_slots and pg_database_size over a read-only connection | rate (transactions per second of the database of the connection, without the one each round of wassup ends with), connections_used, connections_max, active_connections, waiters, lag_bytes, wal_retained_bytes, used_bytes, disk_pct; on a replication edge (replica set): lag_bytes, streaming, wal_retained_bytes and ReplicationBroken/SlotInactive; detail: version, replicas, slots, top_waiting | `dsn_env`, `host`, `port`, `user`, `database`, `sslmode`, `password_env`, `via`, `replica`, `disk_total_bytes`, `interval` | a connection for a role that may log in: either a DSN in the environment variable named by dsn_env, or host, port, user, database and sslmode with the password in the variable named by password_env (taken as it is, nothing to encode). With via naming a tunnel, host and port may be left out. Every role reads connections, slots and sizes; state and lag of replicas and what other sessions do take pg_read_all_stats (or pg_monitor), and the detail says how to grant it | shipped |
| `redis.info` | 2 | CacheFacet | the Redis INFO command | mem_pct, hit_rate (per tick), evictions (per minute), clients; CacheFull when memory is at the limit and the policy is noeviction or the hit rate collapsed; detail: maxmemory, maxmemory_policy, used_memory_human, keys, keys_without_ttl, redis_version, via | `addr`, `url`, `host`, `port`, `user`, `password_env`, `db`, `tls`, `via`, `kubeconfig`, `context` | network access to the Redis port, named by url, by addr (host:port) or by host and port; a password in the environment variable named by password_env when AUTH is on (taken as it is, nothing to encode), and user for an ACL user. With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to the server, which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); bindings with the same via share one. The address may then be left out; one that is set is the name in the messages and in the certificate, not what is dialled | shipped |
| `redis.list` | 2 | QueueFacet | LLEN on one Redis list | depth; detail: key, via | `addr`, `url`, `host`, `port`, `key`, `user`, `password_env`, `db`, `tls`, `via`, `kubeconfig`, `context` | network access to the Redis port, named by url, by addr (host:port) or by host and port; a password in the environment variable named by password_env when AUTH is on (taken as it is, nothing to encode), and user for an ACL user. With via set to k8s.service/<namespace>/<name>:<port> (or k8s.pod/...) wassup opens its own port-forward to the server, which the identity of the kubeconfig has to be allowed to do in that namespace (get on services, get and list on pods, create on pods/portforward); bindings with the same via share one. The address may then be left out; one that is set is the name in the messages and in the certificate, not what is dialled | shipped |
| `s3.bucket` | 1 | StorageFacet | an S3-compatible object store (AWS S3, Hetzner Object Storage, MinIO, Spaces, R2): HeadBucket, one request a round; with list_objects: true also a paged ListObjectsV2 under the optional prefix, one request per 1000 objects a round | latency_ms of HeadBucket; NotReady when the bucket does not exist, ConnectionRefused when the endpoint does not answer; detail: region, endpoint. With list_objects: true also used_bytes and objects summed over the listing, last_write, and disk_pct when quota_bytes is set; a bucket with more than max_objects keys (default 20000) is not sized and reports only lower bounds in the detail. Without the listing the size and the object count are not read and not shown | `bucket`, `endpoint`, `region`, `list_objects`, `prefix`, `path_style`, `access_key_env`, `secret_key_env`, `session_token_env`, `quota_bytes`, `max_objects`, `interval`, `timeout` | read-only credentials in the environment variables named by access_key_env and secret_key_env (default AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY), allowed s3:ListBucket on the bucket, which HeadBucket and the listing both take. The check costs one request a round; the listing costs one more per 1000 objects, which a store that bills requests charges for | shipped |
| `signoz.edge` | 1 | TrafficFacet, DatabaseFacet | a counter SigNoz holds, such as the calls of one service to another, asked for with one read query per metric that the bindings share (POST /api/v5/query_range) | on an edge: rate (per second, the counter's growth over the window summed over the series that match) and error_rate (the percentage of it that the series matching errors make up); on a database: rate, its transactions per second; a rate of 0 when the series that match were counted within known (a day) and not within the window; no rate when no series matches in either; detail: the metric, the series matched, the window | `url (required)`, `metric (required)`, `match`, `errors`, `window`, `known`, `interval`, `user_env`, `password_env`, `token_env` | HTTP access to SigNoz, and a user with the viewer role: its name and password in the environment variables named by user_env and password_env (default SIGNOZ_USER, SIGNOZ_PASSWORD), or an API key in the one named by token_env. wassup signs in with a POST and asks with a POST; neither changes anything in SigNoz. A query costs SigNoz's database a read over the window, once per metric and interval, however many bindings use the metric; while the window holds nothing of a binding, one more over the last day (known) every ten minutes, in steps of 24 minutes | shipped |
| `signoz.health` | 1 | ObservabilityFacet | SigNoz's own health and ingestion metrics (otel-collector, query-service, ClickHouse) | ingest_rate (spans and metrics per second), disk_pct of the ClickHouse volume; NoData when the collector stops receiving, so every signoz.edge reads as no data instead of healthy | `url`, `token_env` | HTTP access to the SigNoz query service; a SigNoz API key in token_env when auth is on | spec only, not implemented yet |
| `terraform.state` | 0 |  | terraform show -json in a working directory | terraform events per added, removed or changed resource, the rules count of hcloud firewalls and FirewallDenied when no firewall allows inbound TCP to the edge's port | `dir`, `interval`, `watch`, `resource`, `port`, `binary` | the terraform binary, an initialised working directory and read access to its state backend | shipped |
