package discover

import (
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/danilopopovikj/wassup/internal/model"
)

// imageTypes maps well known images to catalog types.
var imageTypes = []struct {
	match string
	typ   string
	label string
}{
	{"electricsql/electric", "sync", "Electric"},
	{"electric-sql/electric", "sync", "Electric"},
	{"hatchet-dev/hatchet-engine", "workload", "Hatchet engine"},
	{"hatchet-dev/hatchet-api", "workload", "Hatchet API"},
	{"hatchet-dev/hatchet-lite", "workload", "Hatchet"},
	{"hatchet-dev/hatchet-dashboard", "workload", "Hatchet dashboard"},
	{"rabbitmq", "queue", "RabbitMQ"},
	{"redis", "cache", "Redis"},
	{"valkey", "cache", "Valkey"},
	{"memcached", "cache", "Memcached"},
	{"postgres", "db", "Postgres"},
	{"cloudnative-pg/postgresql", "db", "Postgres"},
	{"mysql", "db", "MySQL"},
	{"mariadb", "db", "MariaDB"},
	{"signoz", "observability", "SigNoz"},
	{"otel/opentelemetry-collector", "observability", "OTel collector"},
	{"prom/prometheus", "observability", "Prometheus"},
	{"grafana/grafana", "observability", "Grafana"},
	{"grafana/loki", "observability", "Loki"},
	{"minio/minio", "storage", "MinIO"},
	{"nats", "queue", "NATS"},
	{"confluentinc/cp-kafka", "queue", "Kafka"},
	{"bitnami/kafka", "queue", "Kafka"},
	{"mher/flower", "observability", "Flower"},
	{"ingress-nginx/controller", "ingress", "Ingress"},
	{"traefik", "ingress", "Ingress"},
}

func typeForImage(image, def string) string {
	img := strings.ToLower(image)
	for _, it := range imageTypes {
		if strings.Contains(img, it.match) {
			return it.typ
		}
	}
	return def
}

func labelForImage(image string) string {
	img := strings.ToLower(image)
	for _, it := range imageTypes {
		if strings.Contains(img, it.match) {
			return it.label
		}
	}
	return ""
}

var celeryQueueRe = regexp.MustCompile(`(?:^|\s)(?:-Q|--queues)[=\s]+([A-Za-z0-9_,.-]+)`)

type k8sObj struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   map[string]any `yaml:"metadata"`
	Spec       map[string]any `yaml:"spec"`
}

func scanManifest(a *accumulator, path, rel string) {
	b, err := os.ReadFile(path)
	if err != nil || !looksLikeK8s(b) {
		return
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	docIndex := 0
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			break
		}
		docIndex++
		var obj k8sObj
		if err := node.Decode(&obj); err != nil || obj.Kind == "" {
			continue
		}
		line := node.Line
		if line == 0 {
			line = 1
		}
		manifestObject(a, rel, line, obj, "manifest")
	}
}

func looksLikeK8s(b []byte) bool {
	s := string(b)
	return strings.Contains(s, "apiVersion:") && strings.Contains(s, "kind:")
}

