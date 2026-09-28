package discover

import (
	"strconv"
	"strings"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

// AddCluster merges what the live cluster says into the findings: nodes,
// workloads with their env and commands, services (addresses), ingresses,
// CNPG clusters and cron jobs. Repository evidence and cluster evidence
// about the same object end up on one candidate.
func AddCluster(f *Findings, inv *k8s.Inventory) {
	if inv == nil {
		return
	}
	a := &accumulator{root: f.Root, f: f, byID: map[string]int{}}
	for i, c := range f.Candidates {
		a.byID[c.ID] = i
	}
	f.Cluster = &ClusterHint{Context: inv.Context, Namespaces: inv.Namespaces}
	ev := func(note string) Evidence { return Evidence{Source: "cluster", Note: note} }
	for _, n := range inv.Nodes {
		a.add(Candidate{ID: model.SlugifyID(n.Name), Type: "node", Label: n.Name, Name: n.Name, Evidence: []Evidence{ev("node " + n.Name)}})
	}
	for _, s := range inv.Services {
		target := selectorValue(s.Selector, "app")
		if target == "" {
			target = selectorValue(s.Selector, "app.kubernetes.io/name")
		}
		if target == "" {
			target = s.Name
		}
		c := Candidate{ID: model.SlugifyID(target), Type: "custom", Label: labelFor(target), Namespace: s.Namespace, Addresses: serviceAddresses(s.Name, s.Namespace), Evidence: []Evidence{ev("Service " + s.Namespace + "/" + s.Name)}}
		a.add(c)
	}
	for _, w := range inv.Workloads {
		typ := "workload"
		img := ""
		if len(w.Images) > 0 {
			img = w.Images[0]
		}
		if t := typeForImage(img, ""); t != "" {
			typ = t
		}
		c := Candidate{ID: model.SlugifyID(w.Name), Type: typ, Label: labelFor(w.Name), Namespace: w.Namespace, Kind: w.Kind, Name: w.Name, Selector: w.Selector, Image: img,
			RunsOn: slugs(w.Nodes), Env: map[string]string{}, Extra: map[string]string{}, Evidence: []Evidence{ev(w.Kind + " " + w.Namespace + "/" + w.Name)}}
		if l := labelForImage(img); l != "" && typ != "workload" {
			c.Label = l
		}
		for _, e := range w.Env {
			v := e.Value
			if e.SecretRef != "" {
				v = "secret:" + e.SecretRef
			} else if e.ConfigRef != "" {
				v = "configmap:" + e.ConfigRef
			}
			c.Env[e.Name] = v
		}
		if len(w.Command) > 0 {
			c.Extra["command"] = strings.Join(w.Command, " ; ")
		}
		added := a.add(c)
		if added != nil {
			envLinks(a, added, "", 0)
			commandLinks(a, added, w.Command, "", 0)
		}
	}
	for _, cj := range inv.CronJobs {
		c := Candidate{ID: model.SlugifyID(cj.Name), Type: "job", Label: labelFor(cj.Name), Namespace: cj.Namespace, Kind: "CronJob", Name: cj.Name, Extra: map[string]string{"schedule": cj.Schedule}, Evidence: []Evidence{ev("CronJob " + cj.Namespace + "/" + cj.Name)}}
		a.add(c)
	}
	for _, ing := range inv.Ingresses {
		c := Candidate{ID: "ingress", Type: "ingress", Label: "Ingress", Namespace: ing.Namespace, Name: ing.Name, Extra: map[string]string{"hosts": strings.Join(ing.Hosts, ",")}, Evidence: []Evidence{ev("Ingress " + ing.Namespace + "/" + ing.Name)}}
		added := a.add(c)
		for _, b := range ing.Backends {
			a.link(Link{From: added.ID, Host: b + "." + ing.Namespace, Kind: "http", Evidence: []Evidence{ev("Ingress " + ing.Name + " routes to " + b)}})
		}
		for _, h := range ing.Hosts {
			dns := Candidate{ID: model.SlugifyID(h), Type: "dns", Label: h, Addresses: []string{h}, Evidence: []Evidence{ev("Ingress host")}}
			a.add(dns)
			a.link(Link{From: dns.ID, To: added.ID, Kind: "tcp", Evidence: []Evidence{ev("host " + h + " served by Ingress " + ing.Name)}})
		}
	}
	for _, cl := range inv.CNPGClusters {
		roles := &model.Roles{Primary: model.SlugifyID(cl.Name) + "-primary"}
		for i := 1; i < int(cl.Instances); i++ {
			roles.Replicas = append(roles.Replicas, model.SlugifyID(cl.Name)+"-r"+itoa(i))
		}
		c := Candidate{ID: model.SlugifyID(cl.Name), Type: "db", Label: labelFor(cl.Name), Engine: "postgres", Namespace: cl.Namespace, Name: cl.Name, Roles: roles,
			Extra: map[string]string{"cnpg": "true", "primary_pod": cl.Primary}, Evidence: []Evidence{ev("CNPG Cluster " + cl.Namespace + "/" + cl.Name + " (" + itoa(int(cl.Instances)) + " instances)")}}
		for _, suffix := range []string{"-rw", "-ro", "-r", ""} {
			c.Addresses = append(c.Addresses, serviceAddresses(cl.Name+suffix, cl.Namespace)...)
		}
		a.add(c)
	}
	a.finish()
}

// selectorValue reads one key out of "a=b,c=d".
func selectorValue(sel, key string) string {
	for _, part := range strings.Split(sel, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k == key {
			return v
		}
	}
	return ""
}

func slugs(in []string) []string {
	var out []string
	for _, s := range in {
		out = append(out, model.SlugifyID(s))
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }
