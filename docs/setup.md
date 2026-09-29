# Set up a real system

wassup is configured by two files in `.wassup/`: `topology.yaml` (the
components and the edges between them) and `bindings.yaml` (the probes that
attach each of them to live data). You can write them by hand, but the
usual way is to let a coding agent draft them from your repository and
check them with you.

- [With a coding agent](#with-a-coding-agent)
- [By hand](#by-hand)
- [Settings of your machine](#settings-of-your-machine)
- [Access and credentials](#access-and-credentials)
- [The .wassup directory](#the-wassup-directory)
- [Keeping it up to date](#keeping-it-up-to-date)

## With a coding agent

### wassup is already installed

```sh
cd your-repo
wassup        # first run: prints the setup prompt and copies it to the clipboard
```

Paste the prompt into Claude Code. `wassup init --print-prompt` prints it
again.

### Nothing is installed yet

Open your coding agent (Claude Code, or any agent that can run shell
commands) in the repository that deploys your system and send it this
prompt. It installs the binary, then reads the setup guide that ships
inside it (`wassup init --print-prompt`), so the steps always match the
version it installed.

```text
Install wassup and set it up for this repository.

1. Install it, unless `wassup version` already works:
   curl -fsSL https://raw.githubusercontent.com/danilopopovikj/wassup/main/install.sh | sh
   (with Go 1.26 or newer, `go install github.com/danilopopovikj/wassup/cmd/wassup@latest`
   works too). If `wassup version` says its directory is not on my PATH,
   show me the line it prints. Ask me before installing anything system-wide.
2. Run `wassup init --print-prompt`. It prints the setup guide. Follow it
   step by step, and stop where it says to ask me.
```

The guide has the agent read your Terraform, manifests, Helm values,
network policies, code and the live cluster, draft the diagram with every
connection cited, and check it against live data. The agent asks first,
stops to show you the diagram before it attaches live data, stops again
after every tier, and never writes to your cluster. When it is done, run
`wassup`.

## By hand

Every step the agent takes is a command you can run yourself.

```sh
cd your-repo
wassup init                         # creates .wassup/ and .wassup/local.env
wassup discover --propose --write --environment production
                                    # names the cluster, asks, drafts .wassup/proposed/
# review proposed/review.md, then write .wassup/topology.yaml
wassup export                       # the picture, before any live data
# write .wassup/bindings.yaml, one tier at a time
wassup validate                     # schemas and id cross-checks
wassup probe --tier 0               # Kubernetes, DNS, certificates, pings
wassup probe --tier 1               # adds what needs a token
wassup probe                        # everything
wassup probe db                     # one element, with every value it read
wassup                              # the diagram
```

Two complete `.wassup/` directories are in [examples/](../examples): a k3s
cluster with CloudNativePG on Hetzner, and a managed cloud setup. The
component types are described in
[catalog.md](../skill/wassup/reference/catalog.md) and the probes in
[probes.md](../skill/wassup/reference/probes.md).

## Settings of your machine

`.wassup/local.env` holds what belongs to your machine and not to the
repository: which cluster to read, and the credentials the bindings name.
wassup reads it on every run, so you export nothing and paste no commands.
Git ignores it; wassup adds the line to `.wassup/.gitignore` itself.

```sh
# .wassup/local.env
WASSUP_KUBECONFIG=infra/terraform/out/kubeconfig-production.yaml
WASSUP_CONTEXT=production
BOOKSTORE_DB_PASSWORD=p@ss:w/rd#1
HATCHET_CLIENT_TOKEN=...
```

One `NAME=value` per line, the value as it is: nothing is expanded and
nothing needs encoding. A variable that is set in your environment wins over
the file, `--env-file <path>` wins over `local.env`, and a flag wins over
both.

A binding names the parts of a connection, so no URL has to be built:

```yaml
components:
  db:
    - probe: pg.stats
      via: k8s.service/shop/bookstore-db-rw:5432   # wassup opens the port-forward
      user: app
      database: bookstore
      password_env: BOOKSTORE_DB_PASSWORD
```

`dsn_env` still names a variable that holds a whole connection string.

## Access and credentials

wassup only reads. It never changes the systems it watches, and that does
not rest on how it is used: the clients it is built on refuse everything
else before it is sent.

| Source | What is refused in code |
| --- | --- |
| Kubernetes | every request that is not a GET or a HEAD, except the one that opens a port-forward |
| HTTP APIs | every request that is not a GET or a HEAD |
| Postgres | every statement that is not one `SELECT` or one `SHOW`; on a server they run in a read-only transaction |
| Redis | every command but `INFO`, `LLEN` and `LINDEX` |

The build fails on code that goes around these clients. Two things a guard
cannot promise: a read can be expensive, and a server may act on a read.
Both are opt-in where wassup knows of them (`list_objects` on a bucket,
`table` on Electric).

Probes take credentials from the environment variables named in
`bindings.yaml`, or from your kubeconfig. `wassup probes` lists the access
each probe needs.

You do not need an admin kubeconfig. `wassup access` prints the smallest
account for the probes you have bound: get, list and watch on what they
read, the port-forward in the namespaces a `via` reaches into, and never
secrets, `nodes/proxy` or exec. It also prints the commands that write a
kubeconfig with a token that expires. wassup runs none of them.

```sh
wassup access                  # the manifest, what was left out, the commands
wassup access --manifest       # the manifest alone
```

## The .wassup directory

`.wassup/` is the only channel between you, Claude Code and the TUI.

| File | Written by | Read by | Purpose |
| --- | --- | --- | --- |
| `topology.yaml` | Claude Code | TUI, CLI | components, groups, edges |
| `bindings.yaml` | Claude Code | TUI, CLI | probes attaching each component and edge to live data |
| `findings.yaml` | Claude Code (scan) | TUI, CLI | repo and infra issues attached to components |
| `thresholds.yaml` | Claude Code, you | TUI | optional overrides of the default thresholds |
| `layout.json` | TUI (drag, resize, collapse) | TUI | positions you own |
| `local.env` | you | every command | the cluster to read and the credentials of this machine; gitignored |
| `proposed/` | `wassup discover --write` | you, Claude Code | the draft, `review.md` and the evidence; gitignored |
| `state/snapshot.json` | TUI, every tick, atomic | `wassup explain` | current state of everything |
| `state/history/<date>.jsonl` | TUI | timeline, CLI | one compact frame per tick, 24h at full resolution, 7 days rolled up |
| `state/events.jsonl` | TUI | TUI, CLI | change markers: deploys, terraform applies, reboots, scale events |
| `state/annotations.json` | `wassup annotate` | TUI | highlighted paths and notes |

Commit everything except `state/`, `proposed/` and `local.env`, which are
gitignored.

## Keeping it up to date

`wassup sync` re-runs discovery and shows what the repository or the cluster
gained since. `--apply` merges the additions without touching your labels,
notes or layout, `--prune` removes what vanished, and `--check` fails CI on
drift.
