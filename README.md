# wassup

One live architecture diagram of a Kubernetes-hosted system, in the terminal.
It answers three questions at a glance: **where is it stuck, since when, what
changed.** It is the first layer of visual support for an on-call engineer who
diagnoses with Claude Code in the neighbouring tmux pane.

```
 bookstore  1 warn, 1 crit  probes ●●●●  tick 2s ago      lens 1/1 · cause: Database     10:31:07
╔═ Hetzner · cloud ═══════════════════════════════════════════════════════════════════════════════╗
║  ╭──────────────────────╮        ╭──────────────────────╮        ╭──────────────────────╮        ║
║  │ ● Load balancer   lb │──────▶│ ● Ingress    ingress │──────▶│ ● API       workload │        ║
║  │ flowing, 1.1k conn/s │1.1k req/s flowing, 1.1k req/s │        │ flowing, 1.1k req/s  │        ║
║  ╰──────────────────────╯        ╰──────────────────────╯        ╰──────────┬───────────╯        ║
║                                                                   38 queued •••••                ║
║                                                                  ╭──────────▼───────────╮        ║
║                                                                  │ ● Database   primary │        ║
║                                                                  │ conn ████████ 100/100│        ║
║                                                                  ╰──────────────────────╯        ║
 story · API → Database
   requests arrive, 1.1k/s
   load balancer passes them to 3 of 3 nodes
   API is waiting, 38 queued, at the database
 ▸ database is at 100 of 100 connections, CPU 35 percent, so the pool is the limit, not the database
```

![connection pool exhausted: the lens lights the path from the load balancer to the database, the panel explains the edge](docs/screenshots/02-pool-exhausted-panel.png)

More frames from the recorded scenarios: [node out of memory](docs/screenshots/04-node-memory-lens.png),
[node dropped from the load balancer, timeline strip](docs/screenshots/07-node-dropped-timeline.png).
`WASSUP_SHOTS=<dir> go test ./internal/render/ -run TestShots` regenerates them as SVG.

wassup is not an agent, not a dashboard and not a kubectl replacement.
Claude Code writes its configuration, reads its snapshots and draws on it;
wassup itself needs no AI to run. MIT, single binary, runs in tmux, no daemon.

## Principles

- **One diagram.** The general view and the issue view are the same picture, filtered.
- **Six states.** Every node and edge is always in exactly one of: flowing, idle, waiting, processing, blocked, failing. One glyph and one color per state, everywhere.
- **Plain words.** Labels read "waiting, 38 queued, at the database", never "CrashLoopBackOff".
- **Files, not sockets.** Everything Claude Code and wassup exchange is a file in `.wassup/`.
- **Watch, don't poll.** Kubernetes data comes from informers; metrics tick every 5 seconds; the screen redraws at up to 10 fps.
- **Layout is stable.** Boxes never jump on their own. Positions live in `layout.json`, which the user owns.
- **Extensible by type.** New infrastructure is a probe implementing one interface plus a catalog type, never a renderer change.

## Install

```sh
go install github.com/danilopopovikj/wassup/cmd/wassup@latest
# or build here
make build
```

Try it with no cluster at all:

```sh
wassup demo            # replays the connection-pool scenario
wassup demo 4 --speed 5
wassup demo --list
```

## Setting up a real system

```sh
cd your-repo
wassup                 # first run: prints the setup prompt and copies it to the clipboard
```

Paste the prompt into Claude Code. It installs the skill (`wassup skill
install`), discovers the cluster (`wassup discover --json`), writes
`.wassup/topology.yaml` and `.wassup/bindings.yaml`, and checks them with
`wassup validate` and `wassup probe --once`. Then run `wassup`.

The `.wassup/` directory is the only channel between you, Claude Code and the
TUI:

| File | Written by | Read by | Purpose |
| --- | --- | --- | --- |
| `topology.yaml` | Claude Code | TUI, CLI | components, groups, edges |
| `bindings.yaml` | Claude Code | TUI, CLI | probes attaching each component and edge to live data |
| `findings.yaml` | Claude Code (scan) | TUI, CLI | repo and infra issues attached to components |
| `thresholds.yaml` | Claude Code, you | TUI | optional overrides of the default thresholds |
| `layout.json` | TUI (drag, resize, collapse) | TUI | positions you own |
| `state/snapshot.json` | TUI, every tick, atomic | `wassup explain` | current state of everything |
| `state/history/<date>.jsonl` | TUI | timeline, CLI | one compact frame per tick, 24h at full resolution, 7 days rolled up |
| `state/events.jsonl` | TUI | TUI, CLI | change markers: deploys, terraform applies, reboots, scale events |
| `state/annotations.json` | `wassup annotate` | TUI | highlighted paths and notes |

