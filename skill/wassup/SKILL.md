---
name: wassup
description: Set up, read, explain, annotate and repair the wassup architecture diagram of a Kubernetes-hosted system. Use when a prompt contains a wassup:// ref, asks to set up / remap / rescan / annotate wassup, or asks any infrastructure question in a repository that has a .wassup/ directory.
---

# wassup

wassup is a terminal app that shows one live architecture diagram of a
Kubernetes-hosted system. Every component and edge is always in exactly one of
six states: **flowing, idle, waiting, processing, blocked, failing**. Labels
are plain words ("waiting, 38 queued, at the database"), never Kubernetes
vocabulary. You and wassup exchange files under `.wassup/`; nothing else.

Files you write: `topology.yaml`, `bindings.yaml`, `findings.yaml`,
`thresholds.yaml`, `state/annotations.json` (through `wassup annotate`).
Files you read: `state/snapshot.json` (through `wassup explain`,
`wassup snapshot`), `state/events.jsonl`. Never edit `layout.json`: the user
owns positions. Probes read credentials from env vars named in their spec
(`*_env`) or from the kubeconfig. The one place for a credential is
`.wassup/local.env`, which git ignores and the user fills in; never write
one into any other file, and never read or print what `local.env` holds.

wassup only reads, and so do you while you work on it. Never change the
cluster or a database to make a probe work: no `kubectl apply`, `create`,
`patch`, `delete`, `scale` or `exec`, no SQL other than `SELECT` and `SHOW`,
no Redis command that writes. When a probe lacks access (a role, a grant, an
RBAC rule), say what is missing and let the user add it.

Reference material in this skill:

- `reference/catalog.md`: the component types, lanes, gauges and edge kinds.
- `reference/probes.md`: every probe, its spec fields, what it needs, how to test it.
- `reference/style.md`: the label and story language rules.
- `reference/schema/*.json`: JSON Schemas for topology, bindings, findings, annotations, thresholds, layout.
- `reference/examples/`: a full `.wassup/` for k3s + CNPG + Hetzner, and one for a managed cloud cluster.

All CLI commands accept `--json` and exit non-zero on validation errors with a
machine readable list.

## Ref handling

When a prompt contains `wassup://<type>/<id>`, `wassup://edge/<from>-><to>` or
`wassup://group/<id>`, that element is the primary subject. Resolve it with
`wassup explain <ref> --json` **before reading any code**, and address the
element by its label in replies ("the database", "the API"), not by its id.
A ref with `@<timestamp>` means the user was scrubbing the timeline: explain
then reads the matching history frame and says so; reason about that moment,
not about now.

## Workflow: Setup

Setup goes in tiers, and you stop for the user between them. Each tier
reaches further into the system than the one before, so the user decides
how far you go, with the result of the last tier in front of them.

| Tier | Needs | Probes |
| --- | --- | --- |
| 0 | the kubeconfig and the network, nothing else | `k8s.*`, `cnpg.*`, `dns.record`, `cert.tls`, `http.ping` |
| 1 | a token or a key for an API | `hatchet.*`, `electric.sync`, `hcloud.*`, `amqp.queue`, `s3.bucket` |
| 2 | a connection to a data store | `pg.stats`, `pg.pool`, `redis.*`, `celery.queue` |

1. **Ask three questions before you run anything.** Do not find the
   answers out by running into them.
   - Which environment? `wassup discover --no-cluster --json` lists the
     environments the repository has (overlays, values files, Terraform
     directories). Draft one, the one the user names.
   - Which kubeconfig and which context? The default context of the machine
     is the cluster somebody worked on last and may belong to another
     project. `wassup discover` prints the cluster it is about to read
     (kubeconfig, context, server, number of nodes) and, when nobody named
     it, stops with exit code 5 and lists the kubeconfigs it found in the
     repository. Show that list to the user and let them pick.
   - How far may the probes reach: tier 0, 1 or 2? Say what each tier
     contacts. If the user wants a read-only account before anything reads
     the cluster, that is step 7, and it can come first.
   Write the kubeconfig and the context into `.wassup/local.env`
   (`WASSUP_KUBECONFIG`, `WASSUP_CONTEXT`). wassup reads that file on every
   run and git ignores it, so nothing is exported and no command carries a
   flag. Credentials go into the same file, and **the user** puts them
   there: tell them the names of the variables, never read, print or copy a
   value yourself.
2. Run `wassup discover --propose --write --environment <name>`. It reads
   Terraform, Kubernetes manifests, Helm values, `.env` files and
   application code, merges the live cluster's inventory (workloads with
   their env vars and commands, services, ingresses, network policies, CNPG
   clusters, cron jobs), and writes `.wassup/proposed/topology.yaml`,
   `bindings.yaml`, `review.md` and `evidence.json`, and makes sure
   `.wassup/.gitignore` ignores `proposed/`, `state/` and `local.env`. The
   evidence holds the names of environment variables and the hosts they
   point at, never the value of a credential and no URL with a user or
   password in it. The scan follows `envFrom` into ConfigMaps, skips what
   `.gitignore` ignores, lockfiles, test directories, nested worktrees and
   the environments that were not asked for, and notes what it skipped.
