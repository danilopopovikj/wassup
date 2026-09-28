package discover

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/danilopopovikj/wassup/internal/discover/redact"
	"github.com/danilopopovikj/wassup/internal/model"
)

// imageTypes maps well known images to catalog types.
var imageTypes = []struct {
	match string
	typ   string
	label string
}{
	{"electricsql/electric", "syncengine", "Electric"},
	{"electric-sql/electric", "syncengine", "Electric"},
	{"hatchet-dev/hatchet-engine", "workload", "Hatchet engine"},
	{"hatchet-dev/hatchet-api", "workload", "Hatchet API"},
	{"hatchet-dev/hatchet-lite", "workload", "Hatchet"},
	{"hatchet-dev/hatchet-dashboard", "workload", "Hatchet dashboard"},
	{"rabbitmq", "queue", "RabbitMQ"},
	{"redis", "cache", "Redis"},
	{"valkey", "cache", "Valkey"},
	{"memcached", "cache", "Memcached"},
	{"postgres", "database", "Postgres"},
	{"cloudnative-pg/postgresql", "database", "Postgres"},
	{"mysql", "database", "MySQL"},
	{"mariadb", "database", "MariaDB"},
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
	Data       map[string]any `yaml:"data"` // ConfigMap
}

// envEntry is one variable of a container, as the manifest states it.
type envEntry struct {
	name, value string
	secret      string // "<secret>/<key>"
	configMap   string // name of the ConfigMap the value comes from
	key         string // key in that ConfigMap
}

// envSource is one envFrom entry.
type envSource struct {
	configMap, secret, prefix string
}

// pendingContainer is a container whose environment is not resolved yet.
type pendingContainer struct {
	env  []envEntry
	from []envSource
	args []string // command and args
}

// pendingWorkload is a pod template read from a manifest. Its ConfigMaps
// may live in a file that is read later, so it waits for the end.
type pendingWorkload struct {
	id, ns     string
	rel        string
	line       int
	containers []pendingContainer
}

// pendingService is a Service waiting for the workloads it may select.
type pendingService struct {
	name, ns string
	selector map[string]string
	ev       Evidence
}

