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
prompt. It installs the binary, reads your infrastructure, drafts the
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
   `go install github.com/danilopopovikj/wassup/cmd/wassup@latest`.
   Without Go, take the archive for this OS and architecture from
   https://github.com/danilopopovikj/wassup/releases, check it against
   checksums.txt and put `wassup` in a directory of mine. Then run
   `"$(go env GOPATH)/bin/wassup" version`: when the directory is not on my
   PATH it prints the line to add to my shell profile. Show me that line;
   the binary has to work in my terminal, not only in yours. Ask me before
   installing Go or anything else system-wide.

2. From the repository root, run `wassup skill install`. It copies the
   wassup skill to .claude/skills/wassup/ (`--dir <path>` if your skills
   live elsewhere). Read SKILL.md there in full and follow its Setup
   workflow. The skill is the authority on the steps below; its reference/
   folder has the catalog of component types, every probe with the access
   it needs, the JSON Schemas and two complete examples.

3. Ask me three questions before you read anything, and wait for my
   answers:
   - Which environment is this for? `wassup discover --no-cluster --json`
     lists the ones the repository has.
   - Which kubeconfig and which context? `wassup discover` names the
     cluster it is about to read and lists the kubeconfigs it finds in the
     repository. Do not take the default context of this machine for
     granted.
   - How far may the probes reach? Tier 0 needs the kubeconfig only
     (Kubernetes, DNS, certificates, pings). Tier 1 needs tokens (Hatchet,
     Electric, load balancers, object storage). Tier 2 connects to the
     databases.
   Put the kubeconfig and the context into .wassup/local.env. If I want a
   read-only account first, run `wassup access` and show me what it
   prints.

4. Run `wassup discover --propose --write --environment <name>`. It reads
   Terraform, Kubernetes manifests, Helm values, network policies, .env
   files, application code and the live cluster, and drafts
   .wassup/proposed/. Treat the draft as evidence, not as the answer:
   review proposed/review.md line by line against its citations, the least
   certain lines first, drop what you cannot back, and place or drop every
   entry in the unresolved hosts list.

5. Write .wassup/topology.yaml, with labels in words someone who has never
   used Kubernetes would say, and write it for the picture: read
   reference/picture.md of the skill first. The machines that run the
   application stand in the middle with what runs on them inside, what
   holds data at the bottom, the services of others in a group of their
   own on the right. `runs_on` names every machine a component may run on.
   No `lane:` on anything that is part of the system. Components in the
   order of the flow, short labels, one edge per relation that carries
   work. Then run `wassup validate` and `wassup export`, look at the
   picture yourself, go through the list at the end of
   reference/picture.md and change the topology until the diagram is easy
   to follow. Never write layout.json to tidy it. Show me the rendered
   diagram, not a description of it, and wait for my answer before you
   write bindings.

6. Write .wassup/bindings.yaml one tier at a time. After each tier run
   `wassup validate` and `wassup probe --tier <n>`, fix every unbound
   component or edge (`wassup probe <id>` tests one), tell me what is
   bound, what is not and why, and stop until I say go on.

Rules:
- Read only. wassup never changes the system and neither should you: run
  nothing that writes to the cluster or the infrastructure.
- Bindings name the environment variables that hold credentials. The
  values go into .wassup/local.env, which git ignores, and I put them
  there: tell me the names, never read, print or copy a value.
- A database or a service inside the cluster is reached with
  `via: k8s.service/<namespace>/<service>:<port>`. Never ask me to run
  `kubectl port-forward`.
- A box that nothing counts reads "no rate measured"; that is not a fault.
  Tell me which probe would count it (k8s.scrape, signoz.edge, pg.stats)
  and what that probe needs. Every service of others should say a rate:
  bind the edge from each component that calls it, run `wassup probe <id>`
  on each, and name the reason for every one that still has none.
- Never guess. A connection you cannot back with evidence is left out and
  reported, not drawn.
- Commit .wassup/ only after I approve the result (its state/ directory is
  gitignored).

When you are done, tell me: what is on the diagram, what stays unbound and
why, and which variables .wassup/local.env has to hold before I run
`wassup`.
```

The agent asks first, stops to show you the diagram before it attaches live
data, stops again after every tier, and never writes to your cluster. When
it is done, run `wassup`.

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
