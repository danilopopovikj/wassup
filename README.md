# wassup

[![CI](https://github.com/danilopopovikj/wassup/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/danilopopovikj/wassup/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/danilopopovikj/wassup.svg)](https://pkg.go.dev/github.com/danilopopovikj/wassup)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/danilopopovikj/wassup/badge)](https://scorecard.dev/viewer/?uri=github.com/danilopopovikj/wassup)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

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

![connection pool exhausted: the lens lights the path from the load balancer to the database, the panel explains the edge](docs/screenshots/02-pool-exhausted-panel.svg)

More frames from the recorded scenarios: [node out of memory](docs/screenshots/04-node-memory-lens.svg),
[node dropped from the load balancer, timeline strip](docs/screenshots/07-node-dropped-timeline.svg).

wassup is not an agent, not a dashboard and not a kubectl replacement.
Claude Code writes its configuration, reads its snapshots and draws on it;
wassup itself needs no AI to run. MIT, single binary, runs in tmux, no daemon.

> **Status:** early and before 1.0. It works, the recorded scenarios pass,
> and the file formats in `.wassup/` may still change between minor versions.

## Contents

- [Try it in a minute](#try-it-in-a-minute)
- [Install](#install)
  - [Install and set up with AI](#install-and-set-up-with-ai)
  - [Install by hand](#install-by-hand)
- [Set up a real system](#set-up-a-real-system)
- [Principles](#principles)
- [Keys](#keys)
- [Commands](#commands)
- [What it can watch](#what-it-can-watch)
- [The recorded scenarios](#the-recorded-scenarios)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

## Try it in a minute

No cluster needed. The demo replays a recorded incident:

```sh
go install github.com/danilopopovikj/wassup/cmd/wassup@latest

wassup demo              # the connection pool scenario
wassup demo 4 --speed 5  # node memory pressure, five times faster
wassup demo --list       # all twelve
```

Press `i` for the issue lens, `enter` for the detail panel, `?` for every key.

## Install

### Install and set up with AI

The short way: open your coding agent (Claude Code, or any agent that can
run shell commands) in the repository that deploys your system, and send it
this prompt. It installs the binary, reads your infrastructure, drafts the
diagram and checks it against live data.

```text
Install wassup and set it up for this repository.

wassup is a terminal app that draws one live architecture diagram of a
Kubernetes-hosted system and shows where it is stuck, since when, and what
changed. It is configured by two files you will write in .wassup/:
topology.yaml (the components and the edges between them) and bindings.yaml
(the probes that attach each of them to live data).
Source and docs: https://github.com/danilopopovikj/wassup

1. Install the binary, unless `wassup version` already works. With Go 1.26
   or newer, run
   `go install github.com/danilopopovikj/wassup/cmd/wassup@latest`
   and make sure "$(go env GOPATH)/bin" is on the PATH. Without Go, take the
   archive for this OS and architecture from
   https://github.com/danilopopovikj/wassup/releases, check it against
   checksums.txt and put `wassup` on the PATH. Ask me before installing Go
   or anything else system-wide.

2. From the repository root, run `wassup skill install`. It copies the
   wassup skill to .claude/skills/wassup/ (`--dir <path>` if your skills
   live elsewhere). Read SKILL.md there in full and follow its Setup
   workflow. The skill is the authority on the steps below; its reference/
   folder has the catalog of component types, every probe with the access
   it needs, the JSON Schemas and two complete examples.

3. Run `wassup discover --propose --write`. It reads Terraform, Kubernetes
   manifests, Helm values, .env files, application code and the live
   cluster, and drafts .wassup/proposed/ with every component and edge
   cited to a file and line. Treat the draft as evidence, not as the
   answer: open the citation behind each edge and confirm the connection
   is real, and place or drop every entry in the unresolved hosts list.

4. Write .wassup/topology.yaml, with labels in words someone who has never
   used Kubernetes would say. Then stop and show me the topology, and wait
   for my answer before you write bindings.

5. Write .wassup/bindings.yaml, then run `wassup validate` and
   `wassup probe --once`. Fix every unbound component or edge (wrong
   namespace, selector, permissions, missing env var) and run both again
   until what remains unbound has a reason you can name.

Rules:
- Read only. wassup never changes the system and neither should you: run
  nothing that writes to the cluster or the infrastructure.
- No secrets in .wassup/. Bindings name the environment variables that
  hold credentials; never copy a value out of .env or a secret.
- Ask me only for what cannot be discovered, such as the kubeconfig path
  and context when they are not obvious, or the name of an env var that
  holds a token.
- Never guess. A connection you cannot back with evidence is left out and
  reported, not drawn.
- Commit .wassup/ only after I approve the result (its state/ directory is
  gitignored).

When you are done, tell me: what is on the diagram, what stays unbound and
why, and which environment variables I need to export before I run
`wassup`.
```

The agent stops once to show you the topology before it attaches live data,
and it never writes to your cluster. When it is done, run `wassup`.

### Install by hand

wassup needs no AI to run, and every step the agent takes is a command you
can run yourself.

| How | Command |
| --- | --- |
| Go 1.26 or newer | `go install github.com/danilopopovikj/wassup/cmd/wassup@latest` |
| Binary | Download the archive for Linux or macOS from [Releases](https://github.com/danilopopovikj/wassup/releases) and put `wassup` on your `PATH` |
| From source | `git clone https://github.com/danilopopovikj/wassup && cd wassup && make install` |

Release archives come with checksums, a bill of materials and a signed build
provenance. [SECURITY.md](SECURITY.md#verifying-a-release) shows how to
verify one.

Optional: `resvg` or `rsvg-convert` on the `PATH` lets `wassup export` write
PNG next to SVG.

## Set up a real system

If you sent the prompt from
[Install and set up with AI](#install-and-set-up-with-ai), this is already
done. With the binary installed by hand, the same setup starts from the
first run:

```sh
cd your-repo
wassup                 # first run: prints the setup prompt and copies it to the clipboard
```

Paste the prompt into Claude Code. It installs the skill (`wassup skill
install`), runs `wassup discover --propose --write`, which reads Terraform,
Kubernetes manifests, Helm values, `.env` files, application code and the
live cluster and drafts `topology.yaml` and `bindings.yaml` with every
component and edge cited to a file and line, verifies each data flow against
that evidence, and checks the result with `wassup validate` and `wassup
probe --once`. Then run `wassup`. Later, `wassup sync` shows what the repo
or cluster gained since, and `--apply` merges it without touching your layout.

wassup only reads. Probes take credentials from the environment variables
named in `bindings.yaml`, or from your kubeconfig, never from a file in
`.wassup/`. `wassup probes` lists the access each probe needs.

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

Two complete examples are in [examples/](examples): a k3s cluster with
CloudNativePG on Hetzner, and a managed cloud setup.

## Principles

- **One diagram.** The general view and the issue view are the same picture, filtered.
- **Six states.** Every node and edge is always in exactly one of: flowing, idle, waiting, processing, blocked, failing. One glyph and one color per state, everywhere.
- **Plain words.** Labels read "waiting, 38 queued, at the database", never "CrashLoopBackOff".
- **Files, not sockets.** Everything Claude Code and wassup exchange is a file in `.wassup/`.
- **Watch, don't poll.** Kubernetes data comes from informers; metrics tick every 5 seconds; the screen redraws at up to 10 fps.
- **Layout is stable.** Boxes never jump on their own. Positions live in `layout.json`, which the user owns.
- **Extensible by type.** New infrastructure is a probe implementing one interface plus a catalog type, never a renderer change.

## Keys

`←↑→↓`/`hjkl` move · `enter` detail · `tab` panel · `i` issue lens · `n` next
issue · `t` scrub the timeline, `[` `]` step, `esc` live · `c` copy the
`wassup://` ref, `C` ref plus summary, `y` the panel · `f` findings · `e`
changes · `a` annotations · `x`/`X` clear annotations · `s` export svg/png/txt
· `g` collapse group · `d` detail level (minimal, normal, full) · `r`/`R`
reset layout · `/` filter · `?` all keys.
Mouse: click selects, drag moves, drag a corner resizes, click a group title
collapses it.

## Commands

| Command | Does |
| --- | --- |
| `wassup` | runs the TUI; first run prints the setup prompt |
| `wassup init [--print-prompt]` | creates `.wassup/` or prints the prompt |
| `wassup discover [--propose --write]` | evidence from Terraform, manifests, Helm values, code and the cluster; `--propose` drafts topology and bindings with every edge cited |
| `wassup sync [--apply] [--prune] [--check]` | re-discovers and diffs against `.wassup/`; merges additions, prunes what vanished, or fails CI on drift |
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
| `wassup version` | prints the version |

Every command accepts `--json`.

## What it can watch

Kubernetes workloads, nodes, ingresses, volumes and cron jobs; CloudNativePG
clusters, Postgres statistics and connection pools; Redis; S3-compatible
object storage; Hetzner load balancers and firewalls; DNS records, TLS
certificates and HTTP endpoints; SigNoz; Terraform state and git as sources
of change markers. `wassup probes` prints the full list with the access each
one needs, and [probes.md](skill/wassup/reference/probes.md) is the same
list as a document.

Background work and sync engines map onto the catalog without new renderer
code: Hatchet queues and workflows (`hatchet.queue`, `hatchet.workflow`),
worker pools with slots and stuck tasks (`hatchet.workers`, `celery.worker`),
Celery queues on Redis or RabbitMQ (`celery.queue`, `amqp.queue`), and
Electric as a `syncengine` component whose replication slot is watched
through `pg.stats` (`electric.sync`). The mapping is spelled out in
[catalog.md](skill/wassup/reference/catalog.md).

Something missing? A new system comes in as a probe, and
[internal/probe/README.md](internal/probe/README.md) explains how to write
one.

## The recorded scenarios

The build is accepted against twelve recorded scenarios in
`testdata/scenarios/`: deploy crash loop, connection pool exhausted, primary
disk filling from an inactive replication slot, node memory pressure, TLS
certificate not renewed, Celery backlog with a stuck worker, node dropped out
of the load balancer, firewall change blocked observability, cache full,
an external dependency down, a Hatchet backlog with every slot busy, and an
Electric replication slot gone inactive.

```sh
go test ./...                                    # replays all twelve
wassup replay --check testdata/scenarios/<name>  # checks one
wassup replay testdata/scenarios/<name>          # plays one in the TUI
```

## Contributing

Contributions are welcome: bug reports, probes for systems wassup does not
know yet, clearer labels, better docs.

- [CONTRIBUTING.md](CONTRIBUTING.md) covers the setup, the layout of the
  code and how pull requests are handled.
- [CLAUDE.md](CLAUDE.md) holds the building principles, the vocabulary and
  the recipes for adding a provider, a shape, a scenario or a command.
- Questions and ideas go to
  [Discussions](https://github.com/danilopopovikj/wassup/discussions).

Everyone taking part is expected to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Security

Please report vulnerabilities privately, as described in
[SECURITY.md](SECURITY.md), not in a public issue.

## License

[MIT](LICENSE) © Danilo Popovikj