func scanManifest(a *accumulator, path, rel string) {
	b, err := os.ReadFile(path)
	if err != nil || !looksLikeK8s(b) {
		return
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			break
		}
		var obj k8sObj
		if err := node.Decode(&obj); err != nil || obj.Kind == "" {
			continue
		}
		line := node.Line
		if line == 0 {
			line = 1
		}
		if policyKinds[obj.Kind] {
			scanPolicy(a, rel, line, &node)
			continue
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
	return scalar(cur)
}

// scalar renders a YAML scalar as text.
func scalar(v any) string {
	switch v := v.(type) {
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

// stringMap flattens a YAML mapping of scalars.
func stringMap(v any) map[string]string {
	m := asMap(v)
	if len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, x := range m {
		out[k] = scalar(x)
	}
	return out
}

// sortedKeys returns the keys of a map in order, so that a scan reads the
// same way every time.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// namespaceOf returns the namespace of a manifest: its own, or the one the
// nearest kustomization above it sets.
func (a *accumulator) namespaceOf(rel, own string) string {
	if own != "" {
		if hasPlaceholder(own) {
			return ""
		}
		return own
	}
	for dir := filepath.Dir(filepath.Join(a.root, rel)); ; dir = filepath.Dir(dir) {
		if ns, ok := a.kustomizeNS[dir]; ok {
			return ns
		}
		if dir == a.root || filepath.Dir(dir) == dir {
			return ""
		}
	}
}

func manifestObject(a *accumulator, rel string, line int, obj k8sObj, source string) {
	name := str(obj.Metadata, "name")
	if name == "" {
		return
	}
	if hasPlaceholder(name) {
		a.note("%s:%d: the %s %q has a placeholder for a name and was left out", rel, line, obj.Kind, name)
		return
	}
	ns := a.namespaceOf(rel, str(obj.Metadata, "namespace"))
	ev := Evidence{Source: source, File: rel, Line: line, Note: obj.Kind + " " + name}
	switch obj.Kind {
	case "ConfigMap":
		a.addConfigMap(ns, name, stringMap(obj.Data))
	case "Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job", "Rollout":
		podTemplate := asMap(obj.Spec["template"])
		if obj.Kind == "CronJob" {
			podTemplate = asMap(asMap(asMap(obj.Spec["jobTemplate"])["spec"])["template"])
		}
		tmpl := asMap(podTemplate["spec"])
		typ := "workload"
		if obj.Kind == "CronJob" || obj.Kind == "Job" {
			typ = "scheduledjob"
		}
		c := Candidate{ID: model.SlugifyID(name), Type: typ, Label: labelFor(name), Namespace: ns, Kind: obj.Kind, Name: name, Env: map[string]string{}, Extra: map[string]string{}, Evidence: []Evidence{ev}}
		sel := asMap(asMap(obj.Spec["selector"])["matchLabels"])
		if len(sel) > 0 {
			c.Selector = selectorFromLabels(sel)
		}
		// The pod's labels say what a Service selects; the object's labels
		// say which release and product it belongs to.
		c.Labels = mergeMap(mergeMap(stringMap(asMap(podTemplate["metadata"])["labels"]), stringMap(sel)), stringMap(obj.Metadata["labels"]))
		if rel := str(obj.Metadata, "annotations", "meta.helm.sh/release-name"); rel != "" {
			c.Extra["release"] = rel
		}
		if sched := str(obj.Spec, "schedule"); sched != "" {
			c.Extra["schedule"] = sched
		}
		placement(&c, tmpl)
		pw := pendingWorkload{id: c.ID, ns: ns, rel: rel, line: line}
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
			var pc pendingContainer
			for _, fv := range list(cont, "envFrom") {
				fm := asMap(fv)
				pc.from = append(pc.from, envSource{configMap: str(fm, "configMapRef", "name"), secret: str(fm, "secretRef", "name"), prefix: str(fm, "prefix")})
			}
			for _, ev := range list(cont, "env") {
				em := asMap(ev)
				n := str(em, "name")
				if n == "" {
					continue
				}
				e := envEntry{name: n, value: str(em, "value")}
				if s := str(em, "valueFrom", "secretKeyRef", "name"); s != "" {
					e.secret = s + "/" + str(em, "valueFrom", "secretKeyRef", "key")
				} else if s := str(em, "valueFrom", "configMapKeyRef", "name"); s != "" {
					e.configMap, e.key = s, str(em, "valueFrom", "configMapKeyRef", "key")
				}
				pc.env = append(pc.env, e)
			}
			for _, x := range list(cont, "command") {
				pc.args = append(pc.args, scalar(x))
			}
			for _, x := range list(cont, "args") {
				pc.args = append(pc.args, scalar(x))
			}
			pw.containers = append(pw.containers, pc)
		}
		if a.add(c) != nil {
			a.workloads = append(a.workloads, pw)
		}
	case "Service":
		a.services = append(a.services, pendingService{name: name, ns: ns, selector: stringMap(obj.Spec["selector"]),
			ev: Evidence{Source: source, File: rel, Line: line, Note: "Service " + name}})
	case "Ingress":
		// An Ingress is the ingress routing to the workloads behind its
		// Services: one flow per Service, however many paths lead to it.
		var hosts []string
		backends := map[string]bool{}
		if svc := str(obj.Spec, "defaultBackend", "service", "name"); svc != "" {
			backends[svc] = true
		}
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
		addIngress(a, ingressObject{name: name, ns: ns, hosts: hosts, services: sortedKeys(backends), ev: ev})
	case "Cluster":
		if !strings.HasPrefix(obj.APIVersion, "postgresql.cnpg.io/") {
			return
		}
		instances := 1
		if v, err := strconv.Atoi(str(obj.Spec, "instances")); err == nil {
			instances = v
		}
		c := Candidate{ID: model.SlugifyID(name), Type: "database", Label: labelFor(name), Engine: "postgres", Namespace: ns, Name: name, Extra: map[string]string{"cnpg": "true", "instances": strconv.Itoa(instances)}, Evidence: []Evidence{ev}}
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

// ingressObject is one Ingress, from a manifest or from the cluster.
type ingressObject struct {
	name, ns string
	hosts    []string
	services []string
	ev       Evidence
}

// ingressID is the id of the component every Ingress object belongs to.
const ingressID = "ingress"

// addIngress records an Ingress object on the ingress component, with its
// own namespace and name so that each gets its own probe, and one flow per
// Service it routes to.
func addIngress(a *accumulator, ing ingressObject) {
	var hosts []string
	for _, h := range ing.hosts {
		if !usableHost(strings.TrimPrefix(h, "*.")) {
			a.note("Ingress %s: the host %q is a placeholder or a local name and was left out", ing.name, h)
			continue
		}
		hosts = append(hosts, strings.ToLower(h))
	}
	c := Candidate{ID: ingressID, Type: "ingress", Label: "Ingress", Objects: []string{objectRef("Ingress", ing.ns, ing.name)}, Extra: map[string]string{}, Evidence: []Evidence{ing.ev}}
	added := a.add(c)
	if added == nil {
		return
	}
	added.Extra["hosts"] = strings.Join(uniq(append(strings.Split(added.Extra["hosts"], ","), hosts...)), ",")
	id := added.ID
	for _, svc := range ing.services {
		host := svc
		if ing.ns != "" {
			host = svc + "." + ing.ns
		}
		a.link(Link{From: id, Host: host, Kind: "http", Internal: true, Evidence: []Evidence{{Source: ing.ev.Source, File: ing.ev.File, Line: ing.ev.Line, Note: "Ingress " + ing.name + " routes to Service " + svc}}})
	}
	for _, h := range hosts {
		if strings.HasPrefix(h, "*.") {
			continue // a wildcard is not a record to resolve
		}
		dns := Candidate{ID: model.SlugifyID(h), Type: "dns", Label: h, Addresses: []string{h}, Evidence: []Evidence{{Source: ing.ev.Source, File: ing.ev.File, Line: ing.ev.Line, Note: "Ingress host"}}}
		a.add(dns)
		a.link(Link{From: dns.ID, To: id, Kind: "tcp", Evidence: []Evidence{{Source: ing.ev.Source, File: ing.ev.File, Line: ing.ev.Line, Note: "host " + h + " served by Ingress " + ing.name}}})
	}
}

// objectRef writes "Kind namespace/name".
func objectRef(kind, ns, name string) string {
	return kind + " " + ns + "/" + name
}

// parseObjectRef reads what objectRef wrote.
func parseObjectRef(s string) (kind, ns, name string) {
	kind, rest, _ := strings.Cut(s, " ")
	ns, name, _ = strings.Cut(rest, "/")
	return kind, ns, name
}

// settleIngress gives the ingress component a namespace only when every
// Ingress object agrees on it; the objects keep their own.
func (a *accumulator) settleIngress() {
	c := a.f.candidate(a.f.canonical(ingressID))
	if c == nil || c.Kind != "" {
		return // none, or folded into the controller's workload
	}
	seen := map[string]bool{}
	for _, o := range c.Objects {
		if kind, ns, _ := parseObjectRef(o); kind == "Ingress" {
			seen[ns] = true
		}
	}
	c.Namespace = ""
	if len(seen) == 1 {
		c.Namespace = sortedKeys(seen)[0]
	}
}

// placement records where the manifest says the pods may run: the node
// selector, the required node affinity and the tolerations. Propose matches
// them against the nodes it knows.
func placement(c *Candidate, podSpec map[string]any) {
	want := map[string]string{} // label -> value, or values joined by |
	for k, v := range stringMap(podSpec["nodeSelector"]) {
		want[k] = v
	}
	if n := str(podSpec, "nodeName"); n != "" {
		want[hostnameLabel] = n
	}
	terms := list(podSpec, "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	if len(terms) == 1 { // several terms are alternatives; one term is a rule
		for _, ev := range list(asMap(terms[0]), "matchExpressions") {
			em := asMap(ev)
			if str(em, "operator") != "In" {
				continue
			}
			var vals []string
			for _, x := range list(em, "values") {
				vals = append(vals, scalar(x))
			}
			if key := str(em, "key"); key != "" && len(vals) > 0 {
				want[key] = strings.Join(vals, "|")
			}
		}
	}
	var sel []string
	for _, k := range sortedKeys(want) {
		if !hasPlaceholder(want[k]) {
			sel = append(sel, k+"="+want[k])
		}
	}
	if len(sel) > 0 {
		c.Extra["node_selector"] = strings.Join(sel, ",")
	}
	var tol []string
	for _, tv := range list(podSpec, "tolerations") {
		tm := asMap(tv)
		key := str(tm, "key")
		if key == "" {
			key = "*" // tolerates every taint
		}
		if v := str(tm, "value"); v != "" {
			key += "=" + v
		}
		tol = append(tol, key)
	}
	if len(tol) > 0 {
		c.Extra["tolerations"] = strings.Join(uniq(tol), ",")
	}
}

// hostnameLabel is the label every node carries with its own name.
const hostnameLabel = "kubernetes.io/hostname"

// addConfigMap keeps the data of a ConfigMap for the workloads that load
// it. When two files define the same ConfigMap (a base and an overlay), the
// keys of the first one read stay.
func (a *accumulator) addConfigMap(ns, name string, data map[string]string) {
	key := ns + "/" + name
	if a.configMaps[key] == nil {
		a.configMaps[key] = map[string]string{}
	}
	for k, v := range data {
		if _, ok := a.configMaps[key][k]; !ok {
			a.configMaps[key][k] = v
		}
	}
}

// configMap finds a ConfigMap by namespace and name. A ConfigMap or a
// workload without a namespace matches by name when only one carries it.
func (a *accumulator) configMap(ns, name string) (map[string]string, bool) {
	if data, ok := a.configMaps[ns+"/"+name]; ok {
		return data, true
	}
	var found []string
	for _, key := range sortedKeys(a.configMaps) {
		if strings.HasSuffix(key, "/"+name) {
			found = append(found, key)
		}
	}
	if len(found) == 1 {
		return a.configMaps[found[0]], true
	}
	return nil, false
}

// scanKustomization reads a kustomization file for the namespace it puts
// its resources in and the ConfigMaps it generates from literals and env
// files.
func scanKustomization(a *accumulator, path, rel string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var k map[string]any
	if err := yaml.Unmarshal(b, &k); err != nil || k == nil {
		return
	}
	dir := filepath.Dir(path)
	ns := str(k, "namespace")
	if hasPlaceholder(ns) {
		ns = ""
	}
	if ns != "" {
		a.kustomizeNS[dir] = ns
		// The namespace reaches the directories the kustomization pulls
		// in (an overlay over a base), unless they set their own.
		for _, key := range []string{"resources", "bases", "components"} {
			for _, rv := range list(k, key) {
				res := scalar(rv)
				if res == "" || strings.Contains(res, "://") {
					continue
				}
				target := filepath.Join(dir, res)
				if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
					continue
				}
				a.claimNamespace(target, ns, filepath.Dir(rel))
			}
		}
	}
	for _, gv := range list(k, "configMapGenerator") {
		gm := asMap(gv)
		name := str(gm, "name")
		if name == "" {
			continue
		}
		data := map[string]string{}
		for _, lv := range list(gm, "literals") {
			if key, val, ok := strings.Cut(scalar(lv), "="); ok {
				data[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(val), `"'`)
			}
		}
		for _, ev := range list(gm, "envs") {
			for key, val := range readEnvFile(filepath.Join(dir, scalar(ev))) {
				if _, ok := data[key]; !ok {
					data[key] = val
				}
			}
		}
		cmNS := firstNonEmpty(str(gm, "namespace"), ns)
		a.addConfigMap(cmNS, name, data)
	}
}

// productionOverlays are the overlay names that win when several overlays
// use one base.
var productionOverlays = map[string]bool{"production": true, "prod": true, "live": true}

// claimNamespace records that the overlay in by puts the resources of dir
// in ns. When overlays disagree, production wins and the choice is noted.
func (a *accumulator) claimNamespace(dir, ns, by string) {
	if a.kustomizeOwn == nil {
		a.kustomizeOwn = map[string]string{}
	}
	cur, claimed := a.kustomizeNS[dir]
	prev, byOverlay := a.kustomizeOwn[dir]
	switch {
	case !claimed:
		a.kustomizeNS[dir], a.kustomizeOwn[dir] = ns, by
	case !byOverlay || cur == ns:
		// the directory set its own namespace, or both agree
	default:
		rel, _ := filepath.Rel(a.root, dir)
		if productionOverlays[filepath.Base(by)] && !productionOverlays[filepath.Base(prev)] {
			a.kustomizeNS[dir], a.kustomizeOwn[dir] = ns, by
			prev, by = by, prev
		}
		a.note("%s is used by the overlays %s and %s with different namespaces; the namespace of %s was taken", filepath.ToSlash(rel), filepath.ToSlash(prev), filepath.ToSlash(by), filepath.ToSlash(prev))
	}
}

// readEnvFile reads KEY=VALUE lines.
func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

// envValue is the only way a literal value enters a candidate's
// environment: reduced to what redact.EnvValue lets through.
func envValue(name, raw string) string {
	return redact.EnvValue(name, raw)
}

// resolveWorkloads resolves the environment of every pod template read,
// now that all the ConfigMaps are known, and derives the links from it.
func (a *accumulator) resolveWorkloads() {
	for _, pw := range a.workloads {
		env := map[string]string{}
		var from, cmds []string
		for _, pc := range pw.containers {
			// raw holds the literal values of this container for $(VAR)
			// interpolation. It never leaves this loop.
			raw := map[string]string{}
			lookup := func(name string) (string, bool) { v, ok := raw[name]; return v, ok }
			// envFrom first: env overrides it.
			for _, src := range pc.from {
				if src.secret != "" {
					from = append(from, "secret:"+src.secret)
				}
				if src.configMap == "" {
					continue
				}
				from = append(from, "configmap:"+src.configMap)
				data, ok := a.configMap(pw.ns, src.configMap)
				if !ok {
					a.note("%s loads its environment from the ConfigMap %s, which is not in the repository: its connections can only be read from the cluster", pw.id, src.configMap)
					continue
				}
				for _, k := range sortedKeys(data) {
					raw[src.prefix+k] = data[k]
				}
				for _, k := range sortedKeys(data) {
					name := src.prefix + k
					env[name] = firstNonEmpty(envValue(name, redact.Expand(data[k], lookup)), "configmap:"+src.configMap+"/"+k)
				}
			}
			for _, e := range pc.env {
				switch {
				case e.secret != "":
					env[e.name] = "secret:" + e.secret
					continue
				case e.configMap != "":
					env[e.name] = "configmap:" + e.configMap + "/" + e.key
					data, ok := a.configMap(pw.ns, e.configMap)
					if !ok || data[e.key] == "" {
						continue
					}
					e.value = data[e.key]
				}
				value := redact.Expand(e.value, lookup)
				raw[e.name] = value
				if v := envValue(e.name, value); v != "" || e.configMap == "" {
					env[e.name] = v // a ConfigMap value that names no host keeps its reference
				}
			}
			if args := redact.Args(pc.args); len(args) > 0 {
				cmds = append(cmds, strings.Join(args, " "))
			}
		}
		c := a.f.candidate(a.f.canonical(pw.id))
		if c == nil {
			continue
		}
		c.Env = mergeMap(c.Env, env)
		if c.Extra == nil {
			c.Extra = map[string]string{}
		}
		if len(from) > 0 {
			c.Extra["env_from"] = strings.Join(uniq(from), " ")
		}
		if len(cmds) > 0 {
			c.Extra["command"] = strings.Join(cmds, " ; ")
		}
		a.deriveLinks(c.ID, cmds, "manifest", pw.rel, pw.line)
	}
	a.workloads = nil
	a.attachServices()
}

// deriveLinks reads the environment and the commands of a candidate for
// the flows they imply. The candidates and links they produce are added
// afterwards: adding may move the candidates in memory, and the one being
// read must stay put while it is changed.
func (a *accumulator) deriveLinks(id string, cmds []string, source, rel string, line int) {
	c := a.f.candidate(id)
	if c == nil {
		return
	}
	var d derived
	envLinks(&d, c, source, rel, line)
	commandLinks(&d, c, cmds, source, rel, line)
	for _, nc := range d.candidates {
		a.add(nc)
	}
	for _, l := range d.links {
		a.link(l)
	}
}

// derived collects what the environment of one workload implies.
type derived struct {
	candidates []Candidate
	links      []Link
}

// attachServices gives the addresses of each Service to the workload its
// selector picks, by labels: a Service called "backend" that selects
// app=crm-backend names the Deployment whose pods carry that label,
// whatever the Deployment is called. Failing that, the Service belongs to
// whatever already answers to its name (the Services an operator creates
// for a database). A Service that selects nothing known keeps a candidate
// of its own, which is only drawn if a workload turns up for it.
func (a *accumulator) attachServices() {
	for _, s := range a.services {
		addrs := serviceAddresses(s.name, s.ns)
		note := s.ev
		target := a.selected(s)
		if target == "" {
			target = a.owner(addrs[len(addrs)-1])
		}
		if target != "" {
			note.Note = "Service " + s.name + " selects " + target
			a.add(Candidate{ID: target, Addresses: addrs, Evidence: []Evidence{note}})
			continue
		}
		target = firstNonEmpty(s.selector["app"], s.selector["app.kubernetes.io/name"], s.name)
		note.Note = "Service " + s.name + " selects " + target
		c := Candidate{ID: model.SlugifyID(target), Type: "custom", Label: labelFor(target), Namespace: s.ns, Addresses: addrs, Evidence: []Evidence{note}}
		if cur := a.add(c); cur != nil && cur.Type == "custom" && cur.Kind != "" {
			cur.Type = "workload"
		}
	}
	a.services = nil
}

// owner returns the id of the candidate that already answers to an address.
func (a *accumulator) owner(address string) string {
	for _, c := range a.f.Candidates {
		if c.Type == "custom" {
			continue
		}
		for _, have := range c.Addresses {
			if have == address {
				return c.ID
			}
		}
	}
	return ""
}

// selected returns the id of the workload whose pods carry every label of
// the Service's selector, in the Service's namespace.
func (a *accumulator) selected(s pendingService) string {
	if len(s.selector) == 0 {
		return ""
	}
	for _, c := range a.f.Candidates {
		if c.Kind == "" || len(c.Labels) == 0 || (c.Namespace != "" && s.ns != "" && c.Namespace != s.ns) {
			continue
		}
		match := true
		for k, v := range s.selector {
			if c.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return c.ID
		}
	}
	return ""
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

// envHint is what the name of a variable implies.
type envHint struct {
	pattern string
	kind    string
	dstType string
	note    string
}

// envHints recognizes what an env var name implies even when its value is a secret.
var envHints = []envHint{
	{"ELECTRIC_DATABASE_URL", "replication", "database", "electric source database"},
	{"DATABASE_URL", "sql", "database", "database connection"},
	{"POSTGRES", "sql", "database", "database connection"},
	{"PG_DSN", "sql", "database", "database connection"},
	{"PGHOST", "sql", "database", "database host"},
	{"DB_HOST", "sql", "database", "database host"},
	{"REDIS", "cache", "cache", "redis connection"},
	{"CELERY_BROKER", "queue", "queue", "celery broker"},
	{"BROKER_URL", "queue", "queue", "broker"},
	{"RABBITMQ", "queue", "queue", "rabbitmq"},
	{"AMQP", "queue", "queue", "amqp"},
	{"HATCHET_CLIENT_HOST_PORT", "grpc", "workload", "hatchet engine"},
	{"HATCHET_CLIENT_TOKEN", "grpc", "workload", "hatchet token"},
	{"ELECTRIC_URL", "http", "syncengine", "electric shapes"},
	{"SIGNOZ", "tcp", "observability", "traces"},
	{"OTEL_EXPORTER_OTLP", "tcp", "observability", "traces"},
	{"SENTRY_DSN", "external", "external", "error tracking"},
	{"STRIPE", "external", "external", "payments"},
	{"OPENAI", "external", "external", "llm"},
	{"ANTHROPIC", "external", "external", "llm"},
	{"GITHUB", "external", "external", "github"},
	{"S3_ENDPOINT", "tcp", "storage", "object storage"},
	{"AWS_S3", "tcp", "storage", "object storage"},
	{"S3_BUCKET", "tcp", "storage", "object storage"},
	{"BUCKET_NAME", "tcp", "storage", "object storage"},
	{"MINIO", "tcp", "storage", "object storage"},
	{"AWS_ENDPOINT_URL", "tcp", "storage", "object storage"},
}

// hintFor returns the first hint the name matches.
func hintFor(upper string) (envHint, bool) {
	for _, h := range envHints {
		if strings.Contains(upper, h.pattern) {
			return h, true
		}
	}
	return envHint{}, false
}

// bucketEndpointEnv lists the env vars that name an S3-compatible endpoint,
// in the order they are tried.
var bucketEndpointEnv = []string{"S3_ENDPOINT", "S3_ENDPOINT_URL", "AWS_S3_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL", "MINIO_ENDPOINT"}

// bucketRegionEnv lists the env vars that name the bucket's region.
var bucketRegionEnv = []string{"S3_REGION", "AWS_S3_REGION_NAME", "AWS_REGION", "AWS_DEFAULT_REGION"}

// literalEnv reports whether an env value is a plain literal, not a secret
// or configmap reference.
func literalEnv(val string) bool {
	return val != "" && !strings.HasPrefix(val, "secret:") && !strings.HasPrefix(val, "configmap:")
}

// settingName reports whether a variable holds a plain setting (a bucket, a
// region, a replication slot) rather than the address of something.
func settingName(upper string) bool {
	return strings.Contains(upper, "BUCKET") || strings.Contains(upper, "REGION") || strings.Contains(upper, "SLOT")
}

// bucketLinks turns a literal bucket name in the environment (S3_BUCKET,
// AWS_STORAGE_BUCKET_NAME, MEDIA_BUCKET) into a storage candidate the
// workload writes to, carrying the endpoint and region when the same
// environment names them. It reports whether it found a bucket.
func bucketLinks(d *derived, c *Candidate, source, rel string, line int) bool {
	found := false
	for _, name := range sortedKeys(c.Env) {
		val := c.Env[name]
		upper := strings.ToUpper(name)
		if !strings.Contains(upper, "BUCKET") || !literalEnv(val) || strings.ContainsAny(val, "/:$") || hasPlaceholder(val) {
			continue
		}
		found = true
		bc := Candidate{ID: bucketID(val), Type: "storage", Label: val + " bucket", Name: val, Extra: map[string]string{"bucket": val},
			Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: name + "=" + val + " in " + c.Kind + " " + c.Name}}}
		for _, k := range bucketEndpointEnv {
			if v := c.Env[k]; literalEnv(v) {
				bc.Extra["endpoint"] = v
				break
			}
		}
		for _, k := range bucketRegionEnv {
			if v := c.Env[k]; literalEnv(v) {
				bc.Extra["region"] = v
				break
			}
		}
		d.candidates = append(d.candidates, bc)
		d.links = append(d.links, Link{From: c.ID, To: bc.ID, Kind: "tcp", Label: "objects", Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: c.Name + " uses bucket " + val}}})
	}
	return found
}

