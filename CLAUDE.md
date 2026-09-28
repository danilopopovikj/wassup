# Building wassup

wassup is a terminal app that shows one live architecture diagram of a
Kubernetes-hosted system and answers three questions at a glance: where is it
stuck, since when, what changed. This file is for anyone (human or agent)
changing the code. Read it before touching anything; it encodes decisions
already taken so they are not re-litigated in every session.

## Principles

1. **One diagram.** The general view and the issue view are the same picture,
   filtered. Never add a second screen, tab or mode to learn.
2. **Six states, one glyph and one color each.** flowing, idle, waiting,
   processing, blocked, failing. Two markers describe wassup itself, not the
   system: unbound (no probe data) and stale (data older than 3 ticks), plus
   "no data" on edges when the traffic source is down. Nothing else may
   introduce a state, a glyph or a color.
3. **Plain words.** Labels read "waiting, 38 queued, at the database", never
   "CrashLoopBackOff". Present tense, lowercase, the number first, the
   location last. Kubernetes vocabulary goes in the detail panel and
   `explain`, never on the diagram. The acceptance test is a person who has
   never used Kubernetes saying, in their own words, what is wrong. If they
   cannot, the label is wrong, not the person. `skill/wassup/reference/style.md`
   is the style guide.
4. **High signal by default.** The engineer on call needs cpu, ram, disk and
   whatever is amber or red; everything else waits behind `d` (detail level)
   or `enter` (panel). A gauge that reads zero says nothing and is hidden. A
   rate on a health-check edge is noise. Before adding anything to a box or
   an edge, ask what decision it changes.
5. **Files, not sockets.** Everything Claude Code and wassup exchange is a
   file in `.wassup/`, versioned, schema-validated, written atomically.
   No daemon, no MCP server, no network between the two. wassup needs no AI
   to run.
6. **Never fabricate.** A number the provider did not report is omitted
   (`facet.Num` unset), never written as 0. A component with no probe data is
   drawn unbound, never healthy. Idle, unbound, stale and no data must look
   different at a glance.
7. **Layout is the user's.** Boxes never jump on their own. `layout.json`
   belongs to the user; code writes it only from a drag, a resize, a collapse
   or a detail toggle, and `sync --apply` never touches it.
8. **Extensible by type, not by special case.** New infrastructure comes in
   as a probe that fills a facet, and, only when it is a genuinely new shape,
   as a catalog type with a facet named after it. The renderer, the state
   engine and the story never learn a product name.
9. **Deterministic and offline-testable.** Layout, routing, the engine and
   discovery are pure functions over files. The twelve recorded
   scenarios are the acceptance suite; every behavior change is a fixture
   change first.
10. **Keep it simple.** Standard library first; a dependency needs a reason
    that survives a code review. No frameworks for probes, no plugin loaders,
    no reflection tricks. Small functions, doc comments that say why.

## One vocabulary

The same word names the node type in `topology.yaml`, the Go facet, the
catalog entry and the docs. The type is the lowercase word, the facet is
`<Type>Facet`. No abbreviations, no ambiguous words (Gate, Record, Service
were rejected for a reason).

| Type in YAML | Facet | Edge kind | Facet |
| --- | --- | --- | --- |
| `node` | `NodeFacet` | http, grpc, sql, queue, cache, tcp, external | `TrafficFacet` |
| `workload` | `WorkloadFacet` | replication | `ReplicationFacet` |
| `backgroundworker` | `BackgroundWorkerFacet` | | |
| `queue` | `QueueFacet` | | |
| `scheduledjob` | `ScheduledJobFacet` | | |
| `loadbalancer` | `LoadBalancerFacet` | | |
| `ingress` | `IngressFacet` (+ `CertificateFacet`) | | |
| `firewall` | `FirewallFacet` | | |
| `dns` | `DNSFacet` | | |
| `database` | `DatabaseFacet` (+ `ReplicationFacet`) | | |
| `syncengine` | `SyncEngineFacet` (+ `ReplicationFacet`) | | |
| `cache` | `CacheFacet` | | |
| `storage` | `StorageFacet` | | |
| `observability` | `ObservabilityFacet` | | |
| `external` | `ExternalFacet` | | |

Older spellings (`lb`, `db`, `job`, `worker`, `sync`) are normalized on load
by `model.TypeAliases`; do not use them in new code, fixtures or docs.
Conditions (`CrashLoopBackOff`, `PoolExhausted`, `ReplicationBroken`, …) and
metric keys (`cpu_pct`, `depth`, `pool_used`, `lag_bytes`, …) are listed in
`internal/probe/README.md`; add to those lists, never invent a parallel one.

## Architecture in one paragraph

