package measure

import (
	"strings"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
	"github.com/danilopopovikj/wassup/internal/probe/signoz"
)

// bookstore is a small shop: two hosts in front of a router, an API and a
// storefront behind it, a worker, a database and three services of others.
func bookstore() *model.Config {
	return &model.Config{
		Topology: model.Topology{
			Components: []model.Component{
				{ID: "dns-shop", Type: "dns", Label: "shop.example"},
				{ID: "dns-api", Type: "dns", Label: "api.example"},
				{ID: "lb", Type: "loadbalancer"},
				{ID: "ingress", Type: "ingress"},
				{ID: "web", Type: "workload", Label: "Storefront"},
				{ID: "api", Type: "workload", Label: "API"},
				{ID: "worker", Type: "backgroundworker", Label: "Workers"},
				{ID: "db", Type: "database", Label: "Database"},
				{ID: "payments", Type: "external", Label: "Payments"},
				{ID: "mail", Type: "external", Label: "Mail"},
				{ID: "maps", Type: "external", Label: "Maps"},
				{ID: "nightly", Type: "scheduledjob", Label: "Nightly"},
			},
			Edges: []model.Edge{
				{From: "dns-shop", To: "lb", Kind: "http"},
				{From: "dns-api", To: "lb", Kind: "http"},
				{From: "ingress", To: "web", Kind: "http"},
				{From: "ingress", To: "api", Kind: "http"},
				{From: "api", To: "db", Kind: "sql"},
				{From: "api", To: "payments", Kind: "external"},
				{From: "api", To: "mail", Kind: "external"},
				{From: "worker", To: "mail", Kind: "external"},
				{From: "api", To: "maps", Kind: "external"},
				{From: "nightly", To: "worker", Kind: "queue"},
			},
		},
		Bindings: model.Bindings{
			Components: map[string][]model.ProbeSpec{
				"dns-shop": {{"probe": "dns.record", "host": "shop.example"}},
				"dns-api":  {{"probe": "dns.record", "host": "api.example"}},
				"web":      {{"probe": "k8s.workload", "namespace": "shop", "selector": "app=web"}},
				"api":      {{"probe": "k8s.workload", "namespace": "shop", "selector": "app=api"}},
				"worker":   {{"probe": "k8s.workload", "namespace": "shop", "selector": "app=worker"}},
				"payments": {{"probe": "http.ping", "url": "https://api.payments.example/"}},
				"mail":     {{"probe": "http.ping", "url": "https://api.mail.example/"}},
				"maps":     {{"probe": "http.ping", "url": "https://maps.example/"}},
			},
			Edges: map[string][]model.ProbeSpec{
				// bound already, the way the loader hands a nested map over
				"api->payments": {{"probe": "signoz.edge", "url": "https://signoz.shop.example", "metric": "signoz_external_call_latency_count",
					"match": model.ProbeSpec{"service.name": "api", "address": `.*payments\.example.*`}}},
			},
		},
	}
}

func inventory() *k8s.Inventory {
	return &k8s.Inventory{
		Workloads: []k8s.WorkloadInfo{
			{Namespace: "shop", Name: "web", Labels: map[string]string{"app": "web"}},
			{Namespace: "shop", Name: "api", Labels: map[string]string{"app": "api"}},
			{Namespace: "shop", Name: "worker", Labels: map[string]string{"app": "worker"}},
		},
		Services: []k8s.ServiceInfo{
			{Namespace: "shop", Name: "web", Selector: "app=web"},
			{Namespace: "shop", Name: "api", Selector: "app=api"},
		},
		Ingresses: []k8s.IngressInfo{
			{Namespace: "shop", Name: "web", Hosts: []string{"shop.example"}, Backends: []string{"web:80 /", "api:8000 /api"}},
			{Namespace: "shop", Name: "api", Hosts: []string{"api.example"}, Backends: []string{"api:8000 /"}},
		},
	}
}

var last = time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)

func survey() *signoz.Survey {
	return &signoz.Survey{
		Lookback: 7 * 24 * time.Hour,
		External: []signoz.Call{
			{Service: "api", Address: "api.payments.example", Count: 900, Last: last},
			{Service: "api", Address: "smtp.mail.example:587", Count: 40, Last: last},
			{Service: "api", Address: "geo.lookup.example", Count: 12, Last: last},
		},
		Queries: []signoz.Call{{Service: "api", Count: 50000, Last: last}},
		Spans:   []signoz.Call{{Service: "api", Count: 90000, Last: last}, {Service: "billing", Count: 3, Last: last}},
		Quiet:   []string{"worker"},
	}
}

func sources() Sources {
	return Sources{
		Inventory: inventory(),
		Router: &Router{
			Spec:     model.ProbeSpec{"namespace": "edge", "selector": "app=traefik", "port": "9100"},
			Services: map[string]bool{"shop-web-80@kubernetes": true, "shop-api-8000@kubernetes": true},
		},
		Survey: survey(),
		SigNoz: model.ProbeSpec{"url": "https://signoz.shop.example", "user_env": "SIGNOZ_USER", "password_env": "SIGNOZ_PASSWORD"},
	}
}

func verdicts(rep Report) map[string]Edge {
	out := map[string]Edge{}
	for _, e := range rep.Edges {
		out[e.ID] = e
	}
	return out
}

