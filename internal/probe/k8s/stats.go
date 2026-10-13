package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// statsSummary is the subset of the kubelet's /stats/summary this package
// reads: node filesystem and memory usage, node start time, the memory of
// the kubelet and the container runtime, and per-volume usage.
type statsSummary struct {
	Node struct {
		NodeName         string       `json:"nodeName"`
		StartTime        time.Time    `json:"startTime"`
		Fs               *fsStats     `json:"fs"`
		Memory           *memoryStats `json:"memory"`
		SystemContainers []struct {
			Name   string       `json:"name"`
			Memory *memoryStats `json:"memory"`
		} `json:"systemContainers"`
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

// memoryStats is the memory of the node or of one system container.
type memoryStats struct {
	WorkingSetBytes *uint64 `json:"workingSetBytes"`
	RSSBytes        *uint64 `json:"rssBytes"`
}

// runtimeCacheBytes is the file cache the kubelet and the container runtime
// hold that the node's working set counts: their working set less what
// their processes hold. The runtime writes the containers' logs and unpacks
// image layers, and the pages it touched again stay in the working set
// although the kernel frees them as soon as a process needs the memory. On
// k3s the runtime runs inside the kubelet's unit, so that is one container.
func (s *statsSummary) runtimeCacheBytes() (uint64, bool) {
	var cache uint64
	found := false
	for _, c := range s.Node.SystemContainers {
		if c.Name != "kubelet" && c.Name != "runtime" {
			continue
		}
		m := c.Memory
		if m == nil || m.WorkingSetBytes == nil || m.RSSBytes == nil {
			continue
		}
		found = true
		if *m.WorkingSetBytes > *m.RSSBytes {
			cache += *m.WorkingSetBytes - *m.RSSBytes
		}
	}
	return cache, found
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
