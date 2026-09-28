# Set up wassup for this repository

You are Claude Code working in a repository that deploys to Kubernetes. Set up
`wassup`, the terminal diagram that shows where the system is stuck, since
when, and what changed. wassup only reads, and so do you: run nothing that
writes to the cluster or the infrastructure.

1. If `.claude/skills/wassup/SKILL.md` does not exist, run `wassup skill install`
   and read the skill. Follow its **Setup** workflow; it is the authority on
   the steps below.
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
   credentials there myself: tell me the names of the variables.
3. Run `wassup discover --propose --write --environment <name>` and review
   `.wassup/proposed/review.md` line by line against its citations, the
   least certain lines first. Drop what you cannot back.
4. Write `.wassup/topology.yaml` using only catalog types and semantic lanes.
   Then run `wassup export` and show me the rendered diagram, not a
   description of it. Wait for my answer.
5. Write `.wassup/bindings.yaml` one tier at a time. After each tier run
   `wassup validate` and `wassup probe --tier <n>`, fix what is unbound
   (`wassup probe <id>` tests one element), tell me what you found, and stop
   until I say go on.
6. Tell me what is not implemented yet: traffic rates come from
   `signoz.edge`, which has not shipped, so edges read `no rate measured`.
   That is not a fault.
7. Commit `.wassup/` after I approve (its `state/` and `proposed/`
   directories and `local.env` are gitignored).