func str(m map[string]any, keys ...string) string {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = mm[k]
		if !ok {
			return ""
		}
	}
	switch v := cur.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func list(m map[string]any, keys ...string) []any {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = mm[k]
		if !ok {
			return nil
		}
	}
	l, _ := cur.([]any)
	return l
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func manifestObject(a *accumulator, rel string, line int, obj k8sObj, source string) {
	name := str(obj.Metadata, "name")
	ns := str(obj.Metadata, "namespace")
	ev := Evidence{Source: source, File: rel, Line: line, Note: obj.Kind + " " + name}
	switch obj.Kind {
	case "Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job", "Rollout":
		tmpl := asMap(asMap(obj.Spec["template"])["spec"])
		if obj.Kind == "CronJob" {
			tmpl = asMap(asMap(asMap(asMap(obj.Spec["jobTemplate"])["spec"])["template"])["spec"])
		}
		typ := "workload"
		if obj.Kind == "CronJob" || obj.Kind == "Job" {
			typ = "job"
		}
		c := Candidate{ID: model.SlugifyID(name), Type: typ, Label: labelFor(name), Namespace: ns, Kind: obj.Kind, Name: name, Env: map[string]string{}, Extra: map[string]string{}, Evidence: []Evidence{ev}}
		if sel := asMap(asMap(obj.Spec["selector"])["matchLabels"]); len(sel) > 0 {
			c.Selector = selectorFromLabels(sel)
		}
		if sched := str(obj.Spec, "schedule"); sched != "" {
			c.Extra["schedule"] = sched
		}
		var cmdAll []string
		for _, cv := range list(tmpl, "containers") {
			cont := asMap(cv)
			img := str(cont, "image")
			if c.Image == "" {
				c.Image = img
			}
			if t := typeForImage(img, ""); t != "" && typ == "workload" {
				c.Type = t
				if l := labelForImage(img); l != "" && c.Label == labelFor(name) {
					c.Label = l
				}
			}
			for _, ev := range list(cont, "env") {
				em := asMap(ev)
				n := str(em, "name")
				if n == "" {
					continue
				}
				v := str(em, "value")
				if v == "" {
					if s := str(em, "valueFrom", "secretKeyRef", "name"); s != "" {
						v = "secret:" + s + "/" + str(em, "valueFrom", "secretKeyRef", "key")
					} else if s := str(em, "valueFrom", "configMapKeyRef", "name"); s != "" {
						v = "configmap:" + s + "/" + str(em, "valueFrom", "configMapKeyRef", "key")
					}
				}
				c.Env[n] = v
			}
			var parts []string
			for _, x := range list(cont, "command") {
				parts = append(parts, str(map[string]any{"v": x}, "v"))
			}
			for _, x := range list(cont, "args") {
				parts = append(parts, str(map[string]any{"v": x}, "v"))
			}
			if len(parts) > 0 {
				cmdAll = append(cmdAll, strings.Join(parts, " "))
			}
		}
		if len(cmdAll) > 0 {
			c.Extra["command"] = strings.Join(cmdAll, " ; ")
		}
		added := a.add(c)
		if added == nil {
			return
		}
		envLinks(a, added, rel, line)
		commandLinks(a, added, cmdAll, rel, line)
	case "Service":
		sel := asMap(obj.Spec["selector"])
		// A Service names whatever the selector points at; the workload with
		// the same labels gets these addresses.
		target := str(sel, "app")
		if target == "" {
			target = str(sel, "app.kubernetes.io/name")
		}
		if target == "" {
			target = name
		}
		c := Candidate{ID: model.SlugifyID(target), Type: "custom", Label: labelFor(target), Namespace: ns, Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: "Service " + name + " selects " + target}}}
		c.Addresses = serviceAddresses(name, ns)
		if cur := a.add(c); cur != nil && cur.Type == "custom" {
			cur.Type = "workload"
		}
	case "Ingress":
		var hosts []string
		backends := map[string]bool{}
		for _, rv := range list(obj.Spec, "rules") {
			rm := asMap(rv)
			if h := str(rm, "host"); h != "" {
				hosts = append(hosts, h)
			}
			for _, pv := range list(rm, "http", "paths") {
				svc := str(asMap(pv), "backend", "service", "name")
				if svc != "" {
					backends[svc] = true
				}
			}
		}
		c := Candidate{ID: "ingress", Type: "ingress", Label: "Ingress", Namespace: ns, Name: name, Extra: map[string]string{"hosts": strings.Join(hosts, ",")}, Evidence: []Evidence{ev}}
		added := a.add(c)
		for svc := range backends {
			a.link(Link{From: added.ID, Host: svc + "." + ns, Kind: "http", Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: "Ingress " + name + " routes to Service " + svc}}})
		}
		for _, h := range hosts {
			dns := Candidate{ID: model.SlugifyID(h), Type: "dns", Label: h, Addresses: []string{h}, Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: "Ingress host"}}}
			a.add(dns)
			a.link(Link{From: dns.ID, To: added.ID, Kind: "tcp", Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: "host " + h + " served by Ingress " + name}}})
		}
	case "Cluster":
		if !strings.HasPrefix(obj.APIVersion, "postgresql.cnpg.io/") {
			return
		}
		instances := 1
		if v, err := strconv.Atoi(str(obj.Spec, "instances")); err == nil {
			instances = v
		}
		c := Candidate{ID: model.SlugifyID(name), Type: "db", Label: labelFor(name), Engine: "postgres", Namespace: ns, Name: name, Extra: map[string]string{"cnpg": "true", "instances": strconv.Itoa(instances)}, Evidence: []Evidence{ev}}
		roles := &model.Roles{Primary: model.SlugifyID(name) + "-primary"}
		for i := 1; i < instances; i++ {
			roles.Replicas = append(roles.Replicas, model.SlugifyID(name)+"-r"+strconv.Itoa(i))
		}
		c.Roles = roles
		// CNPG services: -rw (primary), -ro (replicas), -r (any)
		c.Addresses = append(c.Addresses, serviceAddresses(name+"-rw", ns)...)
		c.Addresses = append(c.Addresses, serviceAddresses(name+"-ro", ns)...)
		c.Addresses = append(c.Addresses, serviceAddresses(name+"-r", ns)...)
		c.Addresses = append(c.Addresses, serviceAddresses(name, ns)...)
		a.add(c)
	case "Certificate":
		a.note("cert-manager Certificate %s in %s: k8s.ingress reads its readiness", name, ns)
	}
}

