#!/usr/bin/env python3
"""Generates the ten scenario fixtures under testdata/scenarios/<nn>-<slug>/.

Each fixture is a self-contained .wassup directory (topology.yaml,
bindings.yaml) plus scenario.yaml, observations.jsonl and expected.yaml.
Times in observations are offsets in seconds from scenario.start; a record
holds until the next record for the same target and probe.

Run: python3 testdata/scenarios/gen.py
"""
import json, os, copy

HERE = os.path.dirname(os.path.abspath(__file__))

# ---------------------------------------------------------------- topology
def topology(workers=("worker",)):
    comps = [
        {"id": "dns", "type": "dns", "label": "bookstore.example", "group": "hetzner"},
        {"id": "fw", "type": "firewall", "label": "Firewall", "group": "hetzner"},
        {"id": "lb", "type": "lb", "label": "Load balancer", "group": "hetzner"},
        {"id": "ingress", "type": "ingress", "label": "Ingress", "group": "k3s",
         "runs_on": ["node-1", "node-2", "node-3"]},
        {"id": "node-1", "type": "node", "group": "k3s"},
        {"id": "node-2", "type": "node", "group": "k3s"},
        {"id": "node-3", "type": "node", "group": "k3s"},
        {"id": "api", "type": "workload", "label": "API", "group": "bookstore",
         "runs_on": ["node-1", "node-2", "node-3"], "owner": "@danilo"},
    ]
    for w in workers:
        label = "Workers" if w == "worker" else w
        comps.append({"id": w, "type": "workload", "label": label, "group": "bookstore",
                      "runs_on": ["node-1", "node-2"]})
    comps += [
        {"id": "default-queue", "type": "queue", "label": "Default queue", "group": "bookstore"},
        {"id": "exports-queue", "type": "queue", "label": "Exports queue", "group": "bookstore"},
        {"id": "docs-sync", "type": "job", "label": "Docs sync", "group": "bookstore"},
        {"id": "cache", "type": "cache", "label": "Redis", "group": "bookstore"},
        {"id": "db", "type": "db", "label": "Database", "group": "bookstore", "engine": "postgres",
         "roles": {"primary": "db-primary", "replicas": ["db-r1", "db-r2"]}},
        {"id": "signoz", "type": "observability", "label": "SigNoz", "group": "k3s"},
        {"id": "github", "type": "external", "label": "GitHub"},
    ]
    edges = [
        {"from": "dns", "to": "lb", "kind": "tcp"},
        {"from": "lb", "to": "ingress", "kind": "http", "label": "requests"},
        {"from": "lb", "to": "node-1", "kind": "tcp"},
        {"from": "lb", "to": "node-2", "kind": "tcp"},
        {"from": "lb", "to": "node-3", "kind": "tcp"},
        {"from": "ingress", "to": "api", "kind": "http"},
        {"from": "api", "to": "db", "kind": "sql"},
        {"from": "api", "to": "cache", "kind": "cache"},
        {"from": "api", "to": "default-queue", "kind": "queue"},
        {"from": "api", "to": "exports-queue", "kind": "queue"},
    ]
    for w in workers:
        edges += [
            {"from": "default-queue", "to": w, "kind": "queue"},
            {"from": "exports-queue", "to": w, "kind": "queue"},
            {"from": w, "to": "db", "kind": "sql"},
            {"from": w, "to": "github", "kind": "external"},
        ]
    edges += [
        {"from": "docs-sync", "to": workers[0], "kind": "queue"},
        {"from": "db-primary", "to": "db-r1", "kind": "replication"},
        {"from": "db-primary", "to": "db-r2", "kind": "replication"},
        {"from": "fw", "to": "signoz", "kind": "tcp", "label": "OTLP"},
    ]
    return {
        "version": 1,
        "name": "bookstore",
        "settings": {"auto_lens": True, "lookback": "3h"},
        "groups": [
            {"id": "hetzner", "kind": "cloud", "label": "Hetzner"},
            {"id": "k3s", "kind": "cluster", "label": "k3s", "parent": "hetzner"},
            {"id": "bookstore", "kind": "namespace", "label": "bookstore", "parent": "k3s"},
        ],
        "components": comps,
        "edges": edges,
    }

