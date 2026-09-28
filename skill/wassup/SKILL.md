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
owns positions. Never write secrets into `.wassup/`; probes read credentials
from env vars named in their spec (`*_env`) or from the kubeconfig.

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

1. Ask for the kubeconfig path and context only if they are not obvious from
   the repo or env (`KUBECONFIG`, `WASSUP_KUBECONFIG`, `WASSUP_CONTEXT`).
2. Run `wassup discover --kubeconfig ... --context ... --json` for the raw
   inventory. Read terraform (`*.tf`, `terraform show -json`), kustomize and
   helm sources for load balancers, firewalls, DNS, queues, external APIs.
3. Write `.wassup/topology.yaml`:
   - `version: 1`, a short `name`.
   - Groups: `cloud`, `region`, `cluster`, `namespace`, `zone`; nest with `parent`.
   - Components: only catalog types (`reference/catalog.md`). One `workload`
     per Deployment/StatefulSet/DaemonSet, never per pod. Nodes are `node`;
     put `runs_on: [node ids]` on workloads. A Postgres cluster is one `db`
     with `roles: {primary: ..., replicas: [...]}`; the role instances become
     components automatically, do not declare them. Anything outside your
     control is `external`. The traffic source (SigNoz, Prometheus) is
     `observability`.
   - Background work: Hatchet queues and workflows, Celery queues and
     workers, and Electric SQL have a documented mapping at the end of
     `reference/catalog.md`; follow it rather than inventing types.
   - Edges: `from`, `to`, `kind` in http, grpc, tcp, sql, replication, queue,
     cache, external. Include the load balancer to node edges if the LB
     health checks nodes, and firewall edges for paths a firewall gates.
   - Ids are stable lowercase slugs. Labels are what a CS person would say.
4. **Stop and show the topology to the user** before writing bindings.
5. Write `.wassup/bindings.yaml`: one entry per component or edge id, each a
   list of probes (`reference/probes.md`). Bind every component; an unbound
   component is drawn hatched and never as healthy.
6. Run `wassup validate`, then `wassup probe --once`. Fix every unbound
   element: wrong namespace, wrong selector, missing RBAC, missing env var.
   Report what stays unbound and why.
7. Optionally write `thresholds.yaml` overrides and run the Scan workflow.
8. Commit `.wassup/` (`state/` is gitignored).

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
node-1"): verify against `wassup discover --json`, edit `topology.yaml` and
`bindings.yaml` minimally, run `wassup validate`, and report what moved.
Renaming an id is `wassup remap <old> <new>`; it migrates layout, findings,
thresholds and open annotations. The TUI picks changes up through fsnotify
and keeps every saved position.

## Reading the snapshot

`state/snapshot.json`: `components{id: {state, marker, label, severity,
metrics, conditions, since, gauges, notes}}`, `edges{id: {state, label, rate,
queued, error_rate, since}}`, `probe_health{kind: ok|degraded|failed}`,
`issues[{id, cause, severity, path, story}]`. Markers `unbound`, `stale` and
`nodata` describe wassup, not the system: treat them as "unknown", never as
healthy.
