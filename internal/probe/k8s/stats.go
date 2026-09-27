package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// statsSummary is the subset of the kubelet's /stats/summary this package
// reads: node filesystem usage, node start time and per-volume usage.
type statsSummary struct {
	Node struct {
		NodeName  string    `json:"nodeName"`
		StartTime time.Time `json:"startTime"`
		Fs        *fsStats  `json:"fs"`
	} `json:"node"`
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		Volumes []volumeStats `json:"volume"`
	} `json:"pods"`
}

// fsStats is one filesystem in the summary.
type fsStats struct {
	AvailableBytes *uint64 `json:"availableBytes"`
	CapacityBytes  *uint64 `json:"capacityBytes"`
	UsedBytes      *uint64 `json:"usedBytes"`
}

// volumeStats is one pod volume in the summary.
type volumeStats struct {
	fsStats
	Name   string `json:"name"`
	PVCRef *struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"pvcRef"`
}

// pctOf returns used/capacity as a percentage when both are known.
func (f *fsStats) pctOf() (float64, bool) {
	if f == nil || f.UsedBytes == nil || f.CapacityBytes == nil || *f.CapacityBytes == 0 {
		return 0, false
	}
	return pct(float64(*f.UsedBytes), float64(*f.CapacityBytes)), true
}

// nodeStats fetches the kubelet stats summary of a node through the API
// server proxy. Callers treat an error as "no summary" and carry on.
func nodeStats(ctx context.Context, c *Clients, node string) (*statsSummary, error) {
	if node == "" {
		return nil, fmt.Errorf("no node name")
	}
	body, err := c.proxy(ctx, "/api/v1/nodes/"+node+"/proxy/stats/summary")
	if err != nil {
		return nil, err
	}
	var s statsSummary
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("stats summary of %s: %w", node, err)
	}
	return &s, nil
}
