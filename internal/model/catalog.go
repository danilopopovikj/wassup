package model

// Lane is where a type lives on the diagram.
type Lane string

// Lanes top to bottom, side on the right.
const (
	LaneEdge    Lane = "edge"
	LaneCompute Lane = "compute"
	LaneData    Lane = "data"
	LaneSide    Lane = "side"
)

// AllLanes in drawing order.
var AllLanes = []Lane{LaneEdge, LaneCompute, LaneData, LaneSide}

// GaugeSpec describes one gauge a type shows in its box.
type GaugeSpec struct {
	Name   string // "cpu"
	Short  string // at most four cells, drawn in the box; defaults to Name
	Metric string // "cpu_pct" (a percent) or "" when computed from Used/Max
	Used   string // metric holding the used value
	Max    string // metric holding the max value
	Unit   string
	// Countdown gauges (cert days) fill as the value shrinks.
	Countdown bool
	// Rate gauges show a value but no percent.
	RateOnly bool
}

// TypeSpec is one entry of the component catalog.
type TypeSpec struct {
	Type   string
	Lane   Lane
	Gauges []GaugeSpec
	Probes []string
	Notes  string
	// Facets a probe bound to this type may fill (internal/probe/facet);
	// the first is the type's own facet.
	Facets []string
	// Entry marks a type that starts a path for the issue lens.
	Entry bool
	// Container marks a type that hosts workloads placed by runs_on.
	Container bool
}

