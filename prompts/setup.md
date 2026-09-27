# Set up wassup for this repository

You are Claude Code working in a repository that deploys to Kubernetes. Set up
`wassup`, the terminal diagram that shows where the system is stuck, since
when, and what changed.

1. If `.claude/skills/wassup/SKILL.md` does not exist, run `wassup skill install`
   and read the skill. Follow its **Setup** workflow.
2. Prefer discovery over assumptions: `wassup discover --json` lists nodes,
   workloads, services, ingresses, volumes, CNPG clusters and cron jobs from
   the kubeconfig. Read the terraform, kustomize or helm sources in this repo
   for anything the cluster does not know (load balancer, firewall, DNS,
   external services).
3. Ask only for what cannot be discovered: the kubeconfig path and context if
   they are not obvious, the SigNoz URL and token env var if SigNoz is used,
   the Hetzner token env var name if Hetzner is used.
4. Write `.wassup/topology.yaml` using only catalog types and semantic lanes.
   Stop and show the topology to the user before writing bindings.
5. Write `.wassup/bindings.yaml`, then run `wassup validate` and
   `wassup probe --once`. Fix every unbound component or say why it stays
   unbound. Commit `.wassup/` (its `state/` directory is gitignored).
