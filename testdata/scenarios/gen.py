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
def topology(workers=("worker",), extras=False):
    comps = [
        {"id": "dns", "type": "dns", "label": "bookstore.example", "group": "hetzner"},
        {"id": "fw", "type": "firewall", "label": "Firewall", "group": "hetzner"},
        {"id": "lb", "type": "loadbalancer", "label": "Load balancer", "group": "hetzner"},
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
        comps.append({"id": w, "type": "backgroundworker", "label": label, "group": "bookstore",
                      "runs_on": ["node-1", "node-2"]})
    comps += [
        {"id": "default-queue", "type": "queue", "label": "Default queue", "group": "bookstore"},
        {"id": "exports-queue", "type": "queue", "label": "Exports queue", "group": "bookstore"},
        {"id": "docs-sync", "type": "scheduledjob", "label": "Docs sync", "group": "bookstore"},
        {"id": "cache", "type": "cache", "label": "Redis", "group": "bookstore"},
        {"id": "db", "type": "database", "label": "Database", "group": "bookstore", "engine": "postgres",
         "roles": {"primary": "db-primary", "replicas": ["db-r1", "db-r2"]}},
        {"id": "signoz", "type": "observability", "label": "SigNoz", "group": "k3s"},
        {"id": "github", "type": "external", "label": "GitHub"},
    ]
    if extras:
        comps += [
            {"id": "hatchet", "type": "workload", "label": "Hatchet engine", "group": "bookstore", "runs_on": ["node-1", "node-2"]},
            {"id": "hatchet-queue", "type": "queue", "label": "Hatchet tasks", "group": "bookstore"},
            {"id": "hatchet-workers", "type": "backgroundworker", "label": "Hatchet workers", "group": "bookstore", "runs_on": ["node-2", "node-3"]},
            {"id": "billing", "type": "scheduledjob", "label": "Billing workflow", "group": "bookstore"},
            {"id": "node-4", "type": "node", "group": "k3s"},
            {"id": "hatchet-db", "type": "database", "label": "Hatchet DB", "group": "bookstore", "engine": "postgres",
             "runs_on": ["node-4"]},
            {"id": "electric", "type": "syncengine", "label": "Electric", "group": "bookstore", "runs_on": ["node-1"]},
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
    if extras:
        edges += [
            {"from": "api", "to": "hatchet-queue", "kind": "queue", "label": "tasks"},
            {"from": "hatchet-queue", "to": "hatchet-workers", "kind": "queue"},
            {"from": "hatchet", "to": "hatchet-db", "kind": "sql"},
            {"from": "hatchet-workers", "to": "db", "kind": "sql"},
            {"from": "billing", "to": "hatchet-workers", "kind": "queue"},
            {"from": "db-primary", "to": "electric", "kind": "replication", "label": "slot"},
            {"from": "ingress", "to": "electric", "kind": "http", "label": "shapes"},
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

def bindings(workers=("worker",), extras=False):
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
                       {"probe": "pg.stats", "via": "k8s.workload/api", "dsn_env": "BOOKSTORE_PG_DSN"}],
        "db-r1": [{"probe": "cnpg.instance", "namespace": "bookstore", "cluster": "bookstore-db", "instance": "bookstore-db-2"}],
        "db-r2": [{"probe": "cnpg.instance", "namespace": "bookstore", "cluster": "bookstore-db", "instance": "bookstore-db-3"}],
        "signoz": [{"probe": "signoz.health", "url": "http://signoz.signoz:8080", "token_env": "SIGNOZ_TOKEN"}],
        "github": [{"probe": "http.ping", "url": "https://api.github.com"}],
    }
    for w in workers:
        comps[w] = [{"probe": "k8s.workload", "namespace": "bookstore", "selector": "app=" + w}]
    if extras:
        HATCHET = {"url": "http://hatchet-api.bookstore:8080", "token_env": "HATCHET_CLIENT_TOKEN", "tenant": "707d0855-80ab-4e1f-a156-f1c4546cbf52"}
        comps.update({
            "hatchet": [{"probe": "k8s.workload", "namespace": "bookstore", "selector": "app=hatchet-engine"},
                        {"probe": "hatchet.health", **HATCHET}],
            "hatchet-queue": [{"probe": "hatchet.queue", **HATCHET}],
            "hatchet-workers": [{"probe": "k8s.workload", "namespace": "bookstore", "selector": "app=hatchet-worker"},
                                {"probe": "hatchet.workers", "long_task": "10m", **HATCHET}],
            "billing": [{"probe": "hatchet.workflow", "workflow": "billing", **HATCHET}],
            "node-4": [{"probe": "k8s.node", "name": "node-4"}],
            "hatchet-db": [{"probe": "pg.stats", "dsn_env": "HATCHET_PG_DSN"}],
            "electric": [{"probe": "k8s.workload", "namespace": "bookstore", "selector": "app=electric"},
                         {"probe": "electric.sync", "url": "http://electric.bookstore:3000", "secret_env": "ELECTRIC_SECRET", "table": "public.issues"},
                         {"probe": "pg.stats", "dsn_env": "BOOKSTORE_PG_DSN", "replica": "electric_slot_default"}],
        })
    SIGNOZ = {"probe": "signoz.edge", "url": "http://signoz.signoz:8080", "token_env": "SIGNOZ_TOKEN"}
    FAILED = {"status.code": "STATUS_CODE_ERROR"}
    def served(service):
        """The requests a service answers."""
        return {**SIGNOZ, "metric": "signoz_calls_total", "match": {"service.name": service, "span.kind": "SPAN_KIND_SERVER"}, "errors": FAILED}
    def stored(service, system):
        """The statements a service sends to a database or a cache."""
        return {**SIGNOZ, "metric": "signoz_db_latency_count", "match": {"service.name": service, "db.system": system}, "errors": FAILED}
    def called(service, address):
        """The calls a service makes to an address outside of it."""
        return {**SIGNOZ, "metric": "signoz_external_call_latency_count", "match": {"service.name": service, "address": address}, "errors": FAILED}
    edges = {
        "lb->ingress": [served("ingress")],
        "ingress->api": [served("api")],
        "api->db": [{"probe": "pg.pool", "dsn_env": "BOOKSTORE_PGBOUNCER_DSN", "via": "k8s.workload/api"}, stored("api", "postgresql")],
        "api->cache": [stored("api", "redis")],
        "db-primary->db-r1": [{"probe": "pg.stats", "dsn_env": "BOOKSTORE_PG_DSN", "via": "k8s.workload/api", "replica": "bookstore-db-2"}],
        "db-primary->db-r2": [{"probe": "pg.stats", "dsn_env": "BOOKSTORE_PG_DSN", "via": "k8s.workload/api", "replica": "bookstore-db-3"}],
        "fw->signoz": [{"probe": "hcloud.firewall", "name": "bookstore", "port": 4317}],
    }
    for w in workers:
        edges[w + "->github"] = [called(w, "api\\.github\\.com.*")]
    if extras:
        edges["db-primary->electric"] = [{"probe": "pg.stats", "dsn_env": "BOOKSTORE_PG_DSN", "via": "k8s.workload/api", "replica": "electric_slot_default"}]
        edges["ingress->electric"] = [served("electric")]
        edges["api->hatchet-queue"] = [called("api", "hatchet-api\\.bookstore.*")]
    return {"version": 1, "components": comps, "edges": edges}

# ------------------------------------------------------------- baseline obs
def place(*nodes):
    """Pod placement as the k8s.workload probe reports it: node -> pods, ready
    (and restarts in the window). Each argument is (node, pods) or
    (node, pods, ready) or (node, pods, ready, restarts)."""
    out = {}
    for n in nodes:
        node, pods = n[0], n[1]
        ready = n[2] if len(n) > 2 else pods
        entry = {"pods": pods, "ready": ready}
        if len(n) > 3 and n[3]:
            entry["restarts"] = n[3]
        out[node] = entry
    return out

API_PLACE = place(("node-1", 1), ("node-2", 1), ("node-3", 1))
WORKER_PLACE = place(("node-1", 2), ("node-2", 2))

def baseline(workers=("worker",), extras=False):
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
        detail={"image": "ghcr.io/bookstore/api:a1b2c3d", "kind": "Deployment", "placement": API_PLACE})
    for w in workers:
        rec(w, "k8s.workload", {"replicas_ready": 4, "replicas_desired": 4, "cpu_pct": 35, "mem_pct": 60, "restarts": 0},
            detail={"image": "ghcr.io/bookstore/worker:a1b2c3d", "kind": "Deployment", "placement": WORKER_PLACE})
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
    rec("db-primary", "pg.stats", {"cpu_pct": 30, "mem_pct": 52, "disk_pct": 61, "connections_used": 42, "connections_max": 100, "rate": 850})
    rec("db-r1", "cnpg.instance", {"cpu_pct": 8, "mem_pct": 40, "disk_pct": 61, "lag_bytes": 2048})
    rec("db-r2", "cnpg.instance", {"cpu_pct": 8, "mem_pct": 40, "disk_pct": 61, "lag_bytes": 4096})
    rec("db-primary->db-r1", "pg.stats", {"lag_bytes": 2048, "streaming": 1})
    rec("db-primary->db-r2", "pg.stats", {"lag_bytes": 4096, "streaming": 1})
    rec("signoz", "signoz.health", {"ingest_rate": 2100, "disk_pct": 38})
    rec("fw->signoz", "hcloud.firewall", {"rate": 500})
    rec("github", "http.ping", {"latency_ms": 120, "error_rate": 0, "timeout_rate": 0})
    if extras:
        rec("hatchet", "k8s.workload", {"replicas_ready": 2, "replicas_desired": 2, "cpu_pct": 22, "mem_pct": 48, "restarts": 0},
            detail={"kind": "Deployment", "placement": place(("node-1", 1), ("node-2", 1))})
        rec("hatchet", "hatchet.health", {"latency_ms": 9, "ready": 1}, detail={"version": "v0.62.1"})
        rec("hatchet-queue", "hatchet.queue", {"depth": 4, "pending": 0, "running": 12, "consumers": 6, "oldest_age_s": 3})
        rec("hatchet-workers", "k8s.workload", {"replicas_ready": 6, "replicas_desired": 6, "cpu_pct": 44, "mem_pct": 57, "restarts": 0},
            detail={"kind": "Deployment", "placement": place(("node-2", 3), ("node-3", 3))})
        rec("hatchet-workers", "hatchet.workers", {"workers_online": 6, "workers_total": 6, "pool_used": 12, "pool_max": 24, "active": 12, "waiters": 4})
        rec("billing", "hatchet.workflow", {"active": 0, "succeeded": 23, "failed": 0, "queued": 0}, detail={"schedule": "cron 0 * * * *", "cron": "0 * * * *"})
        rec("node-4", "k8s.node", {"cpu_pct": 12, "mem_pct": 35, "disk_pct": 30, "pods": 4})
        rec("hatchet-db", "pg.stats", {"cpu_pct": 18, "mem_pct": 40, "disk_pct": 35, "connections_used": 30, "connections_max": 200, "rate": 95})
        rec("electric", "k8s.workload", {"replicas_ready": 1, "replicas_desired": 1, "cpu_pct": 12, "mem_pct": 30, "restarts": 0},
            detail={"kind": "Deployment", "placement": place(("node-1", 1))})
        rec("electric", "electric.sync", {"latency_ms": 6, "ready": 1, "shape_ms": 40, "up_to_date": 1, "columns": 9}, detail={"status": "active", "table": "public.issues"})
        rec("electric", "pg.stats", {"lag_bytes": 1024, "streaming": 1})
        rec("api->hatchet-queue", "signoz.edge", {"rate": 8})
        rec("hatchet-queue->hatchet-workers", "hatchet.queue", {"rate": 8})
        rec("hatchet->hatchet-db", "signoz.edge", {"rate": 120, "error_rate": 0})
        rec("hatchet-workers->db", "signoz.edge", {"rate": 30, "error_rate": 0})
        rec("db-primary->electric", "pg.stats", {"lag_bytes": 1024, "streaming": 1})
        rec("ingress->electric", "signoz.edge", {"rate": 40, "error_rate": 0})
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

