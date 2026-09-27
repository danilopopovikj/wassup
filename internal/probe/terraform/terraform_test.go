package terraform

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// sample is a trimmed `terraform show -json` document: a firewall, a server
// and a child module with a load balancer and a data source.
const sample = `{
  "format_version": "1.0",
  "terraform_version": "1.9.5",
  "values": {
    "root_module": {
      "resources": [
        {
          "address": "hcloud_firewall.main",
          "mode": "managed",
          "type": "hcloud_firewall",
          "name": "main",
          "provider_name": "registry.terraform.io/hetznercloud/hcloud",
          "values": {
            "id": "123",
            "name": "k8s-main",
            "rule": [
              {"direction": "in", "protocol": "tcp", "port": "22", "source_ips": ["10.0.0.0/8"], "destination_ips": [], "description": "ssh"},
              {"direction": "in", "protocol": "tcp", "port": "80-443", "source_ips": ["0.0.0.0/0", "::/0"], "destination_ips": [], "description": "allow-lb-only"},
              {"direction": "in", "protocol": "icmp", "port": "", "source_ips": ["0.0.0.0/0"], "destination_ips": [], "description": ""},
              {"direction": "out", "protocol": "tcp", "port": "any", "source_ips": [], "destination_ips": ["0.0.0.0/0"], "description": ""}
            ]
          }
        },
        {
          "address": "hcloud_server.node[0]",
          "mode": "managed",
          "type": "hcloud_server",
          "name": "node",
          "index": 0,
          "provider_name": "registry.terraform.io/hetznercloud/hcloud",
          "values": {"name": "node-1", "server_type": "cpx31"}
        },
        {
          "address": "data.hcloud_image.ubuntu",
          "mode": "data",
          "type": "hcloud_image",
          "name": "ubuntu",
          "provider_name": "registry.terraform.io/hetznercloud/hcloud",
          "values": {"name": "ubuntu-24.04"}
        }
      ],
      "child_modules": [
        {
          "address": "module.lb",
          "resources": [
            {
              "address": "module.lb.hcloud_load_balancer.main",
              "mode": "managed",
              "type": "hcloud_load_balancer",
              "name": "main",
              "provider_name": "registry.terraform.io/hetznercloud/hcloud",
              "values": {"name": "lb", "load_balancer_type": "lb11"}
            }
          ],
          "child_modules": [
            {
              "address": "module.lb.module.dns",
              "resources": [
                {
                  "address": "module.lb.module.dns.hcloud_rdns.lb",
                  "mode": "managed",
                  "type": "hcloud_rdns",
                  "name": "lb",
                  "provider_name": "registry.terraform.io/hetznercloud/hcloud",
                  "values": {"dns_ptr": "bookstore.example"}
                }
              ]
            }
          ]
        }
      ]
    }
  }
}`

