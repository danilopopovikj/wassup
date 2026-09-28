package discover

import (
	"bufio"
	"os"
	"regexp"
	"strings"

	"github.com/danilopopovikj/wassup/internal/model"
)

// A light HCL reader: resource blocks, their string attributes and the
// references they make to other resources. It does not evaluate anything;
// it collects evidence for a human (or Claude Code) to verify.

var (
	resourceRe = regexp.MustCompile(`^\s*resource\s+"([a-zA-Z0-9_]+)"\s+"([a-zA-Z0-9_-]+)"\s*\{`)
	moduleRe   = regexp.MustCompile(`^\s*module\s+"([a-zA-Z0-9_-]+)"\s*\{`)
	providerRe = regexp.MustCompile(`^\s*provider\s+"([a-zA-Z0-9_-]+)"\s*\{`)
	attrRe     = regexp.MustCompile(`^\s*([a-zA-Z0-9_]+)\s*=\s*"([^"]*)"`)
	refRe      = regexp.MustCompile(`\b([a-z][a-z0-9_]*_[a-z0-9_]+)\.([a-zA-Z0-9_-]+)\b`)
	setNameRe  = regexp.MustCompile(`^\s*name\s*=\s*"([^"]+)"`)
	setValueRe = regexp.MustCompile(`^\s*value\s*=\s*"([^"]*)"`)
)

type tfResource struct {
	Type, Name string
	File       string
	Line       int
	Attrs      map[string]string
	Refs       []string // "type.name"
	Body       []string
}

// tfTypeMap maps terraform resource types to catalog types.
var tfTypeMap = map[string]string{
	"hcloud_load_balancer": "loadbalancer", "aws_lb": "loadbalancer", "aws_alb": "loadbalancer", "aws_elb": "loadbalancer", "google_compute_forwarding_rule": "loadbalancer", "google_compute_global_forwarding_rule": "loadbalancer", "digitalocean_loadbalancer": "loadbalancer", "azurerm_lb": "loadbalancer",
	"hcloud_firewall": "firewall", "aws_security_group": "firewall", "google_compute_firewall": "firewall", "digitalocean_firewall": "firewall", "azurerm_network_security_group": "firewall",
	"hcloud_server": "node", "aws_instance": "node", "google_compute_instance": "node", "digitalocean_droplet": "node", "azurerm_linux_virtual_machine": "node",
	"hcloud_volume": "storage", "aws_ebs_volume": "storage", "aws_s3_bucket": "storage", "google_storage_bucket": "storage", "digitalocean_spaces_bucket": "storage", "azurerm_storage_account": "storage", "aws_efs_file_system": "storage",
	"aws_db_instance": "database", "aws_rds_cluster": "database", "google_sql_database_instance": "database", "digitalocean_database_cluster": "database", "azurerm_postgresql_flexible_server": "database", "postgresql_database": "database",
	"aws_elasticache_cluster": "cache", "aws_elasticache_replication_group": "cache", "google_redis_instance": "cache", "azurerm_redis_cache": "cache",
	"aws_sqs_queue": "queue", "google_pubsub_topic": "queue", "aws_mq_broker": "queue",
	"cloudflare_record": "dns", "cloudflare_dns_record": "dns", "aws_route53_record": "dns", "google_dns_record_set": "dns", "digitalocean_record": "dns", "hcloud_rdns": "dns", "dns_a_record_set": "dns",
}

// helmChartMap maps helm chart names to catalog types.
var helmChartMap = map[string]string{
	"electric": "sync", "electric-sql": "sync", "electricsql": "sync",
	"hatchet": "workload", "hatchet-stack": "workload",
	"redis": "cache", "valkey": "cache", "redis-cluster": "cache",
	"rabbitmq": "queue", "nats": "queue", "kafka": "queue",
	"signoz": "observability", "kube-prometheus-stack": "observability", "prometheus": "observability", "grafana": "observability", "loki": "observability", "opentelemetry-collector": "observability",
	"postgresql": "database", "postgresql-ha": "database", "cloudnative-pg": "custom", "cnpg": "custom",
	"ingress-nginx": "ingress", "traefik": "ingress", "cert-manager": "custom",
}

