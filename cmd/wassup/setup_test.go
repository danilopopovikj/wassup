package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/app"
	"github.com/danilopopovikj/wassup/internal/bind"
	"github.com/danilopopovikj/wassup/internal/discover"
	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
	"github.com/danilopopovikj/wassup/internal/state"
)

func TestParseEnvTakesAValueAsItIsWritten(t *testing.T) {
	vals, order, err := parseEnv(`
# the cluster
WASSUP_KUBECONFIG=infra/out/kubeconfig.yaml
export WASSUP_CONTEXT = production

BOOKSTORE_DB_PASSWORD=p@ss:w/rd#1 $HOME %41=
QUOTED="two words"
SINGLE='it''s'
EMPTY=
`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"WASSUP_KUBECONFIG":     "infra/out/kubeconfig.yaml",
		"WASSUP_CONTEXT":        "production",
		"BOOKSTORE_DB_PASSWORD": "p@ss:w/rd#1 $HOME %41=",
		"QUOTED":                "two words",
		"SINGLE":                "it''s",
		"EMPTY":                 "",
	}
	for k, v := range want {
		if got, ok := vals[k]; !ok || got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(vals) != len(want) || len(order) != len(want) || order[0] != "WASSUP_KUBECONFIG" {
		t.Errorf("read %v in order %v", vals, order)
	}
}

func TestParseEnvDoesNotPrintTheLineItCannotRead(t *testing.T) {
	_, _, err := parseEnv("OK=1\nhunter2-is-the-password\n")
	if err == nil || !strings.Contains(err.Error(), "line 2") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err = %v, want the line number and not the line", err)
	}
}

func TestLoadSettingsReadsLocalEnvAndKeepsItOutOfGit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), model.DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	local := "WASSUP_KUBECONFIG=/infra/kubeconfig.yaml\nWASSUP_CONTEXT=production\nBOOKSTORE_DB_PASSWORD=from-the-file\nBOOKSTORE_TOKEN=from-the-file\n"
	if err := os.WriteFile(filepath.Join(dir, localEnvName), []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(t.TempDir(), "extra.env")
	if err := os.WriteFile(extra, []byte("BOOKSTORE_TOKEN=from-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"WASSUP_KUBECONFIG", "WASSUP_CONTEXT", "BOOKSTORE_TOKEN"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("BOOKSTORE_DB_PASSWORD", "from-the-environment")
	saved := flags
	t.Cleanup(func() { flags = saved })
	flags.dir, flags.envFile, flags.kubeconfig, flags.kcontext, flags.clusterChosen = dir, extra, "", "", false

	if err := loadSettings(); err != nil {
		t.Fatal(err)
	}
	if flags.kubeconfig != "/infra/kubeconfig.yaml" || flags.kcontext != "production" || !flags.clusterChosen {
		t.Errorf("kubeconfig %q, context %q, chosen %v", flags.kubeconfig, flags.kcontext, flags.clusterChosen)
	}
	if got := os.Getenv("BOOKSTORE_DB_PASSWORD"); got != "from-the-environment" {
		t.Errorf("the environment must win over the file, got %q", got)
	}
	if got := os.Getenv("BOOKSTORE_TOKEN"); got != "from-env-file" {
		t.Errorf("--env-file must win over local.env, got %q", got)
	}
	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || !strings.Contains(string(gi), localEnvName) {
		t.Errorf(".gitignore = %q (%v), want it to ignore %s", gi, err, localEnvName)
	}
}

func TestAFlagWinsOverTheSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), model.DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, localEnvName), []byte("WASSUP_CONTEXT=staging\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WASSUP_CONTEXT", "")
	os.Unsetenv("WASSUP_CONTEXT")
	saved := flags
	t.Cleanup(func() { flags = saved })
	flags.dir, flags.envFile, flags.kubeconfig, flags.kcontext = dir, "", "", "production"
	if err := loadSettings(); err != nil {
		t.Fatal(err)
	}
	if flags.kcontext != "production" {
		t.Errorf("context = %q, want the flag", flags.kcontext)
	}
}