def bindings(workers=("worker",)):
    comps = {
        "dns": [{"probe": "dns.record", "host": "bookstore.example"}],
        "fw": [{"probe": "hcloud.firewall", "name": "bookstore"}],
        "lb": [{"probe": "hcloud.lb", "name": "bookstore-lb", "targets": {"node-1": "node-1", "node-2": "node-2", "node-3": "node-3"}}],
        "ingress": [{"probe": "k8s.ingress", "namespace": "bookstore", "name": "bookstore"}],
        "node-1": [{"probe": "k8s.node", "name": "node-1"}],
        "node-2": [{"probe": "k8s.node", "name": "node-2"}],
        "node-3": [{"probe": "k8s.node", "name": "node-3"}],
        "api": [{"probe": "k8s.workload", "namespace": "bookstore", "selector": "app=api"}],
        "default-queue": [{"probe": "celery.queue", "broker": "redis://redis.bookstore:6379/0", "queue": "default"}],
        "exports-queue": [{"probe": "celery.queue", "broker": "redis://redis.bookstore:6379/0", "queue": "exports"}],
        "docs-sync": [{"probe": "k8s.cronjob", "namespace": "bookstore", "name": "docs-sync"}],
        "cache": [{"probe": "redis.info", "addr": "redis.bookstore:6379"}],
        "db": [{"probe": "cnpg.cluster", "namespace": "bookstore", "cluster": "bookstore-db"}],
        "db-primary": [{"probe": "cnpg.instance", "namespace": "bookstore", "cluster": "bookstore-db", "role": "primary"},
                       {"probe": "pg.stats", "via": "k8s.workload/api", "dsn_env": "WASSUP_PG_DSN"}],
        "db-r1": [{"probe": "cnpg.instance", "namespace": "bookstore", "cluster": "bookstore-db", "instance": "bookstore-db-2"}],
        "db-r2": [{"probe": "cnpg.instance", "namespace": "bookstore", "cluster": "bookstore-db", "instance": "bookstore-db-3"}],
        "signoz": [{"probe": "signoz.health", "url": "http://signoz.signoz:8080"}],
        "github": [{"probe": "http.ping", "url": "https://api.github.com"}],
    }
    for w in workers:
        comps[w] = [{"probe": "k8s.workload", "namespace": "bookstore", "selector": "app=" + w}]
    edges = {
        "lb->ingress": [{"probe": "signoz.edge", "from": "lb", "to": "ingress"}],
        "ingress->api": [{"probe": "signoz.edge", "from": "ingress", "to": "api"}],
        "api->db": [{"probe": "pg.pool", "via": "k8s.workload/api"}, {"probe": "signoz.edge", "from": "api", "to": "postgres"}],
        "api->cache": [{"probe": "signoz.edge", "from": "api", "to": "redis"}],
        "db-primary->db-r1": [{"probe": "pg.stats", "via": "k8s.workload/api", "replica": "bookstore-db-2"}],
        "db-primary->db-r2": [{"probe": "pg.stats", "via": "k8s.workload/api", "replica": "bookstore-db-3"}],
        "fw->signoz": [{"probe": "hcloud.firewall", "name": "bookstore", "port": 4317}],
    }
    for w in workers:
        edges[w + "->github"] = [{"probe": "signoz.edge", "from": w, "to": "api.github.com"}]
    return {"version": 1, "components": comps, "edges": edges}