// bucketID is the id of the storage component for a bucket, the same
// whether the bucket was seen in Terraform, in the environment or in code.
func bucketID(bucket string) string {
	return model.SlugifyID(bucket + "-bucket")
}

// target is where a value of the environment points: a URL reduced to its
// host, or a bare host with an optional port.
func target(val string) (hostRef, bool) {
	if strings.Contains(val, "://") {
		return parseDSN(val)
	}
	host, port, _ := strings.Cut(val, ":")
	if host == "" {
		return hostRef{}, false
	}
	return hostRef{Host: strings.ToLower(host), Port: port, Raw: val}, true
}

// envLinks derives links from a candidate's environment. The values were
// reduced when they were read: what is left of a literal is a host, a URL
// without user, path or query, or a plain setting.
func envLinks(d *derived, c *Candidate, source, rel string, line int) {
	hasBucket := bucketLinks(d, c, source, rel, line)
	endpoint := map[string]bool{}
	for _, k := range bucketEndpointEnv {
		endpoint[k] = true
	}
	for _, name := range sortedKeys(c.Env) {
		val := c.Env[name]
		upper := strings.ToUpper(name)
		hint, hinted := hintFor(upper)
		if !literalEnv(val) {
			// secret-backed: the name still tells what it connects to
			if hinted {
				c.Extra["needs:"+hint.dstType] = strings.Join(uniq(append(strings.Fields(c.Extra["needs:"+hint.dstType]), name)), " ")
			}
			continue
		}
		if settingName(upper) || hasBucket && endpoint[upper] {
			continue // the bucket carries its endpoint; it is one component
		}
		for _, part := range strings.Split(val, ",") {
			h, ok := target(part)
			if !ok {
				continue
			}
			kind := edgeKindFor(h.Scheme, "")
			switch {
			case (h.Scheme == "" || h.Scheme == "tcp") && hinted:
				kind = hint.kind // no protocol in the value: the name says it
			case (h.Scheme == "http" || h.Scheme == "https") && hinted && hint.kind != "http" && hint.kind != "external":
				kind = hint.kind // OTLP over http is still the trace flow
			}
			l := Link{From: c.ID, Host: h.Host, Port: h.Port, Kind: kind, Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: name + " in " + strings.TrimSpace(c.Kind+" "+c.Name)}}}
			if (l.Kind == "sql" || l.Kind == "replication") && (upper == "ELECTRIC_DATABASE_URL" || c.Type == "syncengine") {
				// a sync engine reads the database's replication stream
				l.Kind, l.Reverse = "replication", true
			} else if l.Kind == "replication" {
				l.Kind = "sql"
			}
			d.links = append(d.links, l)
		}
	}
	// Electric: the sync service's source database is a replication edge.
	if c.Type == "syncengine" {
		c.Extra["replication_slot"] = firstNonEmpty(c.Env["ELECTRIC_REPLICATION_SLOT"], "electric_slot_default")
	}
}

