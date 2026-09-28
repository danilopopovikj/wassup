// Package stubs registers the probe kinds wassup documents but does not yet
// implement. Each validates its spec so `wassup validate` catches typos, and
// reports failed health ("not implemented in this build") so a bound
// component draws as unbound rather than healthy.
package stubs

import (
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

func init() {
	probe.Stub(probe.Access{
		Kind:   "signoz.health",
		Source: "SigNoz's own health and ingestion metrics (otel-collector, query-service, ClickHouse)",
		Delivers: "ingest_rate (spans and metrics per second), disk_pct of the ClickHouse volume; " +
			"NoData when the collector stops receiving, so every signoz.edge reads as no data instead of healthy",
		SpecFields: []string{"url", "token_env"},
		Needs:      "HTTP access to the SigNoz query service; a SigNoz API key in token_env when auth is on",
		Facets:     []string{facet.NameObservability},
		Tier:       probe.TierToken,
	}, "url")

	probe.Stub(probe.Access{
		Kind:   "kubelet.stats",
		Source: "the kubelet summary API (/stats/summary) of one node, through the API server proxy",
		Delivers: "cpu_pct, mem_pct, disk_pct of the node root and image filesystems, iops and used_bytes per volume, " +
			"finer than metrics-server and without it; detail: per-filesystem capacity, inode pressure",
		SpecFields: []string{"node", "kubeconfig", "context"},
		Needs:      "RBAC get on nodes/proxy for the kubeconfig's identity",
		Facets:     []string{facet.NameNode},
	}, "node")
}