func scanTerraform(a *accumulator, path, rel string) {
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	var resources []*tfResource
	var cur *tfResource
	depth := 0
	line := 0
	inModule := false
	for sc.Scan() {
		line++
		text := sc.Text()
		if m := providerRe.FindStringSubmatch(text); m != nil {
			a.provider(m[1])
		}
		if cur == nil {
			if m := resourceRe.FindStringSubmatch(text); m != nil {
				cur = &tfResource{Type: m[1], Name: m[2], File: rel, Line: line, Attrs: map[string]string{}}
				depth = strings.Count(text, "{") - strings.Count(text, "}")
				if depth <= 0 {
					resources = append(resources, cur)
					cur = nil
				}
				continue
			}
			if m := moduleRe.FindStringSubmatch(text); m != nil {
				inModule = true
				depth = strings.Count(text, "{") - strings.Count(text, "}")
				cur = &tfResource{Type: "module", Name: m[1], File: rel, Line: line, Attrs: map[string]string{}}
				continue
			}
			continue
		}
		cur.Body = append(cur.Body, text)
		if m := attrRe.FindStringSubmatch(text); m != nil {
			if _, ok := cur.Attrs[m[1]]; !ok {
				cur.Attrs[m[1]] = m[2]
			}
		}
		for _, m := range refRe.FindAllStringSubmatch(text, -1) {
			ref := m[1] + "." + m[2]
			if m[1] != cur.Type || m[2] != cur.Name {
				if _, known := tfTypeMap[m[1]]; known || strings.HasPrefix(m[1], "hcloud_") || strings.HasPrefix(m[1], "aws_") || strings.HasPrefix(m[1], "google_") || strings.HasPrefix(m[1], "helm_") || strings.HasPrefix(m[1], "kubernetes_") || strings.HasPrefix(m[1], "postgresql_") {
					cur.Refs = append(cur.Refs, ref)
				}
			}
		}
		depth += strings.Count(text, "{") - strings.Count(text, "}")
		if depth <= 0 {
			if !inModule {
				resources = append(resources, cur)
			} else {
				resources = append(resources, cur) // modules carry sources worth noting
			}
			cur = nil
			inModule = false
		}
	}
	a.tf = append(a.tf, resources...)
}

// emitTerraform turns collected resources into candidates and links. It
// runs once all files are read, so a load balancer target in one file can
// point at a server declared in another.
func emitTerraform(a *accumulator) {
	addr := map[string]string{} // "type.name" -> candidate id
	for _, r := range a.tf {
		if _, ok := tfTypeMap[r.Type]; ok {
			name := r.Attrs["name"]
			if name == "" {
				name = r.Name
			}
			addr[r.Type+"."+r.Name] = model.SlugifyID(name)
		}
	}
	for _, r := range a.tf {
		terraformResource(a, r, addr)
	}
	for _, r := range a.tf {
		terraformAttachment(a, r, addr)
	}
}

// attachment resource types connect two other resources.
var attachmentTypes = map[string]bool{
	"hcloud_load_balancer_target": true, "hcloud_firewall_attachment": true, "hcloud_load_balancer_service": false,
	"aws_lb_target_group_attachment": true, "aws_network_interface_sg_attachment": true, "google_compute_instance_group": true,
}