// commandLinks reads celery/hatchet/electric hints from container commands.
func commandLinks(d *derived, c *Candidate, cmds []string, source, rel string, line int) {
	broker := ""
	for _, k := range []string{"CELERY_BROKER_URL", "CELERY_BROKER", "BROKER_URL"} {
		if v := c.Env[k]; literalEnv(v) {
			broker = v
			break
		}
	}
	queue := func(id, label, name, note string) {
		qc := Candidate{ID: id, Type: "queue", Label: label, Namespace: c.Namespace, Name: name, Extra: map[string]string{"celery": "true", "queue": name}, Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: note}}}
		if broker != "" {
			qc.Extra["broker"] = broker
		}
		d.candidates = append(d.candidates, qc)
	}
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
					id := model.SlugifyID(q + "-queue")
					queue(id, labelFor(q)+" queue", q, "celery worker consumes -Q "+q)
					d.links = append(d.links, Link{From: id, To: c.ID, Kind: "queue", Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: c.Name + " consumes " + q}}})
				}
			} else {
				queue("celery-queue", "Celery queue", "celery", "celery worker with the default queue")
				d.links = append(d.links, Link{From: "celery-queue", To: c.ID, Kind: "queue", Evidence: []Evidence{{Source: source, File: rel, Line: line, Note: c.Name + " consumes the default queue"}}})
			}
		}
		if strings.Contains(lc, "celery") && strings.Contains(lc, "beat") {
			c.Extra["celery_beat"] = "true"
		}
		if strings.Contains(lc, "flower") {
			c.Extra["flower"] = "true"
		}
	}
	img := strings.ToLower(c.Image)
	official := strings.Contains(img, "hatchet-dev/hatchet-") // engine, api, dashboard, lite
	_, hasToken := c.Env["HATCHET_CLIENT_TOKEN"]
	if !official && c.Type == "workload" && (strings.Contains(img, "hatchet") || hasToken) && strings.Contains(strings.ToLower(c.Name+" "+img), "worker") {
		c.Extra["hatchet_worker"] = "true"
	}
	if c.Type == "workload" && (c.Extra["celery_worker"] == "true" || c.Extra["hatchet_worker"] == "true") {
		c.Type = "backgroundworker"
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
			c.Env[k] = envValue(k, scalar(val))
		}
	case []any:
		for _, item := range e {
			m := asMap(item)
			if n := str(m, "name"); n != "" {
				c.Env[n] = envValue(n, str(m, "value"))
			}
		}
	}
	if len(c.Env) == 0 && img == "" {
		return
	}
	if added := a.add(c); added != nil {
		a.deriveLinks(added.ID, nil, "helm", rel, 1)
	}
}

func chartNameFrom(rel string) string {
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return strings.TrimSuffix(parts[0], ".yaml")
}
