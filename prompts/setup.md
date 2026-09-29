# Set up wassup for this repository

You are a coding agent (Claude Code or another that runs shell commands)
working in a repository that deploys to Kubernetes. Set up `wassup`, the
terminal diagram that shows where the system is stuck, since when, and what
changed. wassup only reads, and so do you: run nothing that writes to the
cluster or the infrastructure.

1. If `.claude/skills/wassup/SKILL.md` does not exist, run `wassup skill install`
   (`--dir <path>` if your skills live elsewhere) and read SKILL.md in full.
   Follow its **Setup** workflow; it is the authority on the steps below.
2. Ask me three questions before you run anything else, and wait for the
   answers:
   - Which environment is this for (production, staging, ...)?
     `wassup discover --no-cluster --json` lists the ones the repository has.
   - Which kubeconfig and which context? `wassup discover` names the cluster
     it is about to read and lists the kubeconfigs it finds in the
     repository; do not take the default context of this machine for
     granted.
   - How far may the probes reach? Tier 0 needs the kubeconfig only
     (Kubernetes, DNS, certificates, pings). Tier 1 needs tokens (Hatchet,
     Electric, load balancers, object storage). Tier 2 connects to the
     databases.
   Put the kubeconfig and the context into `.wassup/local.env`. I put the
   credentials there myself: tell me the names of the variables, and never
   read, print or copy a value. If I want a read-only account first, run
   `wassup access` and show me what it prints.
3. Run `wassup discover --propose --write --environment <name>` and review
   `.wassup/proposed/review.md` line by line against its citations, the
   least certain lines first. Drop what you cannot back, and place or drop
   every entry in the unresolved hosts list. Never guess: a connection you
   cannot back with evidence is left out and reported, not drawn.
4. Write `.wassup/topology.yaml` using only catalog types, and write it for
   the picture: read `reference/picture.md` of the skill first. The
   machines that run the application stand in the middle with what runs on
   them inside, what holds data at the bottom, the services of others in a
   group of their own on the right. `runs_on` names every machine a
   component may run on. No `lane:` on anything that is part of the
   system. Components in the order of the flow, short labels, one edge per
   relation that carries work.
   Then run `wassup validate` and `wassup export`, look at the picture
   yourself and go through the list at the end of `reference/picture.md`.
   Change the topology until the diagram is easy to follow: every wire can
   be followed from its box to its arrowhead, and every arrow points the
   way the work flows. Never write `layout.json` to tidy it. Show me the
   rendered diagram, not a description of it, and wait for my answer.
5. Write `.wassup/bindings.yaml` one tier at a time. After each tier run
   `wassup validate` and `wassup probe --tier <n>`, fix what is unbound
   (`wassup probe <id>` tests one element), tell me what you found, and stop
   until I say go on. A database or a service inside the cluster is reached
   with `via: k8s.service/<namespace>/<service>:<port>`; never ask me to run
   `kubectl port-forward`.
6. Find the rates with `wassup measure` (with `--signoz https://…` when
   no binding names SigNoz yet). Show me its report: what it found and
   the evidence, what cannot be counted and why, and what it saw that no
   box stands for. Add the components and edges it lists once you have
   checked them against the code or the egress allowlist, run it again,
   then `wassup measure --write` and `wassup probe`. A box that still reads
   `no rate measured` (◌) is up and uncounted, not broken: tell me the
   reason `measure` gave for each.
7. Commit `.wassup/` after I approve (its `state/` and `proposed/`
   directories and `local.env` are gitignored).

When you are done, tell me what is on the diagram, what stays unbound and
why, and which variables `.wassup/local.env` has to hold before I run
`wassup`.