func terraformAttachment(a *accumulator, r *tfResource, addr map[string]string) {
	if !attachmentTypes[r.Type] {
		return
	}
	var lbs, fws, nodes []string
	for _, ref := range r.Refs {
		t, _, _ := strings.Cut(ref, ".")
		id, ok := addr[ref]
		if !ok {
			continue
		}
		switch tfTypeMap[t] {
		case "loadbalancer":
			lbs = append(lbs, id)
		case "firewall":
			fws = append(fws, id)
		case "node":
			nodes = append(nodes, id)
		}
	}
	ev := Evidence{Source: "terraform", File: r.File, Line: r.Line, Note: r.Type + "." + r.Name}
	for _, lb := range lbs {
		for _, n := range nodes {
			a.link(Link{From: lb, To: n, Kind: "tcp", Evidence: []Evidence{ev}})
		}
	}
	for _, fw := range fws {
		if c := a.f.candidate(fw); c != nil {
			if c.Extra == nil {
				c.Extra = map[string]string{}
			}
			c.Extra["applies_to"] = strings.TrimSpace(c.Extra["applies_to"] + " " + strings.Join(nodes, " "))
			c.Evidence = append(c.Evidence, ev)
		}
	}
}

func tfProviderOf(t string) string {
	if i := strings.Index(t, "_"); i > 0 {
		return t[:i]
	}
	return t
}

var providerLabel = map[string]string{"hcloud": "Hetzner", "aws": "AWS", "google": "Google Cloud", "digitalocean": "DigitalOcean", "azurerm": "Azure", "cloudflare": "Cloudflare"}

