# Security policy

## Reporting a vulnerability

Please report it privately, not in a public issue:

1. Open the [Security tab](https://github.com/danilopopovikj/wassup/security) of this repository.
2. Choose **Report a vulnerability**.
3. Describe what you found, how to reproduce it, and what it lets an attacker do.

You will get a reply within 7 days. If the report is confirmed, the fix is
released first and the advisory is published after it, with credit to you
unless you ask otherwise.

## Supported versions

wassup is before 1.0. Fixes go into the latest release only.

## What wassup touches

This is the context a report is judged against.

- **Read only.** Probes read from Kubernetes, databases, brokers and cloud
  APIs. wassup never changes the systems it watches.
- **Credentials stay out of files.** Probes take credentials from the
  environment variables named in `bindings.yaml`, or from the kubeconfig.
  A credential that ends up in `.wassup/`, in a snapshot, in an export or
  in a log is a vulnerability.
- **No network listener.** wassup opens no port and runs no daemon. It and
  Claude Code exchange files in `.wassup/` and nothing else.
- **Files from the repository are input.** `topology.yaml`, `bindings.yaml`
  and the other files in `.wassup/` may come from a repository you cloned.
  A file that makes wassup run a command, write outside `.wassup/`, or send
  data somewhere the bindings do not name is a vulnerability.

## Verifying a release

Release archives come with `checksums.txt` and a signed build provenance.
With the [GitHub CLI](https://cli.github.com):

```sh
gh attestation verify wassup_<version>_<os>_<arch>.tar.gz --repo danilopopovikj/wassup
```