// Catalog is the thirteen building block types plus custom.
var Catalog = map[string]TypeSpec{
	"dns":      {Type: "dns", Lane: LaneEdge, Facets: []string{"DNSFacet", "CertificateFacet"}, Probes: []string{"dns.record"}, Entry: true, Notes: "Optional, shows the hostname and whether it resolves to the LB"},
	"firewall": {Type: "firewall", Lane: LaneEdge, Facets: []string{"FirewallFacet"}, Gauges: []GaugeSpec{{Name: "rules", Short: "rule", Metric: "rules", RateOnly: true}}, Probes: []string{"hcloud.firewall", "terraform.state"}, Entry: true, Notes: "Drawn as a gate on the path; a blocked edge names the rule"},
	"loadbalancer": {Type: "loadbalancer", Lane: LaneEdge, Facets: []string{"LoadBalancerFacet", "CertificateFacet"}, Gauges: []GaugeSpec{
		{Name: "conns", Short: "conn", Metric: "connections", RateOnly: true},
		{Name: "req/s", Short: "req", Metric: "rate", RateOnly: true},
		{Name: "targets", Short: "tgts", Used: "targets_healthy", Max: "targets_total"},
	}, Probes: []string{"hcloud.lb"}, Entry: true, Notes: "Shows which nodes are in rotation"},
	"ingress": {Type: "ingress", Lane: LaneEdge, Facets: []string{"IngressFacet", "CertificateFacet"}, Gauges: []GaugeSpec{
		{Name: "req/s", Short: "req", Metric: "rate", RateOnly: true},
		{Name: "errors", Short: "err", Metric: "error_rate", Unit: "%"},
		{Name: "cert", Metric: "cert_days", Unit: "d", Countdown: true},
	}, Probes: []string{"k8s.ingress", "cert.tls"}, Entry: true, Notes: "Merges into lb box when both exist and user prefers"},
	"node": {Type: "node", Lane: LaneCompute, Facets: []string{"NodeFacet"}, Gauges: []GaugeSpec{
		{Name: "cpu", Metric: "cpu_pct"},
		{Name: "ram", Metric: "mem_pct"},
		{Name: "disk", Metric: "disk_pct"},
		{Name: "pods", Metric: "pods", RateOnly: true},
	}, Probes: []string{"k8s.node", "kubelet.stats"}, Container: true, Notes: "Container for workloads placed by runs_on"},
	"workload": {Type: "workload", Lane: LaneCompute, Facets: []string{"WorkloadFacet", "ExternalFacet"}, Gauges: []GaugeSpec{
		{Name: "ready", Short: "up", Used: "replicas_ready", Max: "replicas_desired"},
		{Name: "cpu", Metric: "cpu_pct"},
		{Name: "ram", Metric: "mem_pct"},
		{Name: "restarts", Short: "rst", Metric: "restarts", RateOnly: true},
	}, Probes: []string{"k8s.workload", "hatchet.health", "http.ping"}, Notes: "Deployment, StatefulSet or DaemonSet; one box per workload, not per pod. Worker fleets are the worker type"},
	"backgroundworker": {Type: "backgroundworker", Lane: LaneCompute, Facets: []string{"BackgroundWorkerFacet", "WorkloadFacet"}, Gauges: []GaugeSpec{
		{Name: "workers", Short: "wrkr", Used: "workers_online", Max: "workers_total"},
		{Name: "slots", Short: "slot", Used: "pool_used", Max: "pool_max"},
		{Name: "ready", Short: "up", Used: "replicas_ready", Max: "replicas_desired"},
		{Name: "cpu", Metric: "cpu_pct"},
		{Name: "ram", Metric: "mem_pct"},
		{Name: "restarts", Short: "rst", Metric: "restarts", RateOnly: true},
	}, Probes: []string{"hatchet.workers", "celery.worker", "k8s.workload"}, Notes: "A fleet of background workers (Celery, Hatchet); every slot busy with work queued reads as waiting"},
	"scheduledjob": {Type: "scheduledjob", Lane: LaneCompute, Facets: []string{"ScheduledJobFacet"}, Gauges: []GaugeSpec{
		{Name: "running", Short: "run", Metric: "active", RateOnly: true},
		{Name: "ok 24h", Short: "ok", Metric: "succeeded", RateOnly: true},
		{Name: "failed", Short: "fail", Metric: "failed", RateOnly: true},
	}, Probes: []string{"k8s.cronjob", "hatchet.workflow"}, Entry: true, Notes: "CronJobs, one-off jobs and Hatchet workflows"},
	"queue": {Type: "queue", Lane: LaneCompute, Facets: []string{"QueueFacet"}, Gauges: []GaugeSpec{
		{Name: "depth", Short: "dpth", Metric: "depth", RateOnly: true},
		{Name: "oldest", Short: "old", Metric: "oldest_age_s", Unit: "s", RateOnly: true},
		{Name: "consumers", Short: "cons", Metric: "consumers", RateOnly: true},
	}, Probes: []string{"celery.queue", "redis.list", "amqp.queue", "hatchet.queue"}, Notes: "Depth is drawn as a growing pile"},
	"cache": {Type: "cache", Lane: LaneData, Facets: []string{"CacheFacet"}, Gauges: []GaugeSpec{
		{Name: "mem", Metric: "mem_pct"},
		{Name: "hits", Metric: "hit_rate", Unit: "%"},
		{Name: "evicted", Short: "evic", Metric: "evictions", RateOnly: true},
	}, Probes: []string{"redis.info"}, Notes: "Hit rate below threshold flips it to failing"},
	"database": {Type: "database", Lane: LaneData, Facets: []string{"DatabaseFacet", "ReplicationFacet"}, Gauges: []GaugeSpec{
		{Name: "cpu", Metric: "cpu_pct"},
		{Name: "ram", Metric: "mem_pct"},
		{Name: "disk", Metric: "disk_pct"},
		{Name: "conns", Short: "conn", Used: "connections_used", Max: "connections_max"},
		{Name: "lag", Metric: "lag_bytes", Unit: "B", RateOnly: true},
	}, Probes: []string{"cnpg.cluster", "cnpg.instance", "pg.stats"}, Notes: "Has roles: one primary, replicas; replication edges drawn between them"},
	"storage": {Type: "storage", Lane: LaneData, Facets: []string{"StorageFacet"}, Gauges: []GaugeSpec{
		{Name: "used", Metric: "disk_pct"},
		{Name: "size", Metric: "used_bytes", Unit: "B", RateOnly: true},
		{Name: "objects", Short: "objs", Metric: "objects", RateOnly: true},
		{Name: "iops", Metric: "iops", RateOnly: true},
	}, Probes: []string{"k8s.pvc", "s3.bucket"}, Notes: "Volumes (k8s.pvc) and buckets (s3.bucket); a bucket shows its size and object count, and a fill gauge only when quota_bytes is set"},
	"syncengine": {Type: "syncengine", Lane: LaneData, Facets: []string{"SyncEngineFacet", "ReplicationFacet"}, Gauges: []GaugeSpec{
		{Name: "lag", Metric: "lag_bytes", Unit: "B", RateOnly: true},
		{Name: "wal", Metric: "wal_retained_bytes", Unit: "B", RateOnly: true},
		{Name: "latency", Short: "lat", Metric: "latency_ms", Unit: "ms", RateOnly: true},
	}, Probes: []string{"electric.sync", "pg.stats"}, Notes: "A sync engine that follows the database's replication stream (Electric SQL); bind pg.stats with its slot name so an inactive slot shows on the replication edge"},
	"observability": {Type: "observability", Lane: LaneSide, Facets: []string{"ObservabilityFacet", "ExternalFacet"}, Gauges: []GaugeSpec{
		{Name: "ingest", Short: "ing", Metric: "ingest_rate", RateOnly: true},
		{Name: "disk", Metric: "disk_pct"},
	}, Probes: []string{"signoz.health"}, Notes: "Marked as the source of traffic data; if it fails, edges show no data, not idle"},
	"external": {Type: "external", Lane: LaneSide, Facets: []string{"ExternalFacet"}, Gauges: []GaugeSpec{
		{Name: "latency", Short: "lat", Metric: "latency_ms", Unit: "ms", RateOnly: true},
		{Name: "errors", Short: "err", Metric: "error_rate", Unit: "%"},
		{Name: "timeouts", Short: "tmo", Metric: "timeout_rate", Unit: "%"},
	}, Probes: []string{"signoz.edge", "http.ping"}, Notes: "GitHub, LLM providers, payment APIs, anything outside your control"},
	"custom": {Type: "custom", Lane: LaneCompute, Facets: nil, Notes: "Free icon, no gauges"},
}