func selectorFromLabels(m map[string]any) string {
	// prefer the conventional keys, keep it short
	for _, k := range []string{"app", "app.kubernetes.io/name", "component"} {
		if v := str(m, k); v != "" {
			return k + "=" + v
		}
	}
	var parts []string
	for k, v := range m {
		parts = append(parts, k+"="+str(map[string]any{"v": v}, "v"))
	}
	return strings.Join(parts, ",")
}

// envHints recognizes what an env var name implies even when its value is a secret.
var envHints = []struct {
	pattern string
	kind    string
	dstType string
	note    string
}{
	{"DATABASE_URL", "sql", "db", "database connection"},
	{"POSTGRES", "sql", "db", "database connection"},
	{"PG_DSN", "sql", "db", "database connection"},
	{"PGHOST", "sql", "db", "database host"},
	{"DB_HOST", "sql", "db", "database host"},
	{"REDIS", "cache", "cache", "redis connection"},
	{"CELERY_BROKER", "queue", "queue", "celery broker"},
	{"BROKER_URL", "queue", "queue", "broker"},
	{"RABBITMQ", "queue", "queue", "rabbitmq"},
	{"AMQP", "queue", "queue", "amqp"},
	{"HATCHET_CLIENT_HOST_PORT", "grpc", "workload", "hatchet engine"},
	{"HATCHET_CLIENT_TOKEN", "grpc", "workload", "hatchet token"},
	{"ELECTRIC_URL", "http", "sync", "electric shapes"},
	{"ELECTRIC_DATABASE_URL", "replication", "db", "electric source database"},
	{"SIGNOZ", "tcp", "observability", "traces"},
	{"OTEL_EXPORTER_OTLP_ENDPOINT", "tcp", "observability", "traces"},
	{"SENTRY_DSN", "external", "external", "error tracking"},
	{"STRIPE", "external", "external", "payments"},
	{"OPENAI", "external", "external", "llm"},
	{"ANTHROPIC", "external", "external", "llm"},
	{"GITHUB", "external", "external", "github"},
	{"S3_ENDPOINT", "tcp", "storage", "object storage"},
	{"AWS_S3", "tcp", "storage", "object storage"},
}

// envLinks derives links from a candidate's environment.
func envLinks(a *accumulator, c *Candidate, rel string, line int) {
	for name, val := range c.Env {
		upper := strings.ToUpper(name)
		// literal DSNs first: they name the host
		if hs := dsnRe.FindAllString(val, -1); len(hs) > 0 {
			for _, d := range hs {
				if h, ok := parseDSN(d); ok {
					l := Link{From: c.ID, Host: h.Host, Kind: edgeKindFor(h.Scheme, ""), Evidence: []Evidence{{Source: "manifest", File: rel, Line: line, Note: name + " in " + c.Kind + " " + c.Name}}}
					if l.Kind == "sql" && (upper == "ELECTRIC_DATABASE_URL" || c.Type == "sync") {
						// a sync engine reads the database's replication stream
						l.Kind, l.Reverse = "replication", true
					}
					a.link(l)
				}
			}
			continue
		}
		// hosts given directly (PGHOST=bookstore-db-rw)
		if strings.HasSuffix(upper, "_HOST") || upper == "PGHOST" {
			if val != "" && !strings.HasPrefix(val, "secret:") && !strings.HasPrefix(val, "configmap:") {
				kind := "tcp"
				for _, h := range envHints {
					if strings.Contains(upper, h.pattern) {
						kind = h.kind
						break
					}
				}
				a.link(Link{From: c.ID, Host: val, Kind: kind, Evidence: []Evidence{{Source: "manifest", File: rel, Line: line, Note: name + "=" + val}}})
			}
			continue
		}
		// secret-backed: the name still tells what it connects to
		for _, h := range envHints {
			if strings.Contains(upper, h.pattern) {
				c.Extra["needs:"+h.dstType] = strings.TrimSpace(c.Extra["needs:"+h.dstType] + " " + name)
				break
			}
		}
	}
	// Electric: the sync service's source database is a replication edge.
	if c.Type == "sync" {
		c.Extra["replication_slot"] = firstNonEmpty(c.Env["ELECTRIC_REPLICATION_SLOT"], "electric_slot_default")
	}
}