Probes (`internal/probe/*`) read a provider and fill a facet
(`internal/probe/facet`), which writes the canonical observation. The binder
(`internal/bind`) joins observations to components and edges. The state engine
(`internal/state`) turns each joined view into exactly one state, a label, a
severity, and computes the issue lens: lit path, cause, story. History
(`internal/history`) keeps compact frames for the timeline, trends and
baselines. Layout (`internal/layout`) places lanes (edge, machines, compute, data,
side) and routes edges with an orthogonal A*. The machines row holds the
nodes in topology order; a node is a frame, and every component that runs on
it (`runs_on`, confirmed by the live pod placement) is drawn as an instance
box inside it, so the picture shows which API and which worker sits on which
server. Edges attach to the instances; the routes of one logical edge share
a trunk (an api on three nodes reaches the database as one bundle), and a
route never crosses a machine's header or rides along a frame border. The renderer (`internal/render`) paints a canvas and hosts the
Bubble Tea model. Discovery (`internal/discover`) reads Terraform, manifests,
Helm values, `.env`, code and the cluster into evidence and proposes topology
and bindings. The runtime (`internal/app`) wires it all and hot-reloads
`.wassup/`. The CLI (`cmd/wassup`) exposes every verb with `--json`.
`internal/scenario` replays fixtures through the same path the TUI uses.

## The provider boundary

- Kubernetes is one provider. Nothing outside `internal/probe/k8s` and
  `cmd/wassup/kube.go` may import client-go.
- A probe declares the facets it fills in `Access.Facets`; `wassup validate`
  checks a binding by facet. A probe never emits a metric key or condition
  the facet does not know.
- Credentials come from env vars named in the spec (`*_env`) or the
  kubeconfig, never from `.wassup/`.
- Probes re-emit every tick even when nothing changed, so stale can be told
  from idle. On read errors they send `Observation{Err}` and degrade their
  health; they never keep reporting yesterday's numbers as today's.

## Discovery and sync

- `wassup discover --propose --write` proposes; it never edits `.wassup/`.
  Every proposed component and edge cites `file:line`. Unresolved hosts are
  listed, not guessed.
- `wassup sync` diffs; `--apply` adds components, edges and bindings and
  leaves labels, groups, notes and layout alone; `--prune` is the only thing
  that removes. Match existing components by id, then by label, then by
  role instances, before calling anything "added".
- The scanner is evidence, the skill is judgement: when a pattern is too
  fuzzy for code (a custom framework), leave it to the skill's Setup
  checklist rather than adding heuristics that guess.

## How to

**Add a provider or data source.** Write a probe under `internal/probe/<name>`
(one package per family), fill the matching facet, register in `init()` with
`Access{Facets: ...}`, import it from `internal/probe/all`. Tests use
`httptest` or fake clients, never a live service. Add the kind to the type's
`Probes` list in `internal/model/catalog.go` (documentation) and regenerate
`skill/wassup/reference/probes.md` with `wassup probes --markdown`.

**Add a shape.** Only when no facet fits. Add the type to
`internal/model/catalog.go` (lane, gauges, `Facets`), the schema enum in
`internal/model/schema/topology.json`, a `<Type>Facet` and `Emit<Type>` in
`internal/probe/facet`, the engine's label rules if the six states need a
type-specific phrase, the render `primaryGauges` entry, a discovery mapping,
the catalog doc, and a scenario.

**Change a label or the story.** Change the fixture's `expected.yaml` in
`testdata/scenarios/gen.py` first, regenerate, watch it fail, then change
`internal/state`. Labels are built in `engine.go`, sentences in `lens.go`.

**Add a scenario.** Extend `testdata/scenarios/gen.py` (records hold until
superseded; relative times), run it, and keep `expected.yaml` to what the
diagram must say. The same fixtures feed `wassup demo` and the screenshots
(`WASSUP_SHOTS=<dir> go test ./internal/render/ -run TestShots`).

**Add a CLI verb.** `cmd/wassup/commands.go`, accept `--json`, exit non-zero
with a machine-readable list on validation errors, update the README table and
`skill/wassup/SKILL.md` when Claude Code should use it.

## Working agreements

- `make lint` (gofmt, vet) and `make test` green before every commit; never
  commit a tree that does not build.
- `go mod tidy` drops modules nothing imports yet; when several people or
  agents add packages in parallel, add the dependency and an import in the
  same change and do not tidy mid-flight.
- Parallel agents own whole directories; nobody edits another's package
  while it is in progress.
- Commit messages say what changed and why in prose; no model or tool names
  in code, comments or commit bodies.
- Fixtures, examples, screenshots and docs are public. They use invented
  names (the example system is `bookstore`) and reserved domains such as
  `bookstore.example`, never the name or hostname of a real system.
- Memory of decisions lives in this file. When a principle changes, change
  it here in the same commit.
