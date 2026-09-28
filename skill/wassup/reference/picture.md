# The picture

wassup places every box and draws every wire itself. What it has to place
is decided by `topology.yaml`, so a diagram that is easy to follow begins
there. This page says how the picture is built, what to write so that it
comes out clean, and what to check before you show it to anybody.

## Where things stand

```
   names                                          ╔ Third parties ╗
   load balancer                                  ║ mail          ║
 ┌ machine 1 ──┐  ┌ machine 2 ──┐  ┌ machine 3 ┐  ║ payments      ║
 │ router      │  │ router      │  │ router    │  ╚═══════════════╝
 │ API         │  │ API         │  │ API       │    monitoring
 │ workers     │  │ · workers   │  │ · workers │  ╔ Buckets ══════╗
 └─────────────┘  └─────────────┘  └───────────┘  ║ uploads       ║
   queue            nightly job                   ╚═══════════════╝
 ┌ data machine ┐ ┌ data machine ┐
 │ database     │ │ database     │
 └──────────────┘ └──────────────┘
```

| Where | What | Decided by |
| --- | --- | --- |
| top | the way in, one row per step: names above the load balancer they point at | the types `dns`, `firewall`, `loadbalancer`, `ingress` |
| middle | the machines that run the application, each a frame with what runs on it inside | `node`, and `runs_on` on what runs there |
| below the machines | what runs on no machine: queues, scheduled jobs | the types `queue`, `scheduledjob`, a workload without `runs_on` |
| bottom | what holds data: a machine with nothing but data on it, and a data store without a machine | `database`, `cache`, `syncengine`, `storage` |
| right | what is outside the system: the services of others first, then what watches the system, then the rest | `external`, `observability`, `lane: side` |

Inside a machine the same component stands on the same row of every
machine: what takes requests on top, what holds data at the bottom, the
rest in the order of the topology. A machine that holds none of a
component's pods at the moment shows its name, faint, and no box:
`· Workers, none here`. The place is kept, because the component may run
there tomorrow, and nothing in the picture moves when it does.

## How the wires run

- **One net per component.** What leaves a component is one line beside the
  row of its copies. Every copy taps it, and it branches to each box it
  reaches. Several boxes that feed one box share a line the other way
  around.
- **Right for what is outside, down for the data.** A wire to the side
  column leaves to the right and runs up or down a spine beside the column.
  A wire to another row runs down the street, the room in the middle of the
  picture that is at the same place in every row.
- **One arrowhead per box and net**, on the last stretch, which is also
  where the rate of the edge is written.
- **A joint is a tee, a crossing is not.** Where the wires of one net meet
  they are joined (`┬ ┴ ├ ┤`). Where two nets meet, one is drawn over the
  other in one piece.
- **No wire to a place that holds nothing.** The wires join the copies that
  run.

A box somebody dragged out of its row is reached by a wire that finds its
own way. `layout.json` belongs to the user: never write it to tidy a
picture, change the topology.

## Writing a topology that draws clean

1. **Name the machines a component may run on.** `runs_on` lists every
   machine of the pool its pods are scheduled on (its node selector and its
   tolerations say which), not only where they were seen today. wassup
   shows where they are now and keeps the other places empty. A component
   that names one machine and moves to another is drawn nowhere.
2. **Leave `lane` out.** The type says where a box stands. `lane: side` is
   for what is outside the system and has no type that says so (a bucket
   in another cloud). A queue or a scheduled job on the side stands among
   the services of others and sends its wires across the whole picture.
3. **Write the components in the order of the flow.** Router, web app, API,
   workers, cache, database: the order of the topology is the order on a
   machine and in the side column. Put the services of others that are
   called most first.
4. **Give what is outside a group of its own.** A group without a parent
   whose boxes all stand on the side is framed there under its title:
   `kind: zone`, `label: Third parties` for the services of others, one
   `kind: cloud` group for the buckets of an account. The user folds a
   frame with a click on its title.
5. **One machine per instance of a database.** A database with `roles`
   takes `runs_on` with one node per instance, the primary first. Machines
   that are reserved for data then stand at the bottom by themselves.
6. **One edge per relation.** Draw what carries the work of the system.
   Leave out what every box does (each workload sending its logs to the
   collector) unless watching it is the point, and health checks always.
   An edge goes to the component, never to each of its pods.
7. **Short labels.** A box is as wide as its label, and the boxes of a
   machine as wide as the widest of them. `Payments, cards and refunds` is
   about as long as a label should be; the rest belongs in `notes`.
8. **Groups that nest.** cloud, then cluster, then namespace. A namespace
   whose workloads all run on machines has nothing left to frame; give it
   no boxes of its own rather than one queue in a frame.

`wassup validate` warns about a workload that names no machine, about a
box of the system that stands on the side, and about a label that is too
long.

## Before you show it

Run `wassup validate`, then `wassup export`, and look at the picture
itself, the `.png` or the `.txt`. Go on only when every line is true:

- [ ] `wassup validate` has no warning about the picture.
- [ ] The machines that run the application stand in the middle, what
      holds data at the bottom, the services of others on the right.
- [ ] Nothing of the system itself stands in the side column.
- [ ] Every workload of the cluster stands inside a machine.
- [ ] No label is cut (`…`) and no box is much wider than the others.
- [ ] Every edge can be followed from its box to its arrowhead without
      guessing, and every arrow points the way the work flows.

After the bindings are written, once more with live data:

- [ ] Every service of others says a rate, `idle`, or `no rate measured`
      with the reason you can name (nothing traces the caller, the address
      was never called).
- [ ] A component that runs on one machine has one box, and its other
      places read `none here`.