## Keys

`←↑→↓`/`hjkl` move · `enter` detail · `tab` panel · `i` issue lens · `n` next
issue · `t` scrub the timeline, `[` `]` step, `esc` live · `c` copy the
`wassup://` ref, `C` ref plus summary, `y` the panel · `f` findings · `e`
changes · `a` annotations · `x`/`X` clear annotations · `s` export svg/png/txt
· `g` collapse group · `d` detail level (minimal, normal, full) · `r`/`R`
reset layout · `/` filter · `?` all keys.
Mouse: click selects, drag moves, drag a corner resizes, click a group title
collapses it.

## CLI

| Command | Does |
| --- | --- |
| `wassup` | runs the TUI; first run prints the setup prompt |
| `wassup init [--print-prompt]` | creates `.wassup/` or prints the prompt |
| `wassup discover` | raw inventory from the kubeconfig |
| `wassup validate` | schemas plus id cross-checks |
| `wassup probe --once` | runs every binding once, reports bound/unbound |
| `wassup snapshot [--watch <ref> --until <state> --timeout <d>]` | prints or waits |
| `wassup explain <ref> [@time]` | everything about an element, under 200 lines |
| `wassup logs <ref> [--since 15m]` | container logs merged across pods |
| `wassup events <ref> [--since 2h]` | change markers and Kubernetes events |
| `wassup annotate --path <ref,...> --note <text> [--confidence low\|medium\|high]` | draws on the diagram |
| `wassup clear-annotations` | removes all annotations |
| `wassup export [--png] [--svg]` | renders the diagram to files |
| `wassup remap <old-id> <new-id>` | renames an id across all files |
| `wassup replay <fixture-dir> [--check]` | runs against recorded observations |
| `wassup skill install` | copies the skill into `.claude/skills/wassup/` |
| `wassup demo [n]` | replays a bundled scenario |
| `wassup probes` | lists the probe kinds this build ships |

Every command accepts `--json`.

## Background work and sync engines

Hatchet, Celery and Electric SQL map onto the catalog without new
renderer code: Hatchet queues and workflows (`hatchet.queue`,
`hatchet.workflow`), worker pools with slots and stuck tasks
(`hatchet.workers`, `celery.worker`), Celery queues on Redis or RabbitMQ
(`celery.queue`, `amqp.queue`), and Electric as a `sync` component whose
replication slot is watched through `pg.stats` (`electric.sync`). Scenarios
11 and 12 record a Hatchet backlog with every slot busy and an Electric slot
gone inactive. The mapping is spelled out in `skill/wassup/reference/catalog.md`.

## The ten scenarios

The build is accepted against ten recorded scenarios in
`testdata/scenarios/`: deploy crash loop, connection pool exhausted, primary
disk filling from an inactive replication slot, node memory pressure, TLS
certificate not renewed, Celery backlog with a stuck worker, node dropped out
of the load balancer, firewall change blocked observability, cache full, and
an external dependency down. `go test ./...` replays all ten; `wassup replay
--check testdata/scenarios/<n>` does one; `wassup replay testdata/scenarios/<n>`
plays it in the TUI.

## Repository layout

```
cmd/wassup/          main, subcommands (cobra)
internal/model/      schemas, types, validation, ids, refs, remap
internal/probe/      Probe interface, registry, one package per probe family
internal/bind/       joins observations to components and edges
internal/state/      six-state engine, severity, labels, cause rule and story
internal/history/    snapshot writer, history frames, events, trends
internal/layout/     lanes, barycenter ordering, orthogonal A* router
internal/render/     Bubble Tea model, canvas, panels, timeline, export
internal/explain/    the explain command
internal/app/        runtime: probes, tick loop, persistence, hot reload
internal/scenario/   offline replay harness used by tests and --check
skill/wassup/        SKILL.md and reference files (embedded, `skill install`)
prompts/             setup.md (embedded)
testdata/scenarios/  fixtures 01 to 10 (gen.py regenerates them)
examples/            full .wassup/ examples
```

## Development

```sh
make test          # go test ./...
make lint          # gofmt + go vet
make demo          # build and run the demo
python3 testdata/scenarios/gen.py   # regenerate fixtures after editing gen.py
```

New infrastructure comes in as a probe (see `internal/probe/README.md`) and,
when it is a new shape, a catalog type in `internal/model/catalog.go`.