# ------------------------------------------------------------- baseline obs
def baseline(workers=("worker",)):
    """A healthy system at t=0."""
    o = []
    def rec(target, probe, metrics=None, conditions=None, events=None, detail=None, at=0):
        r = {"at": at, "target": target, "probe": probe}
        if metrics: r["metrics"] = metrics
        if conditions: r["conditions"] = conditions
        if events: r["events"] = events
        if detail: r["detail"] = detail
        o.append(r)
    rec("dns", "dns.record", {"resolves": 1}, detail={"host": "bookstore.example", "resolves_to": "lb"})
    rec("fw", "hcloud.firewall", {"rules": 6})
    rec("lb", "hcloud.lb", {"connections": 340, "rate": 1100, "targets_healthy": 3, "targets_total": 3})
    for n in ("node-1", "node-2", "node-3"):
        rec("lb->" + n, "hcloud.lb", {"healthy": 1, "rate": 366})
        rec(n, "k8s.node", {"cpu_pct": 42, "mem_pct": 61, "disk_pct": 48, "pods": 12})
    rec("ingress", "k8s.ingress", {"rate": 1100, "error_rate": 0.2, "cert_days": 61}, detail={"hosts": ["bookstore.example"]})
    rec("lb->ingress", "signoz.edge", {"rate": 1100, "error_rate": 0.2, "p95_ms": 80})
    rec("ingress->api", "signoz.edge", {"rate": 1100, "error_rate": 0.2, "p95_ms": 78})
    rec("api", "k8s.workload", {"replicas_ready": 3, "replicas_desired": 3, "cpu_pct": 40, "mem_pct": 55, "restarts": 0},
        detail={"image": "ghcr.io/bookstore/api:a1b2c3d", "kind": "Deployment"})
    for w in workers:
        rec(w, "k8s.workload", {"replicas_ready": 4, "replicas_desired": 4, "cpu_pct": 35, "mem_pct": 60, "restarts": 0},
            detail={"image": "ghcr.io/bookstore/worker:a1b2c3d", "kind": "Deployment"})
        rec("default-queue->" + w, "celery.queue", {"rate": 12 / len(workers)})
        rec("exports-queue->" + w, "celery.queue", {"rate": 0.2 / len(workers)})
        rec(w + "->db", "signoz.edge", {"rate": 50, "error_rate": 0})
        rec(w + "->github", "signoz.edge", {"rate": 2, "error_rate": 0, "p95_ms": 210})
    rec("default-queue", "celery.queue", {"depth": 3, "oldest_age_s": 2, "consumers": 4})
    rec("exports-queue", "celery.queue", {"depth": 0, "oldest_age_s": 0, "consumers": 2})
    rec("api->default-queue", "signoz.edge", {"rate": 12})
    rec("api->exports-queue", "signoz.edge", {"rate": 0.2})
    rec("docs-sync", "k8s.cronjob", {"active": 0, "succeeded": 24, "failed": 0}, detail={"schedule": "every 15 min"})
    rec("cache", "redis.info", {"mem_pct": 41, "hit_rate": 96, "evictions": 0, "clients": 12})
    rec("api->cache", "signoz.edge", {"rate": 5000, "hit_rate": 96})
    rec("api->db", "pg.pool", {"pool_used": 40, "pool_max": 100, "waiters": 0, "rate": 800, "p95_ms": 9})
    rec("db-primary", "pg.stats", {"cpu_pct": 30, "mem_pct": 52, "disk_pct": 61, "connections_used": 42, "connections_max": 100})
    rec("db-r1", "cnpg.instance", {"cpu_pct": 8, "mem_pct": 40, "disk_pct": 61, "lag_bytes": 2048})
    rec("db-r2", "cnpg.instance", {"cpu_pct": 8, "mem_pct": 40, "disk_pct": 61, "lag_bytes": 4096})
    rec("db-primary->db-r1", "pg.stats", {"lag_bytes": 2048, "streaming": 1})
    rec("db-primary->db-r2", "pg.stats", {"lag_bytes": 4096, "streaming": 1})
    rec("signoz", "signoz.health", {"ingest_rate": 2100, "disk_pct": 38})
    rec("fw->signoz", "hcloud.firewall", {"rate": 500})
    rec("github", "http.ping", {"latency_ms": 120, "error_rate": 0, "timeout_rate": 0})
    return o

