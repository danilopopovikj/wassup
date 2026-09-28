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
	// Entry marks a type that starts a path for the issue lens.
	Entry bool
	// Container marks a type that hosts workloads placed by runs_on.
	Container bool
}

// Catalog is the thirteen building block types plus custom.
var Catalog = map[string]TypeSpec{
	"dns":      {Type: "dns", Lane: LaneEdge, Probes: []string{"dns.record"}, Entry: true, Notes: "Optional, shows the hostname and whether it resolves to the LB"},
	"firewall": {Type: "firewall", Lane: LaneEdge, Gauges: []GaugeSpec{{Name: "rules", Short: "rule", Metric: "rules", RateOnly: true}}, Probes: []string{"hcloud.firewall", "terraform.state"}, Entry: true, Notes: "Drawn as a gate on the path; a blocked edge names the rule"},
	"lb": {Type: "lb", Lane: LaneEdge, Gauges: []GaugeSpec{
		{Name: "conns", Short: "conn", Metric: "connections", RateOnly: true},
		{Name: "req/s", Short: "req", Metric: "rate", RateOnly: true},
		{Name: "targets", Short: "tgts", Used: "targets_healthy", Max: "targets_total"},
	}, Probes: []string{"hcloud.lb"}, Entry: true, Notes: "Shows which nodes are in rotation"},
	"ingress": {Type: "ingress", Lane: LaneEdge, Gauges: []GaugeSpec{
		{Name: "req/s", Short: "req", Metric: "rate", RateOnly: true},
		{Name: "errors", Short: "err", Metric: "error_rate", Unit: "%"},
		{Name: "cert", Metric: "cert_days", Unit: "d", Countdown: true},
	}, Probes: []string{"k8s.ingress", "cert.tls"}, Entry: true, Notes: "Merges into lb box when both exist and user prefers"},
	"node": {Type: "node", Lane: LaneCompute, Gauges: []GaugeSpec{
		{Name: "cpu", Metric: "cpu_pct"},
		{Name: "ram", Metric: "mem_pct"},
		{Name: "disk", Metric: "disk_pct"},
		{Name: "pods", Metric: "pods", RateOnly: true},
	}, Probes: []string{"k8s.node", "kubelet.stats"}, Container: true, Notes: "Container for workloads placed by runs_on"},
	"workload": {Type: "workload", Lane: LaneCompute, Gauges: []GaugeSpec{
		{Name: "ready", Short: "up", Used: "replicas_ready", Max: "replicas_desired"},
		{Name: "cpu", Metric: "cpu_pct"},
		{Name: "ram", Metric: "mem_pct"},
		{Name: "restarts", Short: "rst", Metric: "restarts", RateOnly: true},
	}, Probes: []string{"k8s.workload"}, Notes: "Deployment, StatefulSet or DaemonSet; one box per workload, not per pod"},
	"job": {Type: "job", Lane: LaneCompute, Gauges: []GaugeSpec{
		{Name: "running", Short: "run", Metric: "active", RateOnly: true},
		{Name: "ok 24h", Short: "ok", Metric: "succeeded", RateOnly: true},
		{Name: "failed", Short: "fail", Metric: "failed", RateOnly: true},
	}, Probes: []string{"k8s.cronjob"}, Entry: true, Notes: "CronJobs and one-off jobs"},
	"queue": {Type: "queue", Lane: LaneCompute, Gauges: []GaugeSpec{
		{Name: "depth", Short: "dpth", Metric: "depth", RateOnly: true},
		{Name: "oldest", Short: "old", Metric: "oldest_age_s", Unit: "s", RateOnly: true},
		{Name: "consumers", Short: "cons", Metric: "consumers", RateOnly: true},
	}, Probes: []string{"celery.queue", "redis.list", "amqp.queue"}, Notes: "Depth is drawn as a growing pile"},
	"cache": {Type: "cache", Lane: LaneData, Gauges: []GaugeSpec{
		{Name: "mem", Metric: "mem_pct"},
		{Name: "hits", Metric: "hit_rate", Unit: "%"},
		{Name: "evicted", Short: "evic", Metric: "evictions", RateOnly: true},
	}, Probes: []string{"redis.info"}, Notes: "Hit rate below threshold flips it to failing"},
	"db": {Type: "db", Lane: LaneData, Gauges: []GaugeSpec{
		{Name: "cpu", Metric: "cpu_pct"},
		{Name: "ram", Metric: "mem_pct"},
		{Name: "disk", Metric: "disk_pct"},
		{Name: "conns", Short: "conn", Used: "connections_used", Max: "connections_max"},
		{Name: "lag", Metric: "lag_bytes", Unit: "B", RateOnly: true},
	}, Probes: []string{"cnpg.cluster", "cnpg.instance", "pg.stats"}, Notes: "Has roles: one primary, replicas; replication edges drawn between them"},
	"storage": {Type: "storage", Lane: LaneData, Gauges: []GaugeSpec{
		{Name: "used", Metric: "disk_pct"},
		{Name: "iops", Metric: "iops", RateOnly: true},
	}, Probes: []string{"k8s.pvc", "s3.bucket"}, Notes: "Volumes, buckets"},
	"observability": {Type: "observability", Lane: LaneSide, Gauges: []GaugeSpec{
		{Name: "ingest", Short: "ing", Metric: "ingest_rate", RateOnly: true},
		{Name: "disk", Metric: "disk_pct"},
	}, Probes: []string{"signoz.health"}, Notes: "Marked as the source of traffic data; if it fails, edges show no data, not idle"},
	"external": {Type: "external", Lane: LaneSide, Gauges: []GaugeSpec{
		{Name: "latency", Short: "lat", Metric: "latency_ms", Unit: "ms", RateOnly: true},
		{Name: "errors", Short: "err", Metric: "error_rate", Unit: "%"},
		{Name: "timeouts", Short: "tmo", Metric: "timeout_rate", Unit: "%"},
	}, Probes: []string{"signoz.edge", "http.ping"}, Notes: "GitHub, LLM providers, payment APIs, anything outside your control"},
	"custom": {Type: "custom", Lane: LaneCompute, Notes: "Free icon, no gauges"},
}

// CatalogOrder is the documented order of types.
var CatalogOrder = []string{"dns", "firewall", "lb", "ingress", "node", "workload", "job", "queue", "cache", "db", "storage", "observability", "external", "custom"}

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
