package discover

import (
	"strconv"
	"strings"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe/k8s"
)

// AddCluster merges what the live cluster says into the findings: nodes,
// workloads with their env and commands, services (addresses), ingresses,
// CNPG clusters, cron jobs and the egress rules of the network policies.
// What the cluster would not let be read is said in the notes. Repository evidence and cluster evidence
// about the same object end up on one candidate. The inventory holds no
// credential (k8s.Discover reduces every value it reads), and nothing here
// adds one.
func AddCluster(f *Findings, inv *k8s.Inventory) {
	if inv == nil {
		return
	}
	a := newAccumulator(f)
	f.Cluster = &ClusterHint{Context: inv.Context, Namespaces: inv.Namespaces}
	ev := func(note string) Evidence { return Evidence{Source: "cluster", Note: note} }
	for _, n := range inv.Nodes {
		c := Candidate{ID: model.SlugifyID(n.Name), Type: "node", Label: n.Name, Name: n.Name, Labels: n.Labels, Extra: map[string]string{}, Evidence: []Evidence{ev("node " + n.Name)}}
		if len(n.Taints) > 0 {
			c.Extra["taints"] = strings.Join(n.Taints, ",")
		}
		a.add(c)
	}
	type live struct {
		id   string
		env  map[string]string
		cmds []string
	}
	var workloads []live
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
			RunsOn: slugs(w.Nodes), Labels: w.Labels, Env: map[string]string{}, Extra: map[string]string{}, Evidence: []Evidence{ev(w.Kind + " " + w.Namespace + "/" + w.Name)}}
		if l := labelForImage(img); l != "" && typ != "workload" {
			c.Label = l
		}
		if w.Release != "" {
			c.Extra["release"] = w.Release
		}
		var from []string
		for _, e := range w.Env {
			ref := ""
			switch {
			case e.SecretRef != "":
				ref = "secret:" + e.SecretRef
			case e.ConfigRef != "":
				ref = "configmap:" + e.ConfigRef
			}
			if e.Name == "*" { // an envFrom that could not be listed
				from = append(from, strings.TrimSuffix(ref, "/*"))
				continue
			}
			// The inventory already reduced the value; reducing is
			// idempotent, and doing it again keeps this the only door.
			c.Env[e.Name] = firstNonEmpty(envValue(e.Name, e.Value), ref)
		}
		if len(from) > 0 {
			c.Extra["env_from"] = strings.Join(uniq(from), " ")
		}
		if len(w.Command) > 0 {
			c.Extra["command"] = strings.Join(w.Command, " ; ")
		}
		if added := a.add(c); added != nil {
			// What the cluster says about the environment is newer than
			// what the repository says.
			for k, v := range c.Env {
				if literalEnv(v) {
					added.Env[k] = v
				}
			}
			workloads = append(workloads, live{added.ID, c.Env, w.Command})
		}
	}
	for _, s := range inv.Services {
		a.services = append(a.services, pendingService{name: s.Name, ns: s.Namespace, selector: selectorMap(s.Selector), ev: ev("Service " + s.Namespace + "/" + s.Name)})
	}
	a.attachServices()
	for _, w := range workloads {
		a.deriveLive(w.id, w.env, w.cmds)
	}
	for _, cj := range inv.CronJobs {
		c := Candidate{ID: model.SlugifyID(cj.Name), Type: "scheduledjob", Label: labelFor(cj.Name), Namespace: cj.Namespace, Kind: "CronJob", Name: cj.Name, Extra: map[string]string{"schedule": cj.Schedule}, Evidence: []Evidence{ev("CronJob " + cj.Namespace + "/" + cj.Name)}}
		a.add(c)
	}
	for _, ing := range inv.Ingresses {
		addIngress(a, ingressObject{name: ing.Name, ns: ing.Namespace, hosts: ing.Hosts, services: ing.Services, ev: ev("Ingress " + ing.Namespace + "/" + ing.Name)})
	}
	for _, cl := range inv.CNPGClusters {
		roles := &model.Roles{Primary: model.SlugifyID(cl.Name) + "-primary"}
		for i := 1; i < int(cl.Instances); i++ {
			roles.Replicas = append(roles.Replicas, model.SlugifyID(cl.Name)+"-r"+itoa(i))
		}
		c := Candidate{ID: model.SlugifyID(cl.Name), Type: "database", Label: labelFor(cl.Name), Engine: "postgres", Namespace: cl.Namespace, Name: cl.Name, Roles: roles,
			Extra: map[string]string{"cnpg": "true", "primary_pod": cl.Primary}, Evidence: []Evidence{ev("CNPG Cluster " + cl.Namespace + "/" + cl.Name + " (" + itoa(int(cl.Instances)) + " instances)")}}
		for _, suffix := range []string{"-rw", "-ro", "-r", ""} {
			c.Addresses = append(c.Addresses, serviceAddresses(cl.Name+suffix, cl.Namespace)...)
		}
		a.add(c)
	}
	for _, np := range inv.NetworkPolicies {
		a.policy(EgressPolicy{NetworkPolicyInfo: np, Source: "cluster"})
	}
	for _, w := range inv.Warnings {
		a.note("the cluster did not answer for %s", w)
	}
	a.finish()
}

// selectorMap reads "a=b,c=d".
func selectorMap(sel string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(sel, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
			out[k] = v
		}
	}
	return out
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