3. **Review `proposed/review.md`, do not trust the proposal blindly.** It
   has one line and one citation per component and edge, and says for each
   how sure the scan is and what it rests on: `cluster` is what the running
   system proves (a workload that runs, a variable on it that points at a
   Service that exists, a network policy that allows a host), `repository`
   is what files hint at. Start at the bottom: check every `low` line
   against its citation and drop what you cannot back. An egress allowlist
   (`NetworkPolicy`, Cilium `toFQDNs`) is the best list of outside services
   there is; a host in code that no policy allows is probably not called in
   this environment. Open `evidence.json` only for the lines you doubt. For
   each edge confirm the mechanism:
   - `sql`: a `DATABASE_URL`/`PGHOST` or DSN pointing at the database (for
     CNPG, `<cluster>-rw` is the primary, `-ro`/`-r` are replicas).
   - `replication`: a logical replication consumer such as Electric SQL
     (`ELECTRIC_DATABASE_URL`, a `postgresql_replication_slot` or
     publication in Terraform, `ELECTRIC_REPLICATION_SLOT`). The edge goes
     from the primary instance to the consumer.
   - `queue`: a broker URL (`CELERY_BROKER_URL`, `amqp://`), a `celery
     worker -Q ...` command, a Hatchet queue or workflow.
   - `cache`: a `REDIS_URL` used for caching (a Redis used only as a broker
     is a `queue`).
   - `http`/`grpc`: an in-cluster URL or `HATCHET_CLIENT_HOST_PORT`.
   - `external`: a public host in an egress allowlist, in code or in env
     (`api.stripe.com`).
   Then read the `unresolved hosts` list: each is a connection the scanner
   saw but could not place; add the component or fix the host.
4. Move `proposed/topology.yaml` to `.wassup/topology.yaml`, editing ids and
   labels into words a CS person would say, keeping only catalog types
   (`reference/catalog.md`); the mapping for Hatchet, Celery and Electric is
   at the end of that file. One `workload` per Deployment, never per pod.
   A Postgres cluster is one `database` with `roles`; the instances come free.
5. **Stop and show the picture to the user.** Run `wassup validate`, then
   `wassup export`. It prints the paths of the rendered diagram (`.svg`,
   `.png` when a converter is installed, `.txt`). Show the diagram itself:
   print the `.txt` file in your reply and name the `.svg` so the user can
   open it. A description of the diagram is not the diagram. Every box is
   drawn unbound at this point; that is expected. Wait for the user's
   answer before you write bindings.
6. Bind one tier at a time. Take the bindings of the tier from
   `proposed/bindings.yaml` into `.wassup/bindings.yaml`, then run
   `wassup validate` and `wassup probe --tier <n> --json`, fix what is
   unbound, and **stop**: tell the user what is bound, what is not and why,
   and what the next tier will contact. Go on only when they say so.
   - **Tier 0.** The Kubernetes probes, DNS, certificates, pings. Nothing
     but the kubeconfig is used. After it the diagram shows every box that
     runs in the cluster with live data.
   - **Tier 1.** Tell the user which variables to put into
     `.wassup/local.env` (`token_env`, `secret_env`, `access_key_env` name
     them). `s3.bucket` checks that the bucket answers and nothing more;
     `list_objects: true` sizes it and lists every object to do so, one
     request per thousand: ask before you set it.
   - **Tier 2.** A database inside the cluster: set
     `via: k8s.service/<namespace>/<service>:<port>` on `pg.stats` and
     `pg.pool` and wassup opens its own port-forward; never ask the user to
     run `kubectl port-forward`. Name the connection by its parts, `user`,
     `database`, `sslmode` and `password_env`, so nobody builds a URL or
     encodes a password by hand; with `via` the host and the port come from
     the tunnel. `dsn_env` still works for a connection string that exists
     already.
   While you fix one binding, test that one: `wassup probe <id>` runs the
   bindings of one element and prints every value they read. Read
   `probe_results` of every element: `bound` means a probe delivered data in
   this run, `UNBOUND` comes with the probe and its error, `later` means the
   bindings belong to a tier that did not run, and a bound element can still
   list a probe that failed. The errors say what to do; do that first.
   - `pg.stats` works with the application's own database role. Do not ask
     for a new role: without `pg_read_all_stats` the lag comes from the
     replication slots and the detail says so, with the grant to run if the
     user wants the rest.
   - `replica:` on a replication edge is the slot name, the
     `application_name` or the CloudNativePG instance name.
   - Never add `table` to an `electric.sync` binding on your own: it makes
     Electric run a snapshot query over the whole table. Ask the user for a
     small table first (`reference/probes.md`).
   - `k8s.ingress` does not read TLS secrets unless `read_tls_secret: true`
     is set; leave it off unless there is no cert-manager Certificate and
     the ingress host cannot be reached for a handshake.
   - **Say what is not there yet.** `signoz.edge`, `signoz.health` and
     `kubelet.stats` are documented and not implemented (`wassup probes`
     says `spec only`). `signoz.edge` is where traffic rates come from, so
     until it ships most edges read `no rate measured`. Tell the user this
     before they take it for a fault; `wassup validate` warns about every
     binding of such a probe.
