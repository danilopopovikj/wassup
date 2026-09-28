# Contributing to wassup

Thanks for wanting to help. Bug reports, new probes, clearer labels and
better docs are all welcome.

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to contribute

| You want to | Do this |
| --- | --- |
| Ask a question or float an idea | Start a [discussion](https://github.com/danilopopovikj/wassup/discussions) |
| Report a bug | Open a [bug report](https://github.com/danilopopovikj/wassup/issues/new?template=bug_report.yml) |
| Ask for a probe or a feature | Open a [request](https://github.com/danilopopovikj/wassup/issues/new?template=feature_request.yml) |
| Report a vulnerability | Follow [SECURITY.md](SECURITY.md). Do not open a public issue |
| Change code or docs | Open a pull request, as described below |

For anything larger than a small fix, open an issue first. It is cheaper to
agree on the approach before the code exists.

## Set up

You need Go 1.26 or newer and `make`. Nothing else: the tests need no
cluster, no database and no network.

```sh
git clone https://github.com/danilopopovikj/wassup
cd wassup
make build         # builds ./wassup
make test          # go test ./...
make lint          # gofmt and go vet
make demo          # builds and replays the connection pool scenario
```

Two optional tools:

- `python3` regenerates the scenario fixtures (`make fixtures`).
- `resvg` or `rsvg-convert` lets `wassup export` write PNG next to SVG.

## Read this first

[CLAUDE.md](CLAUDE.md) holds the principles the code is built on and the
decisions already taken, for human and AI contributors alike. A pull request
that goes against one of them needs to argue for changing the principle, in
that file, in the same pull request. The ones that come up most:

- **Plain words.** Labels on the diagram read "waiting, 38 queued, at the
  database". Kubernetes vocabulary belongs in the detail panel.
  [style.md](skill/wassup/reference/style.md) is the style guide.
- **Never fabricate.** A number the provider did not report is left out,
  never shown as 0.
- **Extensible by type.** The renderer and the state engine never learn a
  product name.
- **Standard library first.** A new dependency needs a reason.

## Where things are

```
cmd/wassup/          main and the subcommands
internal/model/      schemas, types, validation, ids, refs, remap
internal/probe/      the Probe interface, the registry, one package per probe family
internal/bind/       joins observations to components and edges
internal/state/      the six-state engine, severity, labels, cause and story
internal/history/    snapshot writer, history frames, events, trends
internal/layout/     lanes, ordering, the edge router
internal/render/     the terminal UI, canvas, panels, timeline, export
internal/explain/    the explain command
internal/discover/   reads Terraform, manifests, Helm values, code and the cluster
internal/app/        the runtime: probes, tick loop, persistence, hot reload
internal/scenario/   the offline replay harness used by tests and --check
skill/wassup/        the Claude Code skill and its reference files
prompts/             the setup prompt
testdata/scenarios/  the recorded scenarios (gen.py regenerates them)
examples/            complete .wassup/ directories
```

## Common changes

Each of these has a step-by-step recipe under "How to" in
[CLAUDE.md](CLAUDE.md).

- **Add a probe.** The most common contribution. Start with
  [internal/probe/README.md](internal/probe/README.md). Test with `httptest`
  or a fake client, never a live service.
- **Change a label or the story.** Change the expected output in
  `testdata/scenarios/gen.py` first, regenerate, watch the test fail, then
  change `internal/state`.
- **Add a scenario.** Extend `testdata/scenarios/gen.py` and run
  `make fixtures`.
- **Add a command.** It must accept `--json`. Update the table in the README.

Fixtures and examples are public. Use invented names and reserved domains
such as `shop.example`, never a real system's hostnames or project names.

## Pull requests

1. Fork, and branch from `main`.
2. Keep the change to one topic. Two fixes are two pull requests.
3. Run `make lint` and `make test`. Both must pass.
4. Write the commit message in prose: what changed and why. No tool or
   model names in code, comments or commit messages.
5. Open the pull request and fill in the template.

CI runs lint, the tests on Linux and macOS with the race detector, a
vulnerability check and CodeQL. A maintainer reviews once CI is green.
Pull requests are squashed when merged, so the title becomes the commit
subject: make it say what changed.

## Releases

Maintainers release by pushing a tag:

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The release workflow builds the archives, checksums, bills of materials and
build provenance, and opens a draft release to review and publish. Versions
follow [Semantic Versioning](https://semver.org). Before 1.0, a minor
version may change file formats in `.wassup/`.

## License

wassup is released under the [MIT License](LICENSE). Your contributions are
released under the same license.