// commandLinks reads celery/hatchet/electric hints from container commands.
func commandLinks(a *accumulator, c *Candidate, cmds []string, rel string, line int) {
	for _, cmd := range cmds {
		lc := strings.ToLower(cmd)
		if strings.Contains(lc, "celery") && strings.Contains(lc, "worker") {
			c.Extra["celery_worker"] = "true"
			if m := celeryQueueRe.FindStringSubmatch(cmd); m != nil {
				for _, q := range strings.Split(m[1], ",") {
					q = strings.TrimSpace(q)
					if q == "" {
						continue
					}
					qc := Candidate{ID: model.SlugifyID(q + "-queue"), Type: "queue", Label: labelFor(q) + " queue", Namespace: c.Namespace, Name: q, Extra: map[string]string{"celery": "true", "queue": q}, Evidence: []Evidence{{Source: "manifest", File: rel, Line: line, Note: "celery worker consumes -Q " + q}}}
					a.add(qc)
					a.link(Link{From: qc.ID, To: c.ID, Kind: "queue", Evidence: []Evidence{{Source: "manifest", File: rel, Line: line, Note: c.Name + " consumes " + q}}})
				}
			} else {
				qc := Candidate{ID: "celery-queue", Type: "queue", Label: "Celery queue", Namespace: c.Namespace, Name: "celery", Extra: map[string]string{"celery": "true", "queue": "celery"}, Evidence: []Evidence{{Source: "manifest", File: rel, Line: line, Note: "celery worker with the default queue"}}}
				a.add(qc)
				a.link(Link{From: qc.ID, To: c.ID, Kind: "queue", Evidence: []Evidence{{Source: "manifest", File: rel, Line: line, Note: c.Name + " consumes the default queue"}}})
			}
		}
		if strings.Contains(lc, "celery") && strings.Contains(lc, "beat") {
			c.Extra["celery_beat"] = "true"
		}
		if strings.Contains(lc, "flower") {
			c.Extra["flower"] = "true"
		}
	}
	if strings.Contains(strings.ToLower(c.Image), "hatchet") || c.Env["HATCHET_CLIENT_TOKEN"] != "" && c.Type == "workload" && strings.Contains(strings.ToLower(c.Name), "worker") {
		c.Extra["hatchet_worker"] = "true"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// scanHelmValues reads values.yaml files: images and env-like settings.
func scanHelmValues(a *accumulator, path, rel string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var v map[string]any
	if err := yaml.Unmarshal(b, &v); err != nil || v == nil {
		return
	}
	name := chartNameFrom(rel)
	img := str(v, "image", "repository")
	if img == "" {
		img = str(v, "image")
	}
	c := Candidate{ID: model.SlugifyID(name), Type: "workload", Label: labelFor(name), Name: name, Env: map[string]string{}, Extra: map[string]string{"helm": rel}, Evidence: []Evidence{{Source: "helm", File: rel, Line: 1, Note: "values for " + name}}}
	if img != "" {
		c.Image = img
		if t := typeForImage(img, ""); t != "" {
			c.Type = t
			if l := labelForImage(img); l != "" {
				c.Label = l
			}
		}
	}
	// env: {KEY: value} or env: [{name, value}]
	switch e := v["env"].(type) {
	case map[string]any:
		for k, val := range e {
			c.Env[k] = str(map[string]any{"v": val}, "v")
		}
	case []any:
		for _, item := range e {
			m := asMap(item)
			if n := str(m, "name"); n != "" {
				c.Env[n] = str(m, "value")
			}
		}
	}
	if len(c.Env) == 0 && img == "" {
		return
	}
	added := a.add(c)
	if added != nil {
		envLinks(a, added, rel, 1)
	}
}

func chartNameFrom(rel string) string {
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return strings.TrimSuffix(parts[0], ".yaml")
}