7. A read-only account, whenever the user asks whether wassup can harm the
   system or has only an admin kubeconfig: `wassup access` prints the
   smallest ServiceAccount and roles for the probes that are bound (get,
   list and watch on what they read, the port-forward only in the
   namespaces a `via` reaches into, never secrets, `nodes/proxy` or exec)
   and the commands that write a kubeconfig with a token that expires. It
   contacts nothing. The user runs the commands; you do not.
8. Delete `.wassup/proposed/`, optionally write `thresholds.yaml`, run the
   Scan workflow, commit `.wassup/` (`state/`, `proposed/` and `local.env`
   are gitignored).

## Workflow: Update (keep the diagram true as the system changes)

Run `wassup sync` whenever infrastructure or manifests change, or when the
user says something is missing. It re-runs discovery and diffs the
proposal against the committed files:

- `+ component`/`+ edge` with evidence: something new in the repo or
  cluster. Review the evidence as in Setup step 3.
- `- component`: no longer found. Confirm it is really gone before pruning.
- `+ binding`: an existing component is unbound and the scanner knows a
  probe for it.
- `~`: a type or edge kind that disagrees between the repo and the diagram.

Then `wassup sync --apply` merges the additions (labels, groups, notes and
`layout.json` are never touched; new boxes take auto positions), and
`wassup sync --apply --prune` also removes what vanished. Finish with
`wassup validate` and `wassup probe`. `wassup sync --check` exits 4
on drift, so it can run in CI or a cron to flag a diagram that fell behind.

## Workflow: Explain

Given a ref or a symptom:

1. `wassup explain <ref> --json` first. It returns the binding, the current
   and recent states, metrics, conditions, change markers, attached findings,
   related edges, the lens story, and for workloads the last log lines, for
   databases the top waiting queries. Under 200 lines.
2. `wassup logs <ref> --since 15m` and `wassup events <ref> --since 2h` when
   you need more. `wassup snapshot --json` for the whole picture.
3. Form one hypothesis. Follow the cause rule wassup itself uses: the deepest
   element on the lit path, then the worst severity, then a saturated gauge
   over a downstream failure, then the nearest change marker.
4. Draw it: `wassup annotate --path <ref,ref,...> --note "<one sentence, style.md>" --confidence low|medium|high`.
   The user sees the path highlighted and the note on the diagram.
5. Propose the fix as concrete commands or a manifest diff. Never run
   anything that writes to the cluster without the user's explicit go.
6. After the fix: `wassup snapshot --watch <ref> --until healthy --timeout 5m`
   confirms, then `wassup clear-annotations` or a new annotation with the
   outcome.

## Workflow: Scan

Read the repository for these finding categories and write
`.wassup/findings.yaml` (schema in `reference/schema/findings.json`), one
entry per finding with `id`, `severity` (critical, high, medium, low),
`title`, `component`, `file`, `line`, `evidence`, `suggested_fix`, `found_at`:

- missing resource requests or limits
- no liveness or readiness probes
- a single replica for a stateful service
- secrets in manifests
- no PodDisruptionBudget
- `latest` image tags
- no resource quotas
- terraform firewall rules wider than needed
- missing backup configuration on CNPG clusters
- missing time limits on Celery tasks
- no retry or timeout on external calls

Attach each finding to a component id. Run `wassup validate` afterwards.
Findings show in the TUI panel (`f`) and in `explain`.

## Workflow: Remap

When the user says the diagram is wrong ("replica-2 is on node-3, not
node-1"): verify against `wassup discover --json` (or `wassup sync`), edit
`topology.yaml` and `bindings.yaml` minimally, run `wassup validate`, and
report what moved.
Renaming an id is `wassup remap <old> <new>`; it migrates layout, findings,
thresholds and open annotations. The TUI picks changes up through fsnotify
and keeps every saved position.

## Reading the snapshot

`state/snapshot.json`: `components{id: {state, marker, label, severity,
metrics, conditions, since, gauges, notes}}`, `edges{id: {state, label, rate,
queued, error_rate, since}}`, `probe_health{kind: ok|degraded|failed}`,
`issues[{id, cause, severity, path, story}]`. Markers `unbound`, `stale` and
`nodata` describe wassup, not the system: treat them as "unknown", never as
healthy and never as failing. An element whose probes failed reads
`no data, <reason>`; the whole error of every failed probe is in
`detail.probe_errors`, also on an element that other probes keep bound.