// CatalogOrder is the documented order of types.
var CatalogOrder = []string{"dns", "firewall", "loadbalancer", "ingress", "node", "workload", "backgroundworker", "scheduledjob", "queue", "cache", "database", "storage", "syncengine", "observability", "external", "custom"}

// TypeAliases are older spellings still accepted in topology.yaml and
// normalized on load.
var TypeAliases = map[string]string{"lb": "loadbalancer", "db": "database", "job": "scheduledjob", "worker": "backgroundworker", "sync": "syncengine"}

// EdgeFacets are the facets an edge binding may fill.
var EdgeFacets = []string{"TrafficFacet", "ReplicationFacet", "FirewallFacet"}

// NormalizeType maps an alias to the catalog type.
func NormalizeType(t string) string {
	if n, ok := TypeAliases[t]; ok {
		return n
	}
	return t
}

// GroupKinds are the allowed group kinds.
var GroupKinds = []string{"cloud", "region", "cluster", "namespace", "zone"}

// EdgeKinds are the allowed edge kinds.
var EdgeKinds = []string{"http", "grpc", "tcp", "sql", "replication", "queue", "cache", "external"}

// RateUnit returns the unit an edge kind labels its rate in.
func RateUnit(kind string) string {
	switch kind {
	case "http", "grpc", "external":
		return "req/s"
	case "sql":
		return "tx/s"
	case "queue":
		return "jobs/s"
	case "replication":
		return "B lag"
	case "cache":
		return "ops/s"
	case "tcp":
		return "conn/s"
	}
	return "/s"
}

// LaneOf returns the lane of a component, honoring the lane override.
func LaneOf(c Component) Lane {
	switch Lane(c.Lane) {
	case LaneEdge, LaneCompute, LaneData, LaneSide:
		return Lane(c.Lane)
	}
	if t, ok := Catalog[c.Type]; ok {
		return t.Lane
	}
	return LaneCompute
}

// IsEntry reports whether the component type starts a lens path.
func IsEntry(c Component) bool {
	t, ok := Catalog[c.Type]
	return ok && t.Entry
}
