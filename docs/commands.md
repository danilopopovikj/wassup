# Commands

Every command accepts `--json`. `wassup <command> --help` shows the flags of
one command.

| Command | Does |
| --- | --- |
| `wassup` | runs the TUI; first run prints the setup prompt |
| `wassup init [--print-prompt]` | creates `.wassup/` or prints the prompt |
| `wassup discover [--propose --write] [--environment <name>] [--yes]` | evidence from Terraform, manifests, Helm values, network policies, code and the cluster; `--propose` drafts topology and bindings with every edge cited, and a `review.md` with one line per component and edge. Names the cluster before it reads it and asks when nobody chose it |
| `wassup sync [--apply] [--prune] [--check]` | re-discovers and diffs against `.wassup/`; merges additions, prunes what vanished, or fails CI on drift |
| `wassup validate` | schemas plus id cross-checks |
| `wassup probe [--tier <n>]` | runs every binding (two samples) and prints a line per probe as it finishes; reports bound/unbound per component and edge with the probe that failed, why, and what to do; `--tier` runs the bindings up to a tier and leaves the rest for later; `--json` adds per-probe status, error and metrics |
| `wassup probe <id>` | runs the bindings of one component or edge and prints every value, condition and detail they read |
| `wassup access [--logs] [--manifest]` | prints the smallest read-only account for the probes in `bindings.yaml` and the commands for a kubeconfig with a token that expires; contacts nothing |
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

`wassup export` writes PNG next to SVG when `resvg` or `rsvg-convert` is on
the `PATH`.

## Global flags

| Flag | Does |
| --- | --- |
| `--dir <path>` | path to the `.wassup` directory (default: found upward from the current directory) |
| `--kubeconfig <path>` | kubeconfig for the Kubernetes probes (default: `$WASSUP_KUBECONFIG`, `$KUBECONFIG` or `~/.kube/config`) |
| `--context <name>` | kubeconfig context (default: `$WASSUP_CONTEXT` or the current context) |
| `--env-file <path>` | a file of `NAME=value` lines, read before `.wassup/local.env` |
| `--json` | machine readable output |
| `--no-color` | glyphs only, no colors |
| `--split <percent>` | graph width when the panel is open |

## Exit codes

| Code | Means |
| --- | --- |
| 0 | done |
| 1 | an error |
| 2 | a file in `.wassup/` does not validate |
| 3 | `probe`: something is unbound |
| 4 | `sync --check`: the diagram fell behind |
| 5 | a question could not be asked (which cluster, which environment); nothing was read, and the message lists the flags that answer |

## Tiers

`wassup probe --tier <n>` runs the bindings up to a tier. Each tier needs
more from you than the one before.

| Tier | Needs | Probes |
| --- | --- | --- |
| 0 | the kubeconfig and the network | `k8s.*`, `cnpg.*`, `dns.record`, `cert.tls`, `http.ping`, `terraform.state`, `git.events` |
| 1 | a token or a key for an API | `hatchet.*`, `electric.sync`, `hcloud.*`, `amqp.queue`, `s3.bucket`, `signoz.edge` |
| 2 | a connection to a data store | `pg.stats`, `pg.pool`, `redis.info`, `redis.list`, `celery.queue` |

An element whose bindings all belong to a later tier prints as `later`. It
is not counted as unbound.

## The recorded scenarios

`wassup demo` replays one of twelve recorded incidents. No cluster needed.

| n | Scenario |
| --- | --- |
| 1 | deploy crash loop |
| 2 | connection pool exhausted (the default) |
| 3 | primary disk filling from an inactive replication slot |
| 4 | node memory pressure |
| 5 | TLS certificate not renewed |
| 6 | Celery backlog with a stuck worker |
| 7 | node dropped out of the load balancer |
| 8 | firewall change blocked observability |
| 9 | cache full |
| 10 | an external dependency down |
| 11 | Hatchet backlog with every slot busy |
| 12 | Electric replication slot gone inactive |

```sh
wassup demo              # the connection pool scenario
wassup demo 4 --speed 5  # node memory pressure, five times faster
wassup demo --list       # all twelve
```

The same fixtures are the acceptance suite of the build. They live in
`testdata/scenarios/`:

```sh
go test ./...                                    # replays all twelve
wassup replay --check testdata/scenarios/<name>  # checks one
wassup replay testdata/scenarios/<name>          # plays one in the TUI
```