func TestParseShow(t *testing.T) {
	st, err := ParseShow([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if st.TerraformVersion != "1.9.5" {
		t.Fatalf("version = %q", st.TerraformVersion)
	}
	want := []string{
		"hcloud_firewall.main",
		"hcloud_server.node[0]",
		"module.lb.hcloud_load_balancer.main",
		"module.lb.module.dns.hcloud_rdns.lb",
	}
	if len(st.Resources) != len(want) {
		t.Fatalf("got %d resources, want %d (data sources must be skipped): %+v", len(st.Resources), len(want), st.Resources)
	}
	for i, r := range st.Resources {
		if r.Address != want[i] {
			t.Errorf("resource %d = %q, want %q", i, r.Address, want[i])
		}
		if r.Provider != "registry.terraform.io/hetznercloud/hcloud" || r.Fingerprint == "" {
			t.Errorf("resource %s: provider %q fingerprint %q", r.Address, r.Provider, r.Fingerprint)
		}
	}
	if st.Resources[3].Type != "hcloud_rdns" || st.Resources[3].Name != "lb" || st.Resources[3].Module != "module.lb.module.dns" {
		t.Fatalf("nested resource = %+v", st.Resources[3])
	}

	if got := st.Filter("module.lb"); len(got) != 2 {
		t.Fatalf("filter module.lb = %d resources", len(got))
	}
	if got := st.Filter("hcloud_server.node"); len(got) != 1 {
		t.Fatalf("filter indexed resource = %d resources", len(got))
	}
	if got := st.Filter("hcloud_server.nod"); len(got) != 0 {
		t.Fatalf("filter must not match a prefix inside a name: %d", len(got))
	}
}

func TestParseShowNoState(t *testing.T) {
	if _, err := ParseShow([]byte(`{"format_version":"1.0"}`)); !errors.Is(err, ErrNoState) {
		t.Fatalf("err = %v, want ErrNoState", err)
	}
	if _, err := ParseShow([]byte(`not json`)); err == nil || errors.Is(err, ErrNoState) {
		t.Fatalf("err = %v", err)
	}
}

func TestFirewalls(t *testing.T) {
	st, _ := ParseShow([]byte(sample))
	fws := Firewalls(st.Resources)
	if len(fws) != 1 {
		t.Fatalf("got %d firewalls", len(fws))
	}
	fw := fws[0]
	if fw.Address != "hcloud_firewall.main" || fw.Name != "k8s-main" || len(fw.Rules) != 4 {
		t.Fatalf("firewall = %+v", fw)
	}
	if RuleCount(fws) != 4 {
		t.Fatalf("rules = %d", RuleCount(fws))
	}
	if fw.Rules[1].Description != "allow-lb-only" || len(fw.Rules[1].SourceIPs) != 2 {
		t.Fatalf("rule = %+v", fw.Rules[1])
	}

	cases := []struct {
		port int
		want bool
	}{
		{22, true},
		{80, true},
		{443, true},
		{200, true},   // inside 80-443
		{4317, false}, // otel collector: nothing allows it
		{8080, false},
		{23, false},
	}
	for _, c := range cases {
		allowed, denying := AllowsInboundTCP(fws, c.port)
		if allowed != c.want {
			t.Errorf("port %d: allowed = %v, want %v", c.port, allowed, c.want)
		}
		if !allowed && (len(denying) != 1 || denying[0].Name != "k8s-main") {
			t.Errorf("port %d: denying = %+v", c.port, denying)
		}
	}
	// Outbound rules never allow inbound traffic, even with port any.
	if fw.Rules[3].AllowsInboundTCP(9999) {
		t.Fatal("outbound rule counted as inbound")
	}
	// No firewalls: nothing denies.
	if ok, _ := AllowsInboundTCP(nil, 4317); !ok {
		t.Fatal("no firewalls must not deny")
	}
}

func TestPortMatches(t *testing.T) {
	cases := map[string]map[int]bool{
		"any":    {1: true, 65535: true},
		"":       {80: true},
		"80":     {80: true, 81: false},
		"80-85":  {79: false, 80: true, 83: true, 85: true, 86: false},
		" 443 ":  {443: true},
		"eighty": {80: false},
	}
	for spec, ports := range cases {
		for port, want := range ports {
			if got := PortMatches(spec, port); got != want {
				t.Errorf("PortMatches(%q, %d) = %v, want %v", spec, port, got, want)
			}
		}
	}
}

func TestDiff(t *testing.T) {
	st, _ := ParseShow([]byte(sample))
	changed := strings.Replace(sample, `"server_type": "cpx31"`, `"server_type": "cpx41"`, 1)
	changed = strings.Replace(changed, `"address": "module.lb.module.dns.hcloud_rdns.lb",`, `"address": "module.lb.module.dns.hcloud_rdns.old",`, 1)
	st2, err := ParseShow([]byte(changed))
	if err != nil {
		t.Fatal(err)
	}
	added, removed, mod := Diff(st.Resources, st2.Resources)
	if len(added) != 1 || added[0] != "module.lb.module.dns.hcloud_rdns.old" {
		t.Fatalf("added = %v", added)
	}
	if len(removed) != 1 || removed[0] != "module.lb.module.dns.hcloud_rdns.lb" {
		t.Fatalf("removed = %v", removed)
	}
	if len(mod) != 1 || mod[0] != "hcloud_server.node[0]" {
		t.Fatalf("changed = %v", mod)
	}
	if a, r, c := Diff(st.Resources, st.Resources); len(a)+len(r)+len(c) != 0 {
		t.Fatalf("identical inventories differ: %v %v %v", a, r, c)
	}
}

// fakeShow returns a sequence of documents, one per call, then the last.
type fakeShow struct {
	docs  []string
	err   error
	calls int
}

func (f *fakeShow) show(_ context.Context, _, _ string) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	i := f.calls - 1
	if i >= len(f.docs) {
		i = len(f.docs) - 1
	}
	return []byte(f.docs[i]), nil
}

func TestCheckFirewallEdge(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fs := &fakeShow{docs: []string{sample}}
	p := &Probe{show: fs.show, now: func() time.Time { return now }}
	cfg := config{target: "fw->signoz", dir: "/infra", port: 4317}

	o := p.check(context.Background(), cfg)
	if o.Err != "" {
		t.Fatal(o.Err)
	}
	if o.Target != "fw->signoz" || o.Probe != Kind {
		t.Fatalf("target/probe = %q/%q", o.Target, o.Probe)
	}
	// The target id contains "fw": the rules metric is emitted.
	if o.Metrics["rules"] != 4 {
		t.Fatalf("rules = %v", o.Metrics)
	}
	cond, ok := model.HasCondition(o.Conditions, model.CondFirewallDenied)
	if !ok {
		t.Fatalf("no FirewallDenied: %+v", o)
	}
	if cond.Detail != "k8s-main" || cond.Ref != "hcloud_firewall.main" || !cond.Since.Equal(now) {
		t.Fatalf("condition = %+v", cond)
	}
	if o.Detail["resources"] != 4 || o.Detail["port_allowed"] != false {
		t.Fatalf("detail = %v", o.Detail)
	}
	if len(o.Events) != 0 {
		t.Fatalf("first run must not emit events: %+v", o.Events)
	}

	// Since sticks to the first denied observation on the next run.
	now = now.Add(time.Minute)
	o = p.check(context.Background(), cfg)
	cond, _ = model.HasCondition(o.Conditions, model.CondFirewallDenied)
	if !cond.Since.Equal(now.Add(-time.Minute)) {
		t.Fatalf("since moved: %v", cond.Since)
	}

	// Opening the port clears the condition and yields a terraform event.
	fs.docs = append(fs.docs, strings.Replace(sample, `"port": "80-443"`, `"port": "80-4317"`, 1))
	fs.calls = len(fs.docs) - 1
	o = p.check(context.Background(), cfg)
	if _, ok := model.HasCondition(o.Conditions, model.CondFirewallDenied); ok {
		t.Fatalf("FirewallDenied still raised after the rule change: %+v", o.Conditions)
	}
	if len(o.Events) != 1 || o.Events[0].Kind != "terraform" || o.Events[0].Ref != "hcloud_firewall.main" || o.Events[0].Summary != "terraform apply: hcloud_firewall.main" {
		t.Fatalf("events = %+v", o.Events)
	}
	if o.Events[0].Target != "fw->signoz" || !o.Events[0].At.Equal(now) {
		t.Fatalf("event = %+v", o.Events[0])
	}
	changed, _ := o.Detail["changed"].([]string)
	if len(changed) != 1 || changed[0] != "hcloud_firewall.main" {
		t.Fatalf("changed = %v", o.Detail["changed"])
	}
	if p.Health().State != probe.HealthOK {
		t.Fatalf("health = %+v", p.Health())
	}
}

func TestCheckResourceFilter(t *testing.T) {
	fs := &fakeShow{docs: []string{sample}}
	p := &Probe{show: fs.show}

	// A component bound to the server: no firewall metrics, no port check.
	o := p.check(context.Background(), config{target: "node-1", dir: "/infra", resource: "hcloud_server.node"})
	if o.Metrics != nil {
		t.Fatalf("metrics on a non-firewall binding: %v", o.Metrics)
	}
	if o.Detail["matched"] != 1 || o.Detail["resources"] != 4 {
		t.Fatalf("detail = %v", o.Detail)
	}
	if _, ok := o.Detail["firewalls"]; ok {
		t.Fatal("firewalls listed for a server binding")
	}

	// A firewall named by address, with a target id that does not say so.
	p = &Probe{show: fs.show}
	o = p.check(context.Background(), config{target: "gate", dir: "/infra", resource: "hcloud_firewall.main"})
	if o.Metrics["rules"] != 4 {
		t.Fatalf("rules = %v", o.Metrics)
	}
}

func TestCheckFailures(t *testing.T) {
	p := &Probe{show: (&fakeShow{err: exec.ErrNotFound}).show}
	o := p.check(context.Background(), config{target: "fw", dir: "/infra", binary: "terraform"})
	if o.Err == "" || !strings.Contains(o.Err, "not found") {
		t.Fatalf("err = %q", o.Err)
	}
	if p.Health().State != probe.HealthFailed {
		t.Fatalf("health = %+v", p.Health())
	}

	p = &Probe{show: (&fakeShow{docs: []string{`{"format_version":"1.0"}`}}).show}
	o = p.check(context.Background(), config{target: "fw", dir: "/infra"})
	if o.Err == "" || !strings.Contains(o.Err, "no terraform state") {
		t.Fatalf("err = %q", o.Err)
	}
	if p.Health().State != probe.HealthFailed {
		t.Fatalf("health = %+v", p.Health())
	}

	p = &Probe{show: (&fakeShow{err: errors.New("exit status 1: backend initialization required")}).show}
	o = p.check(context.Background(), config{target: "fw", dir: "/infra"})
	if o.Err == "" || p.Health().State != probe.HealthDegraded {
		t.Fatalf("err = %q health = %+v", o.Err, p.Health())
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	p := &Probe{}
	bad := []map[string]any{
		{},
		{"dir": filepath.Join(dir, "missing")},
		{"dir": dir, "port": 0},
		{"dir": dir, "port": "4317"},
		{"dir": dir, "watch": "yes"},
		{"dir": dir, "interval": "often"},
	}
	for _, spec := range bad {
		if err := p.Validate(spec); err == nil {
			t.Errorf("%v: expected error", spec)
		}
	}
	if err := p.Validate(map[string]any{"dir": dir, "port": 4317, "watch": false, "interval": "1m", "resource": "hcloud_firewall.main"}); err != nil {
		t.Fatal(err)
	}
}

func TestWatchFingerprint(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.tf", "resource {}")
	write(".terraform/providers/x.tf", "ignored")
	a := watchFingerprint(dir)
	write(".terraform/providers/x.tf", "still ignored, longer")
	if watchFingerprint(dir) != a {
		t.Fatal("changes under .terraform must not count")
	}
	write("README.md", "ignored too")
	if watchFingerprint(dir) != a {
		t.Fatal("non-tf files must not count")
	}
	write("main.tf", "resource { changed }")
	b := watchFingerprint(dir)
	if b == a {
		t.Fatal("changing main.tf did not change the fingerprint")
	}
	write("modules/lb/lb.tf", "x")
	if watchFingerprint(dir) == b {
		t.Fatal("a new nested .tf did not change the fingerprint")
	}
}

func TestStartStops(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeShow{docs: []string{sample}}
	p := &Probe{show: fs.show}
	out := make(chan probe.Observation, 8)
	ctx, cancel := context.WithCancel(context.Background())
	spec := map[string]any{"dir": dir, "port": 22, "_target": "fw->node-1", "_tick": 20 * time.Millisecond, "watch": false}
	if err := p.Start(ctx, spec, out); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-out:
		if o.Metrics["rules"] != 4 || o.Detail["port_allowed"] != true || len(o.Conditions) != 0 {
			t.Fatalf("observation = %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no observation")
	}
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("no re-emit on tick")
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	for len(out) > 0 {
		<-out
	}
	select {
	case o := <-out:
		t.Fatalf("observation after cancel: %+v", o)
	case <-time.After(100 * time.Millisecond):
	}
}