func TestAnEdgeBoundAlreadyIsLeftAndItsCallsAreAccountedFor(t *testing.T) {
	rep := Plan(bookstore(), sources())
	if e := verdicts(rep)["api->payments"]; e.Status != Counted {
		t.Errorf("api->payments = %+v", e)
	}
	for _, u := range rep.Unplaced {
		if u.Address == "api.payments.example" {
			t.Errorf("a call a binding counts is listed as unplaced: %+v", u)
		}
	}
}

func TestTheCallsOfAServiceToAHostAreBound(t *testing.T) {
	e := verdicts(Plan(bookstore(), sources()))["api->mail"]
	if e.Status != Found || e.Binding.Kind() != "signoz.edge" {
		t.Fatalf("api->mail = %+v", e)
	}
	m := e.Binding["match"].(map[string]any)
	if m["service.name"] != "api" || e.Binding["url"] != "https://signoz.shop.example" || e.Binding["user_env"] != "SIGNOZ_USER" {
		t.Errorf("binding = %+v", e.Binding)
	}
	// the address of another name under the same domain is the service's
	if !strings.Contains(e.Evidence, "40 calls") || !strings.Contains(e.Evidence, "smtp.mail.example:587") {
		t.Errorf("evidence = %q", e.Evidence)
	}
	if e.Binding["window"] != "1h" {
		t.Errorf("a service called 40 times a week is read over an hour: %v", e.Binding["window"])
	}
}

func TestAWorkerThatSendsNoSpansSaysSo(t *testing.T) {
	e := verdicts(Plan(bookstore(), sources()))["worker->mail"]
	if e.Status != Missing || !strings.Contains(e.Reason, "metrics but no span") || e.Fix == "" {
		t.Errorf("worker->mail = %+v", e)
	}
}

func TestAHostThatSharesAServiceNeedsRouterLabels(t *testing.T) {
	v := verdicts(Plan(bookstore(), sources()))
	if e := v["dns-shop->lb"]; e.Status != Missing || !strings.Contains(e.Reason, "shares shop-api-8000 with api.example") || !strings.Contains(e.Fix, "addRoutersLabels") {
		t.Errorf("dns-shop->lb = %+v", e)
	}
	src := sources()
	src.Router.Routers = map[string]bool{"shop-web-shop-example@kubernetes": true, "shop-api-api-example@kubernetes": true}
	e := verdicts(Plan(bookstore(), src))["dns-shop->lb"]
	if e.Status != Found || e.Binding["metric"] != RouterRouterMetric {
		t.Fatalf("with router labels: %+v", e)
	}
	if got := e.Binding["match"].(map[string]any)["router"]; got != `.*-shop-example.*` {
		t.Errorf("match = %v", got)
	}
}

func TestTheRouterCountsTheServicesThatSelectAComponent(t *testing.T) {
	e := verdicts(Plan(bookstore(), sources()))["ingress->web"]
	if e.Status != Found || e.Binding.Kind() != "k8s.scrape" || e.Binding["namespace"] != "edge" {
		t.Fatalf("ingress->web = %+v", e)
	}
	if got := e.Binding["match"].(map[string]any)["service"]; got != `shop-web-80@kubernetes` {
		t.Errorf("match = %v", got)
	}
}

func TestQueriesAreBoundWhenTheCallerHasOneDatabase(t *testing.T) {
	e := verdicts(Plan(bookstore(), sources()))["api->db"]
	if e.Status != Found || e.Binding["metric"] != "signoz_db_latency_count" {
		t.Errorf("api->db = %+v", e)
	}
}

func TestWhatNoBoxStandsForIsListed(t *testing.T) {
	rep := Plan(bookstore(), sources())
	var got []string
	for _, u := range rep.Unplaced {
		got = append(got, u.Address+": "+u.Suggest)
	}
	want := "geo.lookup.example: add an external component for geo.lookup.example and the edge from api"
	if len(got) != 1 || got[0] != want {
		t.Errorf("unplaced = %v", got)
	}
	if strings.Join(rep.Unknown, ",") != "billing" {
		t.Errorf("unknown = %v", rep.Unknown)
	}
}

func TestAnEdgeFromAScheduledJobTakesItsRuns(t *testing.T) {
	if e := verdicts(Plan(bookstore(), sources()))["nightly->worker"]; e.Status != Skipped {
		t.Errorf("nightly->worker = %+v", e)
	}
}

func TestWithoutSourcesEveryEdgeSaysWhatIsMissing(t *testing.T) {
	for _, e := range Plan(bookstore(), Sources{}).Edges {
		if e.Status == Found {
			t.Errorf("%s found with no source", e.ID)
		}
		if e.Status == Missing && e.Reason == "" {
			t.Errorf("%s: missing without a reason", e.ID)
		}
	}
}

func TestDomain(t *testing.T) {
	for host, want := range map[string]string{
		"quickvin.carfax.com": "carfax.com", "api.example.co.uk": "example.co.uk", "openrouter.ai": "openrouter.ai", "vpic.nhtsa.dot.gov": "dot.gov",
	} {
		if got := Domain(host); got != want {
			t.Errorf("Domain(%s) = %s, want %s", host, got, want)
		}
	}
}
