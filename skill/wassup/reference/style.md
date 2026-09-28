# Label and story language

The whole visual language is six states, one glyph and one color each. A
label reads like a colleague talking, and a CS person must be able to read it.

## Labels

- Present tense, lowercase, state word first: `failing, 3 restarts in 5 min`.
- No Kubernetes vocabulary in the first three words. `CrashLoopBackOff`,
  `OOMKilled`, `NotReady` go in the detail panel, never on the diagram.
- The number first when there is one, the location last:
  `waiting, 38 queued, at the database`.
- Percentages are written out: `30 percent errors`, never `30%` in prose.
- Times are `HH:MM` (`rebooted 07:15`), durations are `18 min`, `3 h`, `2 d`.
- Counts use k/M: `2.4k queued`, `1.1k req/s`.

Templates per state:

| State | Template | Example |
| --- | --- | --- |
| flowing | `flowing, <rate> <unit>` | flowing, 1.2k req/s |
| idle | `idle` | idle |
| waiting | `waiting, <n> queued, at the <destination>` | waiting, 38 queued, at the database |
| processing | `processing <unit of work>, <elapsed>` | processing export-42, 18 min |
| blocked | `blocked at firewall since <HH:MM>, rule <name>` / `blocked, health check failing since <HH:MM>` | blocked at firewall rule allow-lb-only |
| failing | `failing, <plain reason>` | failing, 3 restarts in 5 min |

Markers outside the six states: `unbound, no probe data`, `no data, <reason>`
when the probes of an element failed (`no data, connection refused`) and
`stale, <last label>`. A failed probe is never a reason for `failing`.

## Story strip

One sentence per hop, top to bottom, the cause last (first when the cause is
outside the system). Sentences are lowercase, present tense, no semicolons.

```
requests arrive, 1.1k/s
load balancer passes them to 3 of 3 nodes
API is waiting, 38 queued, at the database
database is at 100 of 100 connections, CPU 35 percent, so the pool is the limit, not the database
last change on this path: deploy of api b7e9f21, 09:48, by danilo
```

## Annotations you write

A note is one sentence in the same voice, naming the element by its label and
stating what you believe and why: `the pool is the limit: 100 of 100
connections busy, CPU only 35 percent`. Give `--confidence` honestly. Expire
notes (`--ttl`) so stale hypotheses disappear on their own.

## Findings you write

`title` is a short imperative or noun phrase (`worker has no memory limit`),
`evidence` cites the file and the line, `suggested_fix` is one concrete change.
