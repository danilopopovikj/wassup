package hcloudprobe

// The types below are the subset of the Cloud API JSON schema the probes
// read. Field names follow hcloud-go/v2/hcloud/schema.

// LoadBalancer is GET /load_balancers/{id}.
type LoadBalancer struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Algorithm struct {
		Type string `json:"type"`
	} `json:"algorithm"`
	Location struct {
		Name string `json:"name"`
	} `json:"location"`
	Services []LoadBalancerService `json:"services"`
	Targets  []LoadBalancerTarget  `json:"targets"`
}

// LoadBalancerService is one listening service.
type LoadBalancerService struct {
	Protocol        string `json:"protocol"`
	ListenPort      int    `json:"listen_port"`
	DestinationPort int    `json:"destination_port"`
	HealthCheck     struct {
		Protocol string `json:"protocol"`
		Port     int    `json:"port"`
	} `json:"health_check"`
}

// LoadBalancerTarget is one target; a label_selector target carries the
// servers it matched in Targets.
type LoadBalancerTarget struct {
	Type   string `json:"type"`
	Server *struct {
		ID int64 `json:"id"`
	} `json:"server"`
	LabelSelector *struct {
		Selector string `json:"selector"`
	} `json:"label_selector"`
	IP *struct {
		IP string `json:"ip"`
	} `json:"ip"`
	HealthStatus []LoadBalancerTargetHealthStatus `json:"health_status"`
	Targets      []LoadBalancerTarget             `json:"targets,omitempty"`
}

// LoadBalancerTargetHealthStatus is the health of a target for one service.
type LoadBalancerTargetHealthStatus struct {
	ListenPort int    `json:"listen_port"`
	Status     string `json:"status"` // healthy, unhealthy, unknown
}

// LoadBalancerMetricsResponse is GET /load_balancers/{id}/metrics.
type LoadBalancerMetricsResponse struct {
	Metrics struct {
		Step       float64 `json:"step"`
		TimeSeries map[string]struct {
			Values [][2]any `json:"values"` // [timestamp, "value"]
		} `json:"time_series"`
	} `json:"metrics"`
}

// Firewall is GET /firewalls/{id}.
type Firewall struct {
	ID        int64              `json:"id"`
	Name      string             `json:"name"`
	Rules     []FirewallRule     `json:"rules"`
	AppliedTo []FirewallResource `json:"applied_to"`
}

// FirewallRule is one rule. Port is "80", "1-65535" or absent for
// protocols without ports; Description may be absent.
type FirewallRule struct {
	Direction      string   `json:"direction"` // in, out
	Protocol       string   `json:"protocol"`  // tcp, udp, icmp, esp, gre
	Port           *string  `json:"port"`
	SourceIPs      []string `json:"source_ips"`
	DestinationIPs []string `json:"destination_ips"`
	Description    *string  `json:"description"`
}

// FirewallResource is one server or label selector the firewall applies to.
type FirewallResource struct {
	Type   string `json:"type"` // server, label_selector
	Server *struct {
		ID int64 `json:"id"`
	} `json:"server,omitempty"`
	LabelSelector *struct {
		Selector string `json:"selector"`
	} `json:"label_selector,omitempty"`
	AppliedToResources []FirewallResource `json:"applied_to_resources,omitempty"`
}