def scenario(num, slug, name, symptom, start, duration, obs, expected, workers=("worker",), extras=False):
    SCENARIOS.append(dict(num=num, slug=slug, name=name, symptom=symptom, start=start,
                          duration=duration, obs=obs, expected=expected, workers=workers, extras=extras))

# 9.1 Deploy crash loop --------------------------------------------------
obs = baseline()
override(obs, "api", metrics={"replicas_ready": 2, "replicas_desired": 3, "cpu_pct": 40, "mem_pct": 55, "restarts": 6, "restart_window_s": 300},
         conditions=[{"kind": "CrashLoopBackOff", "ref": "pod/api-7d9f4b-x2k9", "since_s": -120,
                      "detail": "migration 0042_add_invoice_index failed: relation \"invoices\" does not exist"}],
         events=[{"at_s": -120, "kind": "deploy", "summary": "deploy of api b7e9f21", "author": "danilo", "ref": "b7e9f21"}],
         detail={"image": "ghcr.io/bookstore/api:b7e9f21", "kind": "Deployment",
                 "placement": place(("node-1", 1), ("node-2", 1, 0, 6), ("node-3", 1)),
                 "last_logs": ["django.db.utils.ProgrammingError: relation \"invoices\" does not exist",
                               "  File \"manage.py\", line 22, in <module>", "Error: migration failed, exiting"]})
