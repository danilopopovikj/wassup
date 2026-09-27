// Package terraform implements terraform.state, the probe that reads a
// Terraform state through `terraform show -json`, turns resource changes
// into terraform events and answers firewall questions for hcloud.
package terraform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// firewallType is the Hetzner Cloud firewall resource type.
const firewallType = "hcloud_firewall"

// Resource is one entry of the state inventory.
type Resource struct {
	Address  string         `json:"address"`
	Type     string         `json:"type"`
	Name     string         `json:"name"`
	Provider string         `json:"provider"`
	Module   string         `json:"module,omitempty"`
	Values   map[string]any `json:"-"`
	// Fingerprint is a hash of Values, used to detect changed resources.
	Fingerprint string `json:"-"`
}

// State is the parsed inventory of a `terraform show -json` document.
type State struct {
	TerraformVersion string
	Resources        []Resource
}

// ErrNoState is returned when the document has no values: the directory has
// never been applied or the backend is not initialised.
var ErrNoState = errors.New("no terraform state (nothing applied yet or backend not initialised)")

// showDoc is the subset of the `terraform show -json` schema the probe reads.
type showDoc struct {
	FormatVersion    string `json:"format_version"`
	TerraformVersion string `json:"terraform_version"`
	Values           *struct {
		RootModule showModule `json:"root_module"`
	} `json:"values"`
}

type showModule struct {
	Address      string         `json:"address"`
	Resources    []showResource `json:"resources"`
	ChildModules []showModule   `json:"child_modules"`
}

type showResource struct {
	Address      string         `json:"address"`
	Mode         string         `json:"mode"`
	Type         string         `json:"type"`
	Name         string         `json:"name"`
	ProviderName string         `json:"provider_name"`
	Values       map[string]any `json:"values"`
}

// ParseShow parses the JSON printed by `terraform show -json` into a flat
// resource inventory, walking child modules recursively. Data sources are
// skipped: they are reads, not infrastructure.
func ParseShow(data []byte) (*State, error) {
	var doc showDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("terraform show -json: %w", err)
	}
	if doc.Values == nil {
		return nil, ErrNoState
	}
	st := &State{TerraformVersion: doc.TerraformVersion}
	walk(&doc.Values.RootModule, st)
	sort.Slice(st.Resources, func(i, j int) bool { return st.Resources[i].Address < st.Resources[j].Address })
	return st, nil
}

func walk(m *showModule, st *State) {
	for _, r := range m.Resources {
		if r.Mode == "data" {
			continue
		}
		st.Resources = append(st.Resources, Resource{
			Address:     r.Address,
			Type:        r.Type,
			Name:        r.Name,
			Provider:    r.ProviderName,
			Module:      m.Address,
			Values:      r.Values,
			Fingerprint: fingerprint(r.Values),
		})
	}
	for i := range m.ChildModules {
		walk(&m.ChildModules[i], st)
	}
}

// fingerprint hashes a values map; json.Marshal sorts map keys so equal
// values hash equally.
func fingerprint(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Filter returns the resources whose address is addr or lies under it
// (addr as an exact address, an indexed instance prefix or a module path).
// An empty filter keeps everything.
func (s *State) Filter(addr string) []Resource {
	if addr == "" {
		return s.Resources
	}
	var out []Resource
	for _, r := range s.Resources {
		if r.Address == addr || strings.HasPrefix(r.Address, addr+".") || strings.HasPrefix(r.Address, addr+"[") {
			out = append(out, r)
		}
	}
	return out
}

// Diff compares two inventories and returns added, removed and changed
// addresses, each sorted.
func Diff(prev, cur []Resource) (added, removed, changed []string) {
	old := make(map[string]Resource, len(prev))
	for _, r := range prev {
		old[r.Address] = r
	}
	seen := make(map[string]bool, len(cur))
	for _, r := range cur {
		seen[r.Address] = true
		p, ok := old[r.Address]
		switch {
		case !ok:
			added = append(added, r.Address)
		case p.Fingerprint != r.Fingerprint:
			changed = append(changed, r.Address)
		}
	}
	for addr := range old {
		if !seen[addr] {
			removed = append(removed, addr)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}

// Rule is one hcloud_firewall rule block.
type Rule struct {
	Direction      string   `json:"direction"`
	Protocol       string   `json:"protocol"`
	Port           string   `json:"port,omitempty"`
	SourceIPs      []string `json:"source_ips,omitempty"`
	DestinationIPs []string `json:"destination_ips,omitempty"`
	Description    string   `json:"description,omitempty"`
}

// Firewall is an hcloud_firewall resource with its rules.
type Firewall struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	Rules   []Rule `json:"rules"`
}

// Firewalls extracts the hcloud_firewall resources of an inventory.
func Firewalls(resources []Resource) []Firewall {
	var out []Firewall
	for _, r := range resources {
		if r.Type != firewallType {
			continue
		}
		fw := Firewall{Address: r.Address, Name: r.Name}
		if n, ok := r.Values["name"].(string); ok && n != "" {
			fw.Name = n
		}
		if rules, ok := r.Values["rule"].([]any); ok {
			for _, raw := range rules {
				m, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				fw.Rules = append(fw.Rules, Rule{
					Direction:      str(m["direction"]),
					Protocol:       str(m["protocol"]),
					Port:           str(m["port"]),
					SourceIPs:      strs(m["source_ips"]),
					DestinationIPs: strs(m["destination_ips"]),
					Description:    str(m["description"]),
				})
			}
		}
		out = append(out, fw)
	}
	return out
}

// RuleCount sums the rules of the given firewalls.
func RuleCount(fws []Firewall) int {
	n := 0
	for _, fw := range fws {
		n += len(fw.Rules)
	}
	return n
}

// AllowsInboundTCP reports whether the rule lets inbound TCP traffic reach
// port. Hetzner firewalls are allow-lists, so any matching rule allows.
func (r Rule) AllowsInboundTCP(port int) bool {
	if !strings.EqualFold(r.Direction, "in") {
		return false
	}
	if !strings.EqualFold(r.Protocol, "tcp") {
		return false
	}
	return PortMatches(r.Port, port)
}

// PortMatches reports whether an hcloud port spec ("80", "80-85", "any")
// covers port.
func PortMatches(spec string, port int) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "any") {
		return true
	}
	if lo, hi, ok := strings.Cut(spec, "-"); ok {
		l, err1 := strconv.Atoi(strings.TrimSpace(lo))
		h, err2 := strconv.Atoi(strings.TrimSpace(hi))
		return err1 == nil && err2 == nil && port >= l && port <= h
	}
	n, err := strconv.Atoi(spec)
	return err == nil && n == port
}

// AllowsInboundTCP reports whether any firewall allows inbound TCP to port.
// The second result names the firewalls that were checked and deny it, for
// the FirewallDenied detail. With no firewalls there is nothing to deny and
// the traffic is reported as allowed.
func AllowsInboundTCP(fws []Firewall, port int) (bool, []Firewall) {
	if len(fws) == 0 {
		return true, nil
	}
	var denying []Firewall
	for _, fw := range fws {
		allowed := false
		for _, r := range fw.Rules {
			if r.AllowsInboundTCP(port) {
				allowed = true
				break
			}
		}
		if allowed {
			return true, nil
		}
		denying = append(denying, fw)
	}
	return false, denying
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