func terraformResource(a *accumulator, r *tfResource, addr map[string]string) {
	ev := Evidence{Source: "terraform", File: r.File, Line: r.Line, Note: r.Type + "." + r.Name}
	name := r.Attrs["name"]
	if name == "" {
		name = r.Name
	}
	switch {
	case r.Type == "helm_release":
		chart := strings.ToLower(r.Attrs["chart"])
		if i := strings.LastIndex(chart, "/"); i >= 0 {
			chart = chart[i+1:]
		}
		typ, ok := helmChartMap[chart]
		if !ok {
			typ = "workload"
		}
		if typ == "custom" {
			a.note("terraform %s installs %s (an operator, not drawn)", r.Name, chart)
			return
		}
		ns := r.Attrs["namespace"]
		c := Candidate{ID: model.SlugifyID(name), Type: typ, Label: labelFor(name), Namespace: ns, Name: name, Evidence: []Evidence{ev}, Env: map[string]string{}, Extra: map[string]string{"chart": chart}}
		c.Addresses = serviceAddresses(name, ns)
		// set { name = "X" value = "Y" } pairs carry env-like config
		var pendingName string
		for i, l := range r.Body {
			if m := setNameRe.FindStringSubmatch(l); m != nil {
				pendingName = m[1]
				continue
			}
			if m := setValueRe.FindStringSubmatch(l); m != nil && pendingName != "" {
				c.Env[pendingName] = m[1]
				for _, d := range dsnRe.FindAllString(m[1], -1) {
					if h, ok := parseDSN(d); ok {
						l := Link{From: c.ID, Host: h.Host, Kind: edgeKindFor(h.Scheme, ""), Evidence: []Evidence{{Source: "terraform", File: r.File, Line: r.Line + i + 1, Note: pendingName + " in helm_release " + r.Name}}}
						if typ == "sync" && l.Kind == "sql" {
							l.Kind, l.Reverse = "replication", true
						}
						a.link(l)
					}
				}
				pendingName = ""
			}
		}
		a.add(c)
		return
	case r.Type == "module":
		if src := r.Attrs["source"]; src != "" {
			a.note("terraform module %s from %s: its resources are not read; check it by hand", r.Name, src)
		}
		return
	case strings.HasPrefix(r.Type, "kubernetes_deployment") || strings.HasPrefix(r.Type, "kubernetes_stateful_set") || strings.HasPrefix(r.Type, "kubernetes_daemon_set"):
		c := Candidate{ID: model.SlugifyID(name), Type: "workload", Label: labelFor(name), Name: name, Evidence: []Evidence{ev}}
		if ns := r.Attrs["namespace"]; ns != "" {
			c.Namespace = ns
		}
		if img := r.Attrs["image"]; img != "" {
			c.Image = img
			c.Type = typeForImage(img, c.Type)
		}
		a.add(c)
		return
	case r.Type == "postgresql_replication_slot" || r.Type == "postgresql_publication":
		a.note("terraform %s.%s declares logical replication (%s); bind pg.stats with replica: %s on the consumer and its edge", r.Type, r.Name, name, name)
		return
	}
	typ, ok := tfTypeMap[r.Type]
	if !ok {
		return
	}
	a.provider(tfProviderOf(r.Type))
	c := Candidate{ID: model.SlugifyID(name), Type: typ, Label: labelFor(name), Name: name, Evidence: []Evidence{ev}, Extra: map[string]string{"terraform": r.Type + "." + r.Name}}
	if typ != "node" && typ != "storage" && typ != "dns" {
		c.Group = groupIDFor(tfProviderOf(r.Type))
	} else {
		c.Group = groupIDFor(tfProviderOf(r.Type))
	}
	switch typ {
	case "database":
		c.Engine = engineOf(r.Attrs)
		if src := r.Attrs["replicate_source_db"]; src != "" {
			c.Extra["replica_of"] = src
		}
		if host := r.Attrs["address"]; host != "" {
			c.Addresses = append(c.Addresses, host)
		}
	case "dns":
		host := r.Attrs["name"]
		if zone := r.Attrs["zone_name"]; zone != "" && !strings.HasSuffix(host, zone) {
			host = host + "." + zone
		}
		if host != "" {
			c.ID = model.SlugifyID(host)
			c.Label = host
			c.Addresses = append(c.Addresses, host)
		}
	case "loadbalancer":
		if r.Attrs["location"] != "" {
			c.Extra["location"] = r.Attrs["location"]
		}
	}
	added := a.add(c)
	if added == nil {
		return
	}
	// Relations from references.
	for _, ref := range r.Refs {
		t, n, _ := strings.Cut(ref, ".")
		refType := tfTypeMap[t]
		refID, ok := addr[ref]
		if !ok {
			refID = model.SlugifyID(n)
		}
		switch {
		case typ == "loadbalancer" && refType == "node", t == "hcloud_load_balancer_target":
			a.link(Link{From: added.ID, To: refID, Kind: "tcp", Evidence: []Evidence{{Source: "terraform", File: r.File, Line: r.Line, Note: r.Type + "." + r.Name + " references " + ref}}})
		case typ == "firewall" && refType == "node":
			added.Extra["applies_to"] = strings.TrimSpace(added.Extra["applies_to"] + " " + refID)
		case typ == "dns" && refType == "loadbalancer":
			a.link(Link{From: added.ID, To: refID, Kind: "tcp", Evidence: []Evidence{{Source: "terraform", File: r.File, Line: r.Line, Note: "record points at " + ref}}})
		case typ == "database" && refType == "database":
			added.Extra["replica_of"] = refID
		}
	}
}

func engineOf(attrs map[string]string) string {
	for _, k := range []string{"engine", "database_version", "db_engine"} {
		v := strings.ToLower(attrs[k])
		switch {
		case strings.HasPrefix(v, "postgres"), strings.HasPrefix(v, "pg"), strings.HasPrefix(v, "aurora-postgresql"):
			return "postgres"
		case strings.HasPrefix(v, "mysql"), strings.HasPrefix(v, "aurora-mysql"), strings.HasPrefix(v, "mariadb"):
			return "mysql"
		case strings.HasPrefix(v, "redis"):
			return "redis"
		}
	}
	return ""
}

func groupIDFor(provider string) string {
	if _, ok := providerLabel[provider]; ok {
		return provider
	}
	return ""
}

// labelFor turns "bookstore-lb" into "Bookstore lb" only when the source
// name is a slug; otherwise it keeps the name.
func labelFor(name string) string {
	if name == "" {
		return ""
	}
	if strings.ContainsAny(name, " ") {
		return name
	}
	s := strings.ReplaceAll(strings.ReplaceAll(name, "-", " "), "_", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}