def override(obs, target, probe=None, **fields):
    """Replace the baseline record of target (and probe) with new fields."""
    for r in obs:
        if r["target"] == target and (probe is None or r["probe"] == probe):
            for k, v in fields.items():
                if v is None:
                    r.pop(k, None)
                else:
                    r[k] = v
            return r
    raise KeyError(target)

def drop(obs, pred):
    return [r for r in obs if not pred(r)]

# ---------------------------------------------------------------- scenarios
SCENARIOS = []

def scenario(num, slug, name, symptom, start, duration, obs, expected, workers=("worker",)):
    SCENARIOS.append(dict(num=num, slug=slug, name=name, symptom=symptom, start=start,
                          duration=duration, obs=obs, expected=expected, workers=workers))

# 9.1 Deploy crash loop --------------------------------------------------
obs = baseline()
override(obs, "api", metrics={"replicas_ready": 2, "replicas_desired": 3, "cpu_pct": 40, "mem_pct": 55, "restarts": 6, "restart_window_s": 300},
         conditions=[{"kind": "CrashLoopBackOff", "ref": "pod/api-7d9f4b-x2k9", "since_s": -120,
                      "detail": "migration 0042_add_invoice_index failed: relation \"invoices\" does not exist"}],
         events=[{"at_s": -120, "kind": "deploy", "summary": "deploy of api b7e9f21", "author": "danilo", "ref": "b7e9f21"}],
         detail={"image": "ghcr.io/bookstore/api:b7e9f21", "kind": "Deployment",
                 "last_logs": ["django.db.utils.ProgrammingError: relation \"invoices\" does not exist",
                               "  File \"manage.py\", line 22, in <module>", "Error: migration failed, exiting"]})
override(obs, "ingress->api", metrics={"rate": 1100, "error_rate": 30, "p95_ms": 95})
override(obs, "ingress", metrics={"rate": 1100, "error_rate": 30, "cert_days": 61})
scenario(1, "deploy-crash-loop", "Deploy crash loop", "site is giving errors", "2026-09-27T09:50:00Z", 120, obs, {
    "components": {
        "api": {"state": "failing", "label": "failing, 1 of 3 replicas, 6 restarts in 5 min", "severity": "crit"},
        "lb": {"state": "flowing"},
        "db": {"state": "flowing"},
    },
    "edges": {
        "ingress->api": {"state": "failing", "label": "failing, 30 percent errors"},
        "lb->ingress": {"state": "flowing", "label": "flowing, 1.1k req/s"},
    },
    "cause": "api",
    "story_contains": ["requests arrive, 1.1k/s", "load balancer passes them to 3 of 3 nodes",
                       "API is failing, 1 of 3 replicas, 6 restarts in 5 min", "last change on this path: deploy of api b7e9f21, 09:48, by danilo"],
    "lens_on": True,
})

# 9.2 Connection pool exhausted ------------------------------------------
obs = baseline()
override(obs, "api->db", metrics={"pool_used": 100, "pool_max": 100, "waiters": 38, "rate": 800, "p95_ms": 72},
         detail={"top_waiting": ["UPDATE invoices SET status=$1 WHERE id=$2", "SELECT ... FROM events WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 50"]})
