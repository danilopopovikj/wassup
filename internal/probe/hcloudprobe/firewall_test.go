package hcloudprobe

import (
	"testing"
)

func str(s string) *string { return &s }

func TestPortInRange(t *testing.T) {
	cases := []struct {
		spec string
		port int
		want bool
	}{
		{"4317", 4317, true}, {"4317", 4318, false}, {"1-65535", 4317, true}, {"80-90", 79, false},
		{"80-90", 90, true}, {"", 22, true}, {"any", 22, true}, {"eighty", 80, false}, {"10-x", 10, false},
	}
	for _, c := range cases {
		if got := PortInRange(c.spec, c.port); got != c.want {
			t.Errorf("PortInRange(%q, %d) = %v, want %v", c.spec, c.port, got, c.want)
		}
	}
}

// lbOnly is scenario 08: after "allow-lb-only" the only inbound rules let the
// load balancer in on 80/443 and ssh from the office; OTLP 4317 to SigNoz is
// no longer allowed.
var lbOnly = []FirewallRule{
	{Direction: "in", Protocol: "tcp", Port: str("80"), SourceIPs: []string{"10.0.0.5/32"}, Description: str("allow-lb-only")},
	{Direction: "in", Protocol: "tcp", Port: str("443"), SourceIPs: []string{"10.0.0.5/32"}, Description: str("allow-lb-only")},
	{Direction: "in", Protocol: "tcp", Port: str("22"), SourceIPs: []string{"203.0.113.0/24"}, Description: str("ssh-office")},
	{Direction: "in", Protocol: "icmp", SourceIPs: []string{"0.0.0.0/0", "::/0"}},
	{Direction: "out", Protocol: "tcp", Port: str("1-65535"), DestinationIPs: []string{"0.0.0.0/0", "::/0"}, Description: str("egress")},
}

func TestRuleAllows(t *testing.T) {
	if !RuleAllows(lbOnly[0], 80, "tcp") {
		t.Error("80/tcp from the lb should be allowed")
	}
	if RuleAllows(lbOnly[0], 80, "udp") {
		t.Error("protocol must match")
	}
	if RuleAllows(lbOnly[4], 4317, "tcp") {
		t.Error("an outbound rule never allows inbound traffic")
	}
	if !RuleAllows(lbOnly[3], 0, "icmp") {
		t.Error("icmp rule has no port and allows the protocol")
	}
	if RuleAllows(FirewallRule{Direction: "in", Protocol: "tcp", Port: str("1-65535")}, 80, "tcp") {
		t.Error("a rule without sources allows nobody")
	}
}

func TestAllowed(t *testing.T) {
	ok, by := Allowed(lbOnly, 443, "tcp")
	if !ok || by != "allow-lb-only" {
		t.Errorf("443/tcp: %v %q", ok, by)
	}
	ok, closest := Allowed(lbOnly, 4317, "tcp")
	if ok {
		t.Fatal("4317/tcp should be denied")
	}
	if closest != "allow-lb-only" {
		t.Errorf("closest rule = %q, want the first inbound tcp rule", closest)
	}
	ok, closest = Allowed(lbOnly, 53, "udp")
	if ok || closest != "" {
		t.Errorf("udp: %v %q, no inbound udp rule exists", ok, closest)
	}
	wide := append([]FirewallRule{}, lbOnly...)
	wide = append(wide, FirewallRule{Direction: "in", Protocol: "TCP", Port: str("1-65535"), SourceIPs: []string{"10.0.0.0/8"}})
	if ok, by := Allowed(wide, 4317, "tcp"); !ok || by != "in TCP 1-65535 10.0.0.0/8" {
		t.Errorf("range rule: %v %q", ok, by)
	}
}

func TestAppliedTo(t *testing.T) {
	res := []FirewallResource{
		{Type: "server", Server: &struct {
			ID int64 `json:"id"`
		}{42}},
		{Type: "label_selector", LabelSelector: &struct {
			Selector string `json:"selector"`
		}{"role=node"}, AppliedToResources: []FirewallResource{{Type: "server", Server: &struct {
			ID int64 `json:"id"`
		}{7}}}},
	}
	got := appliedTo(res)
	want := []string{"server/42", "selector/role=node", "server/7"}
	if len(got) != len(want) {
		t.Fatalf("appliedTo = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("appliedTo[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFirewallValidate(t *testing.T) {
	p := &FirewallProbe{}
	if err := p.Validate(map[string]any{}); err == nil {
		t.Error("name or id required")
	}
	if err := p.Validate(map[string]any{"name": "main", "port": 4317, "protocol": "tcp", "interval": "2m"}); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := p.Validate(map[string]any{"id": "12", "port": 4317}); err != nil {
		t.Errorf("numeric string id: %v", err)
	}
	if err := p.Validate(map[string]any{"name": "main", "port": 70000}); err == nil {
		t.Error("port out of range")
	}
	if err := p.Validate(map[string]any{"name": "main", "protocol": "sctp"}); err == nil {
		t.Error("unknown protocol")
	}
	if err := p.Validate(map[string]any{"id": "twelve"}); err == nil {
		t.Error("bad id")
	}
}