override(obs, "ingress->api", metrics={"rate": 1100, "error_rate": 30, "p95_ms": 95})
override(obs, "ingress", metrics={"rate": 1100, "error_rate": 30, "cert_days": 61})
override(obs, "api->exports-queue", metrics={"rate": 0})
override(obs, "exports-queue->worker", metrics={"rate": 0.017})
scenario(1, "deploy-crash-loop", "Deploy crash loop", "site is giving errors", "2026-09-27T09:50:00Z", 120, obs, {
    "components": {
        "api": {"state": "failing", "label": "failing, 1 of 3 replicas, 6 restarts in 5 min", "severity": "crit"},
        "lb": {"state": "flowing", "label": "flowing, 1.1k req/s"},
        "db": {"state": "flowing"},
        # its own count of transactions, not the busiest edge that touches it
        "db-primary": {"state": "flowing", "label": "flowing, 850 tx/s"},
        # a job that says it is not running is idle, with or without a rate
        "docs-sync": {"state": "idle", "label": "idle"},
        # a machine takes its state from what runs on it
        "node-1": {"state": "flowing"},
    },
    "edges": {
        "ingress->api": {"state": "failing", "label": "failing, 30 percent errors"},
        "lb->ingress": {"state": "flowing", "label": "flowing, 1.1k req/s"},
        # measured, and nothing went through
        "api->exports-queue": {"state": "idle", "label": "idle"},
        # a job a minute is not no job
        "exports-queue->worker": {"state": "flowing", "label": "flowing, 1 jobs/min"},
        # nothing measures it, which is not the same as nothing going through
        "docs-sync->worker": {"state": "idle", "label": "idle"},
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
         events=[{"at_s": -300, "kind": "scale", "summary": "scale of workers to 6", "author": "danilo"}],
         detail={"image": "ghcr.io/bookstore/worker:a1b2c3d", "kind": "Deployment",
                 "placement": place(("node-1", 3), ("node-2", 3, 1, 2))})
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

# 11 Hatchet backlog, every worker slot busy ---------------------------------
obs = baseline(extras=True)
override(obs, "hatchet-queue", "hatchet.queue", metrics={"depth": 850, "pending": 120, "running": 24, "consumers": 6, "oldest_age_s": 14 * 60, "growth_per_min": 30})
override(obs, "hatchet-workers", "hatchet.workers", metrics={"workers_online": 6, "workers_total": 6, "pool_used": 24, "pool_max": 24, "active": 24, "waiters": 850, "running_s": 1500, "p95_s": 90},
         conditions=[{"kind": "PoolExhausted", "ref": "workers/hatchet-worker", "since_s": -600, "detail": "all 24 slots busy"},
                     {"kind": "TaskRunning", "ref": "task/3f9a", "since_s": -1500 + 120, "detail": "generate-invoice"}],
         detail={"long_tasks": ["generate-invoice 25 min (tenant acme)"]})
override(obs, "billing", "hatchet.workflow", metrics={"active": 0, "succeeded": 20, "failed": 3, "queued": 40},
         conditions=[{"kind": "JobFailed", "ref": "run/8c1d", "since_s": -480 + 120, "detail": "step generate-invoice timed out after 900 s"}],
         detail={"schedule": "cron 0 * * * *", "cron": "0 * * * *", "last_failure": "8c1d"})
scenario(11, "hatchet-backlog", "Hatchet backlog, every worker slot busy", "invoices are late", "2026-09-27T17:00:00Z", 120, obs, {
    "components": {
        "hatchet-queue": {"state": "waiting", "label": "waiting, 850 queued, oldest 14 min"},
        "hatchet-workers": {"state": "waiting", "label": "waiting, all 24 slots busy, 850 queued"},
        "billing": {"state": "failing", "contains": "last run failed"},
        "hatchet": {"state": "flowing"},
        # 95 transactions of its own, where the edge into it carries 120
        "hatchet-db": {"state": "flowing", "label": "flowing, 95 tx/s"},
        # no edge reaches the machine: it is busy because the database on it is
        "node-4": {"state": "flowing", "label": "flowing"},
    },
    "cause": "hatchet-workers",
    "story_contains": ["jobs are queued and growing, 850 queued, oldest 14 min",
                       "Hatchet workers is waiting, all 24 slots busy, 850 queued",
                       "the billing workflow job is failing"],
}, extras=True)

# 12 Electric replication slot inactive, primary disk filling -------------------
obs = baseline(extras=True)
override(obs, "electric", "electric.sync", metrics={"latency_ms": 7, "ready": 0},
         conditions=[{"kind": "NotReady", "ref": "http://electric.bookstore:3000/v1/health", "since_s": -2 * 3600 + 120, "detail": "waiting for Postgres"}],
         detail={"status": "waiting", "table": "public.issues"})
override(obs, "electric", "pg.stats", metrics={"wal_retained_bytes": 12 * 2**30, "streaming": 0},
         conditions=[{"kind": "ReplicationBroken", "ref": "slot/electric_slot_default", "since_s": -2 * 3600 + 120, "detail": "replication slot electric_slot_default inactive"},
                     {"kind": "SlotInactive", "ref": "slot/electric_slot_default", "since_s": -2 * 3600 + 120}])
override(obs, "db-primary->electric", metrics={"wal_retained_bytes": 12 * 2**30, "streaming": 0},
         conditions=[{"kind": "ReplicationBroken", "ref": "slot/electric_slot_default", "since_s": -2 * 3600 + 120, "detail": "replication slot electric_slot_default inactive"},
                     {"kind": "SlotInactive", "ref": "slot/electric_slot_default", "since_s": -2 * 3600 + 120}])
override(obs, "ingress->electric", metrics={"rate": 40, "error_rate": 100, "p95_ms": 5000})
override(obs, "db-primary", metrics={"cpu_pct": 30, "mem_pct": 52, "disk_pct": 78, "connections_used": 42, "connections_max": 100, "wal_retained_bytes": 12 * 2**30})
scenario(12, "electric-slot-inactive", "Electric replication slot inactive", "the app stopped updating live", "2026-09-27T18:00:00Z", 120, obs, {
    "components": {
        "electric": {"state": "failing", "contains": "sync slot inactive for 2 h, 12 GB WAL retained"},
        "db-primary": {"state": "flowing"},
    },
    "edges": {
        "db-primary->electric": {"state": "failing", "label": "failing, sync slot inactive for 2 h, 12 GB WAL retained"},
        "ingress->electric": {"state": "failing", "label": "failing, 100 percent errors"},
    },
    "cause": "electric",
    "story_contains": ["the replication slot electric_slot_default of Electric is inactive for 2 h, 12 GB of WAL is retained on db-primary",
                       "so clients stop receiving changes"],
    "lens_on": True,
}, extras=True)

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

DEMO = os.path.normpath(os.path.join(HERE, "..", "..", "internal", "demo", "data"))

def write_scenario(sc, d):
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "topology.yaml"), "w") as f:
        f.write("# generated by testdata/scenarios/gen.py\n" + dump_yaml(topology(sc["workers"], sc["extras"])) + "\n")
    with open(os.path.join(d, "bindings.yaml"), "w") as f:
        f.write("# generated by testdata/scenarios/gen.py\n" + dump_yaml(bindings(sc["workers"], sc["extras"])) + "\n")
    with open(os.path.join(d, "scenario.yaml"), "w") as f:
        f.write(dump_yaml({"name": sc["name"], "symptom": sc["symptom"], "start": sc["start"],
                           "duration_s": sc["duration"], "tick_s": 5}) + "\n")
    with open(os.path.join(d, "observations.jsonl"), "w") as f:
        for r in sc["obs"]:
            f.write(json.dumps(r, separators=(",", ":")) + "\n")
    with open(os.path.join(d, "expected.yaml"), "w") as f:
        f.write(dump_yaml(sc["expected"]) + "\n")
    print("wrote", d)

for sc in SCENARIOS:
    name = f"{sc['num']:02d}-{sc['slug']}"
    write_scenario(sc, os.path.join(HERE, name))
    # the same fixture, embedded in the binary for `wassup demo`
    write_scenario(sc, os.path.join(DEMO, name))