override(obs, "db-primary", metrics={"cpu_pct": 35, "mem_pct": 52, "disk_pct": 61, "connections_used": 100, "connections_max": 100, "active_connections": 92})
scenario(2, "connection-pool-exhausted", "Connection pool exhausted", "everything is slow", "2026-09-27T10:30:00Z", 120, obs, {
    "components": {
        "db": {"state": "flowing", "severity": "warn"},
        "api": {"state": "flowing"},
    },
    "edges": {
        "api->db": {"state": "waiting", "label": "waiting, 38 queued, at the database", "severity": "warn"},
    },
    "cause": "db",
    "story_contains": ["API is waiting, 38 queued, at the database",
                       "database is at 100 of 100 connections, CPU 35 percent, so the pool is the limit, not the database"],
})

# 9.3 Primary disk filling from an inactive replication slot -------------
obs = baseline()
override(obs, "db-primary", metrics={"cpu_pct": 30, "mem_pct": 52, "disk_pct": 84, "connections_used": 42, "connections_max": 100, "wal_retained_bytes": 40 * 2**30},
         ramp_to={"disk_pct": 86})
override(obs, "db-primary->db-r2", metrics={"wal_retained_bytes": 40 * 2**30, "streaming": 0},
         conditions=[{"kind": "ReplicationBroken", "ref": "pod/bookstore-db-3", "since_s": -3 * 3600, "detail": "replication slot bookstore_db_3 inactive"},
                     {"kind": "SlotInactive", "ref": "slot/bookstore_db_3", "since_s": -3 * 3600}])
# (the scenario lasts an hour, so the edge reads "4 h" at the end)
override(obs, "db-r2", metrics={"cpu_pct": 5, "mem_pct": 30, "disk_pct": 61})
scenario(3, "primary-disk-filling", "Primary disk filling from an inactive replication slot", "none yet, disk gauge climbing", "2026-09-27T08:00:00Z", 3600, obs, {
    "components": {
        "db-primary": {"state": "flowing"},
    },
    "edges": {
        "db-primary->db-r2": {"state": "failing", "label": "failing, replica not streaming for 4 h, 40 GB WAL retained"},
        "db-primary->db-r1": {"state": "flowing"},
    },
    "notes_contain": {"db-primary": ["full in about 7 h"]},
    "cause": "db-primary->db-r2",
    "story_contains": ["db-r2 has not been streaming for 4 h, 40 GB of WAL is retained on db-primary"],
    "lens_on": True,
})

# 9.4 Node memory pressure, workers OOMKilled -----------------------------
obs = baseline()
override(obs, "node-2", metrics={"cpu_pct": 55, "mem_pct": 96, "disk_pct": 48, "pods": 14, "killed": 2},
         conditions=[{"kind": "MemoryPressure", "ref": "node/node-2", "since_s": -600}],
         detail={"kills": [{"pod": "worker-6c9d-k2pq", "memory": "1.9 GB at kill"}, {"pod": "worker-6c9d-zz7r", "memory": "2.0 GB at kill"}]})
override(obs, "worker", metrics={"replicas_ready": 4, "replicas_desired": 6, "cpu_pct": 70, "mem_pct": 92, "restarts": 2, "killed": 2, "evicted": 1},
         conditions=[{"kind": "OOMKilled", "ref": "pod/worker-6c9d-k2pq", "since_s": -400, "detail": "limit 2Gi"},
                     {"kind": "Evicted", "ref": "pod/worker-6c9d-m1nn", "since_s": -300}],
         events=[{"at_s": -300, "kind": "scale", "summary": "scale of workers to 6", "author": "danilo"}])
override(obs, "exports-queue", metrics={"depth": 12, "oldest_age_s": 0, "consumers": 4})
scenario(4, "node-memory-pressure", "Node memory pressure, workers OOMKilled", "my export never finished", "2026-09-27T09:00:00Z", 120, obs, {
    "components": {
        "node-2": {"state": "failing", "label": "failing, memory at 96 percent, 2 pods killed"},
        "worker": {"state": "failing", "contains": "killed, out of memory"},
        "exports-queue": {"state": "waiting", "label": "waiting, 12 queued, at workers"},
    },
    "cause": "node-2",
    "story_contains": ["the exports queue is waiting, 12 queued, at workers",
                       "workers are failing on node-2 because the node is out of memory",
                       "last change on this path: scale of workers to 6, 08:55"],
    "lens_on": True,
})