func TestPathHint(t *testing.T) {
	sep := string(os.PathListSeparator)
	if got := pathHintFor("/home/ada/go/bin", "/usr/bin"+sep+"/home/ada/go/bin", "/bin/zsh"); got != "" {
		t.Errorf("a directory on the PATH needs no hint, got %q", got)
	}
	got := pathHintFor("/home/ada/go/bin", "/usr/bin"+sep+"/bin", "/bin/zsh")
	for _, want := range []string{"/home/ada/go/bin is not on your PATH", "~/.zshrc", `export PATH="/home/ada/go/bin:$PATH"`} {
		if !strings.Contains(got, want) {
			t.Errorf("hint %q lacks %q", got, want)
		}
	}
	if got := pathHintFor("/home/ada/go/bin", "/usr/bin", "/usr/bin/fish"); !strings.Contains(got, "fish_add_path /home/ada/go/bin") {
		t.Errorf("fish hint %q", got)
	}
}

var setupBindings = model.Bindings{
	Components: map[string][]model.ProbeSpec{
		"api":   {{"probe": "k8s.workload", "namespace": "shop", "selector": "app=api"}},
		"node":  {{"probe": "k8s.node", "name": "node-1"}},
		"db":    {{"probe": "cnpg.cluster", "namespace": "shop", "cluster": "bookstore-db"}, {"probe": "pg.stats", "dsn_env": "BOOKSTORE_DSN", "via": "k8s.service/shop/bookstore-db-rw:5432"}},
		"cache": {{"probe": "redis.info", "addr": "redis.shop:6379"}},
		"jobs":  {{"probe": "hatchet.queue", "url": "http://hatchet-api.shop:8080"}},
		"site":  {{"probe": "k8s.ingress", "namespace": "shop", "name": "site", "read_tls_secret": true}},
	},
	Edges: map[string][]model.ProbeSpec{
		"api->db": {{"probe": "pg.pool", "dsn_env": "POOL_DSN", "via": "k8s.service/data/pooler:6432"}},
	},
}

func TestAccessIsWhatTheBoundProbesReadAndNothingElse(t *testing.T) {
	p := buildAccess(setupBindings, "wassup", "default", "8h", false)
	have := map[string]string{}
	for _, r := range p.Rules {
		for _, res := range r.Resources {
			have[r.Group+"/"+res] = strings.Join(r.Verbs, ",")
		}
	}
	for _, res := range []string{"/pods", "/nodes", "/events", "apps/deployments", "networking.k8s.io/ingresses", "postgresql.cnpg.io/clusters", "metrics.k8s.io/pods"} {
		if have[res] != "get,list,watch" {
			t.Errorf("%s: verbs %q, want get,list,watch", res, have[res])
		}
	}
	// Nothing bound reads cron jobs or claims, so the account cannot either.
	for _, res := range []string{"batch/cronjobs", "/persistentvolumeclaims"} {
		if _, ok := have[res]; ok {
			t.Errorf("%s is granted and no bound probe reads it", res)
		}
	}
	for res, verbs := range have {
		for _, v := range strings.Split(verbs, ",") {
			if v != "get" && v != "list" && v != "watch" {
				t.Errorf("the role of reads has %s on %s", v, res)
			}
		}
		for _, never := range []string{"secrets", "nodes/proxy", "pods/exec", "pods/attach", "pods/log"} {
			if strings.HasSuffix(res, "/"+never) {
				t.Errorf("the account is given %s", res)
			}
		}
	}
	if got := strings.Join(p.ForwardNamespaces, ","); got != "data,shop" {
		t.Errorf("port-forward in %q, want the two namespaces a via reaches into", got)
	}
	for _, never := range []string{"secrets", "nodes/proxy", "exec", "attach", `"*"`} {
		if strings.Contains(p.Manifest, never) {
			t.Errorf("the manifest names %s:\n%s", never, p.Manifest)
		}
	}
	for _, want := range []string{"kind: ServiceAccount", "kind: ClusterRole\n", "kind: ClusterRoleBinding", "kind: Role\n", "name: wassup-forward\n  namespace: data", `resources: ["pods/portforward"]`} {
		if !strings.Contains(p.Manifest, want) {
			t.Errorf("the manifest lacks %q:\n%s", want, p.Manifest)
		}
	}
	left := strings.Join(p.LeftOut, "\n")
	for _, want := range []string{"nodes/proxy", "secrets", "pods/log"} {
		if !strings.Contains(left, want) {
			t.Errorf("what was left out does not say %s: %q", want, left)
		}
	}
	for _, want := range []string{"create token wassup --duration 8h", "WASSUP_KUBECONFIG=", "wassup runs none of them"} {
		if !strings.Contains(p.Commands, want) {
			t.Errorf("the commands lack %q:\n%s", want, p.Commands)
		}
	}
}

