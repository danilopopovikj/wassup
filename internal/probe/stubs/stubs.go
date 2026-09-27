// Package stubs registers the probe kinds wassup documents but does not yet
// implement. Each validates its spec so `wassup validate` catches typos, and
// reports failed health ("not implemented in this build") so a bound
// component draws as unbound rather than healthy.
package stubs

import "github.com/danilopopovikj/wassup/internal/probe"

func init() {
	probe.Stub(probe.Access{
		Kind:   "signoz.edge",
		Source: "SigNoz's ClickHouse-backed query API over the traces between two services",
		Delivers: "for the edge from->to: rate (calls per second), error_rate, timeout_rate, p95_ms and rate_baseline " +
			"from the service map spans of the last minute; detail: the top failing operations",
		SpecFields: []string{"from", "to", "url", "token_env", "window"},
		Needs:      "a SigNoz API key in the environment variable named by token_env with read access to the query service",
	}, "from", "to", "url")

	probe.Stub(probe.Access{
		Kind:   "signoz.health",
		Source: "SigNoz's own health and ingestion metrics (otel-collector, query-service, ClickHouse)",
		Delivers: "ingest_rate (spans and metrics per second), disk_pct of the ClickHouse volume; " +
			"NoData when the collector stops receiving, so every signoz.edge reads as no data instead of healthy",
		SpecFields: []string{"url", "token_env"},
		Needs:      "HTTP access to the SigNoz query service; a SigNoz API key in token_env when auth is on",
	}, "url")

	probe.Stub(probe.Access{
		Kind:   "s3.bucket",
		Source: "an S3-compatible object store (Hetzner Object Storage, MinIO, AWS) via ListObjectsV2 and bucket metrics",
		Delivers: "used_bytes, total_bytes when the endpoint exposes a quota, rate of PUT/GET requests when metrics exist; " +
			"a failing HeadBucket marks the storage failing; detail: object count, last write, region",
		SpecFields: []string{"bucket", "endpoint", "region", "access_key_env", "secret_key_env"},
		Needs:      "read-only credentials (s3:ListBucket, s3:GetBucketLocation) in the environment variables named by access_key_env and secret_key_env",
	}, "bucket", "endpoint", "region")

	probe.Stub(probe.Access{
		Kind:   "kubelet.stats",
		Source: "the kubelet summary API (/stats/summary) of one node, through the API server proxy",
		Delivers: "cpu_pct, mem_pct, disk_pct of the node root and image filesystems, iops and used_bytes per volume, " +
			"finer than metrics-server and without it; detail: per-filesystem capacity, inode pressure",
		SpecFields: []string{"node", "kubeconfig", "context"},
		Needs:      "RBAC get on nodes/proxy for the kubeconfig's identity",
	}, "node")

	probe.Stub(probe.Access{
		Kind:   "amqp.queue",
		Source: "the RabbitMQ management API (/api/queues/<vhost>/<queue>)",
		Delivers: "depth (messages ready), oldest_age_s when the queue exposes head message timestamps, consumers, " +
			"growth_per_min and rate (publish and deliver rates); detail: vhost, durable, unacked messages, memory",
		SpecFields: []string{"url", "queue", "vhost", "user_env", "password_env"},
		Needs:      "a management user with the monitoring tag; credentials from user_env and password_env",
	}, "url", "queue")
}