# 9.5 TLS certificate not renewed -----------------------------------------
obs = baseline()
override(obs, "ingress", metrics={"rate": 1100, "error_rate": 0.2, "cert_days": 0},
         conditions=[{"kind": "CertExpired", "ref": "certificate/bookstore-tls", "since_s": -3 * 3600 + 120},
                     {"kind": "CertRenewalFailed", "ref": "certificate/bookstore-tls", "since_s": -8 * 86400, "detail": "DNS challenge error"}],
         detail={"hosts": ["bookstore.example"], "issuer": "letsencrypt-prod", "challenge": "DNS-01 for bookstore.example: NXDOMAIN for _acme-challenge"})
scenario(5, "tls-certificate-expired", "TLS certificate not renewed", "browser says not secure", "2026-09-27T12:00:00Z", 120, obs, {
    "components": {
        "ingress": {"state": "failing", "contains": "certificate expired"},
    },
    "cause": "ingress",
    "story_contains": ["requests arrive, 1.1k/s", "the certificate at the ingress expired 3 h ago",
                       "renewal has failed since 09-19, DNS challenge error"],
    "lens_on": True,
})

# 9.6 Celery backlog with a stuck worker ----------------------------------
W3 = ("worker-1", "worker-2", "worker-3")
obs = baseline(W3)
override(obs, "default-queue", metrics={"depth": 2400, "oldest_age_s": 35 * 60, "consumers": 3, "growth_per_min": 40})
override(obs, "worker-3", metrics={"replicas_ready": 1, "replicas_desired": 1, "cpu_pct": 99, "mem_pct": 60, "restarts": 0, "running_s": 1200, "p95_s": 60},
         conditions=[{"kind": "TaskRunning", "ref": "task/9f1c", "since_s": -1200 + 120, "detail": "send_digest"}],
         detail={"task": "send_digest", "task_id": "9f1c2a", "args": "tenant=acme"})
obs = drop(obs, lambda r: r["target"] == "default-queue->worker-3")
scenario(6, "celery-backlog-stuck-worker", "Celery backlog with a stuck worker", "emails are late", "2026-09-27T14:00:00Z", 120, obs, {
    "components": {
        "default-queue": {"state": "waiting", "label": "waiting, 2.4k queued, oldest 35 min"},
        "worker-3": {"state": "processing", "label": "processing send_digest, 20 min", "severity": "warn"},
        "worker-1": {"state": "flowing"},
    },
    "cause": "worker-3",
    "story_contains": ["jobs are queued and growing, 2.4k queued, oldest 35 min",
                       "one worker is stuck on send_digest for 20 min", "the others are keeping up"],
}, workers=W3)

# 9.7 Node dropped out of the load balancer --------------------------------
obs = baseline()
override(obs, "lb", metrics={"connections": 340, "rate": 1100, "targets_healthy": 2, "targets_total": 3})
override(obs, "lb->node-3", metrics={"healthy": 0, "rate": 0},
         conditions=[{"kind": "HealthCheckFailing", "ref": "target/node-3", "since_s": -840, "detail": "tcp:6443 connection refused"}])
override(obs, "node-3", metrics={"cpu_pct": 0, "mem_pct": 0, "disk_pct": 48, "pods": 0},
         conditions=[{"kind": "NotReady", "ref": "node/node-3", "since_s": -840, "detail": "kubelet stopped posting node status"},
                     {"kind": "Rebooted", "ref": "node/node-3", "since_s": -900}],
         events=[{"at_s": -900, "kind": "node", "summary": "reboot of node-3"}])