func TestAccessWithLogs(t *testing.T) {
	p := buildAccess(setupBindings, "wassup", "default", "8h", true)
	if !strings.Contains(p.Manifest, `"pods/log"`) || !strings.Contains(p.Commands, "--logs") {
		t.Errorf("--logs must grant pods/log and be kept in the command:\n%s\n%s", p.Manifest, p.Commands)
	}
}

func TestAccessWithoutClusterProbes(t *testing.T) {
	p := buildAccess(model.Bindings{Components: map[string][]model.ProbeSpec{"cache": {{"probe": "redis.info", "addr": "redis.shop:6379"}}}}, "wassup", "default", "8h", false)
	if len(p.Rules) != 0 || len(p.ForwardNamespaces) != 0 || strings.Contains(p.Manifest, "ClusterRole") {
		t.Errorf("no binding reads the cluster: %+v", p)
	}
}

func setupConfig() *model.Config {
	return &model.Config{
		Topology: model.Topology{Name: "bookstore", Components: []model.Component{
			{ID: "api", Type: "workload", Label: "API"},
			{ID: "node", Type: "node", Label: "Node 1"},
			{ID: "db", Type: "database", Label: "Database"},
			{ID: "cache", Type: "cache", Label: "Cache"},
			{ID: "jobs", Type: "queue", Label: "Jobs"},
			{ID: "site", Type: "ingress", Label: "Site"},
		}, Edges: []model.Edge{{From: "api", To: "db", Kind: "sql"}}},
		Bindings: setupBindings,
	}
}

func TestTiers(t *testing.T) {
	want := map[string]int{
		"k8s.workload": probe.TierOpen, "k8s.node": probe.TierOpen, "cnpg.cluster": probe.TierOpen, "k8s.ingress": probe.TierOpen,
		"dns.record": probe.TierOpen, "cert.tls": probe.TierOpen, "http.ping": probe.TierOpen,
		"hatchet.queue": probe.TierToken, "electric.sync": probe.TierToken, "hcloud.lb": probe.TierToken, "s3.bucket": probe.TierToken,
		"pg.stats": probe.TierData, "pg.pool": probe.TierData, "redis.info": probe.TierData,
	}
	for kind, tier := range want {
		if got := tierOf(model.ProbeSpec{"probe": kind}); got != tier {
			t.Errorf("%s is tier %d, want %d", kind, got, tier)
		}
	}
}

func TestATierLeavesTheRestForLater(t *testing.T) {
	cfg := setupConfig()
	sel := probeSelection{Tier: probe.TierOpen}
	ran := map[string]bool{}
	for id, specs := range cfg.Bindings.Components {
		for _, s := range specs {
			if sel.runs(id, s) {
				ran[id+" "+s.Kind()] = true
			}
		}
	}
	for _, want := range []string{"api k8s.workload", "node k8s.node", "db cnpg.cluster", "site k8s.ingress"} {
		if !ran[want] {
			t.Errorf("tier 0 must run %s", want)
		}
	}
	for _, never := range []string{"db pg.stats", "cache redis.info", "jobs hatchet.queue"} {
		if ran[never] {
			t.Errorf("tier 0 must not run %s", never)
		}
	}
	if !sel.readsCluster(cfg) {
		t.Error("tier 0 reads the cluster")
	}

	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	rows := map[string]probeRow{}
	snap, joined, inst := evaluate(t, cfg, now,
		probe.Observation{Target: "api", Probe: "k8s.workload", Metrics: map[string]float64{"replicas_ready": 3, "replicas_desired": 3}},
		probe.Observation{Target: "node", Probe: "k8s.node", Metrics: map[string]float64{"cpu_pct": 20}},
		probe.Observation{Target: "db", Probe: "cnpg.cluster", Metrics: map[string]float64{"replicas_ready": 3, "replicas_desired": 3}},
		probe.Observation{Target: "site", Probe: "k8s.ingress", Metrics: map[string]float64{"cert_days": 60}},
	)
	for _, r := range sel.rows(cfg, snap, joined, inst) {
		rows[r.ID] = r
	}
	for _, id := range []string{"cache", "jobs", "api->db"} {
		if r := rows[id]; r.Later == "" || r.Bound || !strings.HasPrefix(r.Label, "not asked yet, ") {
			t.Errorf("%s waits for a later tier: %+v", id, r)
		}
	}
	if r := rows["cache"]; !strings.Contains(r.Later, "redis.info (tier 2)") {
		t.Errorf("cache: later = %q", r.Later)
	}
	if r := rows["db"]; !r.Bound || r.Later != "" || r.Probes != "cnpg.cluster" {
		t.Errorf("the database is bound on what tier 0 reads: %+v", r)
	}
	var list []probeRow
	for _, r := range rows {
		list = append(list, r)
	}
	c := countProbeRows(list)
	if c.Later != 3 || c.Total != 4 || c.Bound != 4 || c.EdgesTotal != 0 || c.unbound() {
		t.Errorf("counts %+v: what waits for a later tier is not unbound", c)
	}
}

func TestProbeOneElementPrintsWhatItRead(t *testing.T) {
	cfg := setupConfig()
	sel := probeSelection{Tier: probe.TierData, ID: "db"}
	if sel.runs("api", cfg.Bindings.Components["api"][0]) || !sel.runs("db", cfg.Bindings.Components["db"][1]) {
		t.Fatal("only the bindings of the database run")
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	snap, joined := evaluateJoined(t, cfg, now,
		probe.Observation{Target: "db", Probe: "cnpg.cluster", Metrics: map[string]float64{"replicas_ready": 3, "replicas_desired": 3}},
		probe.Observation{Target: "db", Probe: "pg.stats", Metrics: map[string]float64{"connections_used": 12, "connections_max": 100},
			Detail: map[string]any{"lag_source": "pg_replication_slots"}},
	)
	rows := sel.rows(cfg, snap, joined, nil)
	if len(rows) != 1 || rows[0].ID != "db" {
		t.Fatalf("rows %+v, want the database alone", rows)
	}
	r := rows[0]
	if len(r.Results) != 2 || r.Results[1].Metrics["connections_used"] != 12 || r.Detail["lag_source"] != "pg_replication_slots" {
		t.Errorf("the values of the database: %+v", r)
	}
}

func TestElementID(t *testing.T) {
	cfg := setupConfig()
	for arg, want := range map[string]string{"db": "db", "api->db": "api->db", "wassup://database/db": "db"} {
		got, err := elementID(cfg, arg)
		if err != nil || got != want {
			t.Errorf("elementID(%q) = %q, %v, want %q", arg, got, err, want)
		}
	}
	if _, err := elementID(cfg, "data"); err == nil || !strings.Contains(err.Error(), "did you mean db") {
		t.Errorf("err = %v, want the id that is near", err)
	}
}

func TestProgressLine(t *testing.T) {
	got := progressLine(app.Progress{Target: "db", Kind: "pg.stats", Status: "error", Error: "connection refused\nmore", Elapsed: 1240 * time.Millisecond, Done: 3, Total: 47})
	for _, want := range []string{"[3/47]", "error", "pg.stats", "db", "1.2s", "connection refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "more") {
		t.Errorf("line %q must be one line", got)
	}
}

// A run without a person at the terminal stops for the cluster and says
// which flags answer, instead of reading the default context.
func TestDiscoverStopsForTheClusterWhenNobodyCanBeAsked(t *testing.T) {
	repo := t.TempDir()
	kc := filepath.Join(repo, "infra", "kubeconfig.yaml")
	if err := os.MkdirAll(filepath.Dir(kc), 0o755); err != nil {
		t.Fatal(err)
	}
	// Port 1 refuses at once: the nodes are not read and the test is fast.
	body := `apiVersion: v1
kind: Config
current-context: staging
clusters:
  - name: staging
    cluster: {server: "https://127.0.0.1:1"}
  - name: production
    cluster: {server: "https://127.0.0.1:1"}
contexts:
  - name: staging
    context: {cluster: staging, user: admin}
  - name: production
    context: {cluster: production, user: admin}
users:
  - name: admin
    user: {token: not-a-real-token}
`
	if err := os.WriteFile(kc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := flags
	t.Cleanup(func() { flags = saved })
	flags.kubeconfig, flags.kcontext, flags.clusterChosen, flags.jsonOut = "", "", false, true
	t.Setenv("KUBECONFIG", kc)
	t.Setenv("WASSUP_KUBECONFIG", "")

	var out bytes.Buffer
	err := confirmCluster(t.Context(), repo, false, strings.NewReader(""), &out)
	q, ok := err.(*needsAnswer)
	if !ok {
		t.Fatalf("err = %v, want a question", err)
	}
	if q.Cluster == nil || q.Cluster.Context != "staging" || q.Cluster.Server != "https://127.0.0.1:1" {
		t.Errorf("cluster %+v", q.Cluster)
	}
	if len(q.Kubeconfigs) != 1 || q.Kubeconfigs[0].Path != kc {
		t.Errorf("kubeconfigs of the repository: %+v", q.Kubeconfigs)
	}
	answers := strings.Join(q.Answers, "\n")
	for _, want := range []string{"--context staging", "--no-cluster", "--context production"} {
		if !strings.Contains(answers, want) {
			t.Errorf("answers lack %q:\n%s", want, answers)
		}
	}

	// Named for wassup: nothing to ask.
	flags.clusterChosen = true
	if err := confirmCluster(t.Context(), repo, false, strings.NewReader(""), &out); err != nil {
		t.Errorf("a cluster that was named is not asked about: %v", err)
	}
}

func TestAskClusterTakesTheKubeconfigOfTheRepository(t *testing.T) {
	saved := flags
	t.Cleanup(func() { flags = saved })
	var out bytes.Buffer
	err := askCluster(clusterInfoFor("staging"), nil, strings.NewReader("n\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "not read") {
		t.Errorf("no: err = %v", err)
	}
	if err := askCluster(clusterInfoFor("staging"), nil, strings.NewReader("y\n"), &out); err != nil {
		t.Errorf("yes: err = %v", err)
	}
	if err := askCluster(clusterInfoFor("staging"), nil, strings.NewReader("7\n"), &out); err == nil {
		t.Error("a number without a choice is not an answer")
	}
}

// evaluateJoined runs the observations of one run through the binder and
// the engine.
func evaluateJoined(t *testing.T, cfg *model.Config, now time.Time, obs ...probe.Observation) (*model.Snapshot, map[string]*bind.Joined) {
	t.Helper()
	b := bind.New()
	for _, o := range obs {
		o.At = now
		b.Apply(o)
	}
	joined := b.All()
	return state.Evaluate(state.Input{Topology: &cfg.Topology, Joined: joined, Now: now, Tick: 1, TickEvery: 5 * time.Second}), joined
}

// evaluate is evaluateJoined for a caller that reports rows.
func evaluate(t *testing.T, cfg *model.Config, now time.Time, obs ...probe.Observation) (*model.Snapshot, map[string]*bind.Joined, []app.Instance) {
	t.Helper()
	snap, joined := evaluateJoined(t, cfg, now, obs...)
	return snap, joined, nil
}

func clusterInfoFor(context string) k8s.ClusterInfo {
	return k8s.ClusterInfo{Context: context, Server: "https://127.0.0.1:1", Kubeconfig: "/infra/kubeconfig.yaml"}
}

func TestValidateNamesTheProbesThatAreNotImplemented(t *testing.T) {
	got := strings.Join(notImplemented(model.Bindings{
		Components: map[string][]model.ProbeSpec{"api": {{"probe": "k8s.workload"}}, "signoz": {{"probe": "signoz.health"}}},
		Edges:      map[string][]model.ProbeSpec{"api->db": {{"probe": "signoz.edge"}}},
	}), "\n")
	if strings.Contains(got, "signoz.edge") {
		t.Errorf("a probe that ships is not named:\n%s", got)
	}
	for _, want := range []string{"signoz: probe signoz.health is not implemented", "not a fault"} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "k8s.workload") {
		t.Errorf("a probe that ships is not named:\n%s", got)
	}
	if w := notImplemented(setupBindings); len(w) != 0 {
		t.Errorf("nothing to say about %v", w)
	}
}

func TestRowsCarryWhatAProbeSaidAboutItsReading(t *testing.T) {
	cfg := setupConfig()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	snap, joined := evaluateJoined(t, cfg, now,
		probe.Observation{Target: "db", Probe: "pg.stats", Metrics: map[string]float64{"connections_used": 12},
			Detail: map[string]any{"grant_note": `role "app" lacks pg_read_all_stats, replication detail is hidden`, "lag_source": "pg_replication_slots"}},
	)
	for _, r := range (probeSelection{Tier: probe.TierData}).rows(cfg, snap, joined, nil) {
		if r.ID == "db" {
			if len(r.Advice) != 1 || !strings.Contains(r.Advice[0], "pg_read_all_stats") {
				t.Errorf("advice %v", r.Advice)
			}
			return
		}
	}
	t.Fatal("no row for the database")
}

// A draft is of one environment. With several and nobody to ask, the scan
// stops and lists them; a local one is never a choice.
func TestDraftAsksWhichEnvironment(t *testing.T) {
	saved := flags
	t.Cleanup(func() { flags = saved })
	flags.jsonOut = true
	var out bytes.Buffer
	envs := []discover.Environment{
		{Name: "local", Paths: []string{"deploy/overlays/local"}, Local: true},
		{Name: "production", Paths: []string{"deploy/overlays/production"}},
		{Name: "staging", Paths: []string{"deploy/overlays/staging"}},
	}
	_, err := chooseEnvironment(envs, strings.NewReader(""), &out)
	q, ok := err.(*needsAnswer)
	if !ok {
		t.Fatalf("err = %v, want a question", err)
	}
	if strings.Join(q.Environments, ",") != "production,staging" || strings.Join(q.Answers, ";") != "--environment production;--environment staging" {
		t.Errorf("question %+v", q)
	}
	// One environment besides the local one: nothing to choose.
	if env, err := chooseEnvironment(envs[:2], strings.NewReader(""), &out); err != nil || env != "" {
		t.Errorf("one environment: %q, %v", env, err)
	}
	if env, err := chooseEnvironment(nil, strings.NewReader(""), &out); err != nil || env != "" {
		t.Errorf("no environment: %q, %v", env, err)
	}
}

func TestDiscoverReadsOneEnvironment(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	deployment := func(name string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + name + "\n  namespace: shop\nspec:\n  selector:\n    matchLabels: {app: " + name + "}\n" +
			"  template:\n    metadata:\n      labels: {app: " + name + "}\n    spec:\n      containers:\n        - name: " + name + "\n          image: registry.bookstore.example/" + name + ":1\n"
	}
	kustomization := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [app.yaml]\n"
	write("deploy/base/kustomization.yaml", kustomization)
	write("deploy/base/app.yaml", deployment("api"))
	write("deploy/overlays/production/kustomization.yaml", kustomization)
	write("deploy/overlays/production/app.yaml", deployment("reports"))
	write("deploy/overlays/staging/kustomization.yaml", kustomization)
	write("deploy/overlays/staging/app.yaml", deployment("preview"))

	saved := flags
	t.Cleanup(func() { flags = saved })
	flags.jsonOut = true

	// Without a name a draft stops for the question.
	_, _, err := scanRepo(t.Context(), scanOptions{Repo: repo, NoCluster: true, Draft: true})
	if q, ok := err.(*needsAnswer); !ok || len(q.Environments) != 2 {
		t.Fatalf("err = %v, want the question with the two environments", err)
	}
	// Listing what is there asks nothing.
	f, _, err := scanRepo(t.Context(), scanOptions{Repo: repo, NoCluster: true})
	if err != nil || len(f.Environments) != 2 {
		t.Fatalf("environments %+v, %v", f, err)
	}
	f, _, err = scanRepo(t.Context(), scanOptions{Repo: repo, NoCluster: true, Draft: true, Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range f.Candidates {
		ids[c.ID] = true
	}
	if !ids["api"] || !ids["reports"] || ids["preview"] {
		t.Errorf("production is the base and its overlay, not staging: %v", ids)
	}
	if _, _, err := scanRepo(t.Context(), scanOptions{Repo: repo, NoCluster: true, Environment: "qa"}); err == nil || !strings.Contains(err.Error(), "production") {
		t.Errorf("an unknown environment lists the known ones: %v", err)
	}
}

func TestAccessGrantsTheMetricsPagesWhereTheyAreRead(t *testing.T) {
	b := model.Bindings{Edges: map[string][]model.ProbeSpec{"router->api": {{
		"probe": "k8s.scrape", "namespace": "edge", "selector": "app=router", "port": 9100, "metric": "router_requests_total",
	}}}}
	p := buildAccess(b, "wassup", "default", "8h", false)
	if got := strings.Join(p.ScrapeNamespaces, ","); got != "edge" {
		t.Errorf("metrics pages in %q, want the namespace the binding names", got)
	}
	for _, r := range p.Rules {
		for _, res := range r.Resources {
			if res == "pods/proxy" {
				t.Errorf("the whole cluster is given pods/proxy: %+v", r)
			}
		}
	}
	for _, want := range []string{"name: wassup-scrape\n  namespace: edge", `resources: ["pods/proxy"]` + "\n    verbs: [\"get\"]"} {
		if !strings.Contains(p.Manifest, want) {
			t.Errorf("the manifest lacks %q:\n%s", want, p.Manifest)
		}
	}
}