scenario(7, "node-dropped-from-lb", "Node dropped out of the load balancer", "none, slight latency bump", "2026-09-27T07:30:00Z", 120, obs, {
    "components": {
        "node-3": {"state": "failing", "label": "failing, not ready, rebooted 07:15"},
        "lb": {"state": "flowing", "severity": "warn"},
    },
    "edges": {
        "lb->node-3": {"state": "blocked", "label": "blocked, health check failing since 07:16"},
        "lb->node-2": {"state": "flowing"},
    },
    "notes_contain": {"lb": ["2 of 3 targets healthy"]},
    "cause": "node-3",
    "story_contains": ["traffic reaches 2 of 3 nodes", "node-3 rebooted at 07:15 and did not rejoin the cluster"],
    "lens_on": True,
})

# 9.8 Firewall change blocked observability --------------------------------
obs = baseline()
override(obs, "fw", metrics={"rules": 5},
         events=[{"at_s": -38 * 60, "kind": "terraform", "summary": "terraform apply: hcloud_firewall.main rule allow-lb-only", "author": "danilo", "ref": "hcloud_firewall.main"}])
override(obs, "fw->signoz", metrics={"rate": 0},
         conditions=[{"kind": "FirewallDenied", "ref": "rule/allow-lb-only", "since_s": -38 * 60, "detail": "allow-lb-only"}])
override(obs, "signoz", metrics={"ingest_rate": 0, "disk_pct": 38},
         conditions=[{"kind": "NoData", "ref": "signoz/otel-collector", "since_s": -40 * 60 + 120}])
obs = drop(obs, lambda r: r["probe"] == "signoz.edge")
scenario(8, "firewall-blocked-observability", "Firewall change blocked observability", "SigNoz shows nothing since 11:00", "2026-09-27T11:40:00Z", 120, obs, {
    "components": {
        "signoz": {"state": "failing", "label": "failing, no data received for 40 min"},
    },
    "edges": {
        "fw->signoz": {"state": "blocked", "label": "blocked at firewall since 11:02, rule allow-lb-only"},
        "ingress->api": {"state": "idle", "marker": "nodata", "label": "no data"},
        "api->cache": {"marker": "nodata"},
    },
    "cause": "signoz",
    "story_contains": ["the firewall changed at 11:02", "observability stopped receiving data",
                       "traffic rates on this diagram are unknown since then", "last change on this path: terraform apply, 11:02, by danilo"],
    "lens_on": True,
})

# 9.9 Cache full, miss storm hits the database -----------------------------
obs = baseline()
override(obs, "cache", metrics={"mem_pct": 100, "hit_rate": 10, "evictions": 5200, "clients": 12},
         detail={"maxmemory": "2gb", "maxmemory_policy": "noeviction", "keys_without_ttl": 184000})
override(obs, "api->cache", metrics={"rate": 5000, "hit_rate": 10})
override(obs, "api->db", metrics={"pool_used": 80, "pool_max": 100, "waiters": 0, "rate": 1600, "rate_baseline": 800, "p95_ms": 40})
override(obs, "db-primary", metrics={"cpu_pct": 85, "mem_pct": 60, "disk_pct": 61, "connections_used": 80, "connections_max": 100})
scenario(9, "cache-full-miss-storm", "Cache full, miss storm hits the database", "slow again", "2026-09-27T15:00:00Z", 120, obs, {
    "components": {
        "cache": {"state": "failing", "label": "failing, memory full, 90 percent misses"},
        "db": {"state": "flowing", "severity": "warn"},
    },
    "edges": {
        "api->cache": {"state": "flowing", "contains": "90 percent miss"},
        "api->db": {"state": "flowing", "contains": "double normal"},
    },
    "cause": "cache",
    "story_contains": ["the cache is full and missing 90 percent",
                       "the database is taking double its normal load, CPU 85 percent"],
    "lens_on": True,
})

# 9.10 External dependency down -------------------------------------------
obs = baseline()
override(obs, "github", metrics={"latency_ms": 30000, "error_rate": 100, "timeout_rate": 100},
         conditions=[{"kind": "Timeout", "ref": "https://api.github.com", "since_s": -12 * 60 + 120, "detail": "HEAD timed out after 30 s"}])
override(obs, "worker->github", metrics={"rate": 2, "error_rate": 100, "p95_ms": 30000})
override(obs, "docs-sync", metrics={"active": 0, "succeeded": 23, "failed": 1},
         conditions=[{"kind": "JobFailed", "ref": "job/docs-sync-29341", "since_s": -300, "detail": "GitHub API timeout"}],
         detail={"schedule": "every 15 min", "last_failure": "docs-sync-29341"})
scenario(10, "external-dependency-down", "External dependency down", "docs sync failed", "2026-09-27T16:00:00Z", 120, obs, {
    "components": {
        "github": {"state": "failing", "label": "failing, timing out for 12 min"},
        "docs-sync": {"state": "failing", "contains": "last run failed"},
    },
    "edges": {
        "worker->github": {"state": "failing", "label": "failing, 100 percent errors"},
    },
    "cause": "github",
    "story_first": "the docs sync job failed because GitHub is not responding",
    "story_contains": ["nothing inside the system is at fault", "retries every 15 min"],
    "lens_on": True,
})

# ------------------------------------------------------------------- write
def dump_yaml(v, indent=0):
    """Tiny YAML emitter for the plain structures used here."""
    pad = "  " * indent
    out = []
    if isinstance(v, dict):
        for k, val in v.items():
            if isinstance(val, (dict, list)) and val:
                out.append(f"{pad}{k}:")
                out.append(dump_yaml(val, indent + 1))
            else:
                out.append(f"{pad}{k}: {scalar(val)}")
    elif isinstance(v, list):
        for item in v:
            if isinstance(item, dict):
                first = True
                for k, val in item.items():
                    lead = f"{pad}- " if first else f"{pad}  "
                    first = False
                    if isinstance(val, (dict, list)) and val:
                        out.append(f"{lead}{k}:")
                        out.append(dump_yaml(val, indent + 2))
                    else:
                        out.append(f"{lead}{k}: {scalar(val)}")
            else:
                out.append(f"{pad}- {scalar(item)}")
    return "\n".join(x for x in out if x != "")

def scalar(v):
    if isinstance(v, bool):
        return "true" if v else "false"
    if v is None:
        return "null"
    if isinstance(v, (int, float)):
        return str(v)
    if isinstance(v, dict) and not v:
        return "{}"
    if isinstance(v, list) and not v:
        return "[]"
    s = str(v)
    if s == "" or any(c in s for c in ":#{}[],&*!|>'\"%@`") or s.strip() != s or s.lower() in ("yes", "no", "true", "false", "null", "on", "off"):
        return json.dumps(s)
    return s

for sc in SCENARIOS:
    d = os.path.join(HERE, f"{sc['num']:02d}-{sc['slug']}")
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "topology.yaml"), "w") as f:
        f.write("# generated by testdata/scenarios/gen.py\n" + dump_yaml(topology(sc["workers"])) + "\n")
    with open(os.path.join(d, "bindings.yaml"), "w") as f:
        f.write("# generated by testdata/scenarios/gen.py\n" + dump_yaml(bindings(sc["workers"])) + "\n")
    with open(os.path.join(d, "scenario.yaml"), "w") as f:
        f.write(dump_yaml({"name": sc["name"], "symptom": sc["symptom"], "start": sc["start"],
                           "duration_s": sc["duration"], "tick_s": 5}) + "\n")
    with open(os.path.join(d, "observations.jsonl"), "w") as f:
        for r in sc["obs"]:
            f.write(json.dumps(r, separators=(",", ":")) + "\n")
    with open(os.path.join(d, "expected.yaml"), "w") as f:
        f.write(dump_yaml(sc["expected"]) + "\n")
    print("wrote", d)
