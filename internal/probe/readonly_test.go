package probe

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReadOnlySendsReadsAndNothingElse(t *testing.T) {
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { seen.Add(1) }))
	defer srv.Close()
	client := &http.Client{Transport: ReadOnly(nil)}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, _ := http.NewRequest(method, srv.URL+"/queues", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
	}
	if seen.Load() != 2 {
		t.Fatalf("the server saw %d requests, want the 2 reads", seen.Load())
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, "PURGE"} {
		body := &closeRecorder{Reader: strings.NewReader("{}")}
		req, _ := http.NewRequest(method, srv.URL+"/queues/orders", body)
		_, err := client.Do(req)
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrReadOnly", method, err)
		}
		if !body.closed {
			t.Errorf("%s: the body of the refused request was left open", method)
		}
	}
	if seen.Load() != 2 {
		t.Errorf("the server saw %d requests, a refused one was sent", seen.Load())
	}
}

func TestReadOnlyExceptSendsWhatIsAllowed(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { paths = append(paths, r.URL.Path) }))
	defer srv.Close()
	client := &http.Client{Transport: ReadOnlyExcept(nil, func(r *http.Request) bool { return r.URL.Path == "/open" })}

	resp, err := client.Post(srv.URL+"/open", "text/plain", nil)
	if err != nil {
		t.Fatalf("the allowed request: %v", err)
	}
	resp.Body.Close()
	if _, err := client.Post(srv.URL+"/other", "text/plain", nil); !errors.Is(err, ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
	if len(paths) != 1 || paths[0] != "/open" {
		t.Errorf("the server saw %v, want only /open", paths)
	}
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// The rules below are read from the source, so a change that gives wassup a
// way to write fails here before it runs anywhere. Each names the one place
// that holds the guard; the guards themselves are tested where they live.

// onlyIn lists, per import, the directory that may use it. A database or a
// cluster is reached through the package that holds its guard and through
// no other.
var onlyIn = map[string]string{
	"k8s.io/":                        "internal/probe/k8s/",
	"github.com/jackc/pgx/":          "internal/probe/pgprobe/",
	"github.com/redis/go-redis/":     "internal/probe/redisprobe/",
	"database/sql":                   "",
	"sigs.k8s.io/controller-runtime": "",
}

// clusterWrites are the calls of the Kubernetes clients that change the
// cluster.
var clusterWrites = set("Create", "Update", "UpdateStatus", "Patch", "Delete", "DeleteCollection",
	"Apply", "ApplyStatus", "Evict", "EvictV1", "EvictV1beta1", "Bind", "UpdateScale", "ApplyScale",
	"UpdateEphemeralContainers", "UpdateResize", "CreateToken", "Put", "Verb")

// calls lists the calls that are made in one place only: the file that puts
// the guard around them.
var calls = []struct {
	dir   string // where the rule applies; "" is everywhere
	names map[string]bool
	files map[string]bool // the files that may
	why   string
}{
	{"internal/probe/k8s/", clusterWrites, nil,
		"it changes the cluster"},
	{"internal/probe/k8s/", set("Post"), set("internal/probe/k8s/portforward.go"),
		"only the port-forward is opened with a POST"},
	{"internal/probe/k8s/", set("NewForConfig", "NewForConfigOrDie", "InClusterConfig", "RESTClientFor", "ClientConfig"),
		set("internal/probe/k8s/clients.go"),
		"a client is built from the configuration that went through tuneConfig, which makes it read only"},
	{"internal/probe/pgprobe/", set("Exec", "SendBatch", "CopyFrom", "Prepare", "Begin", "BeginTx", "LargeObjects"),
		set("internal/probe/pgprobe/readonly.go"),
		"statements are sent through reads, in a read-only transaction"},
	{"internal/probe/redisprobe/", set("NewClient", "NewClusterClient", "NewFailoverClient", "NewFailoverClusterClient", "NewUniversalClient", "NewRing"),
		set("internal/probe/redisprobe/client.go"),
		"a client is built by newClient, which adds the hook that refuses what does not read"},
	{"", set("Command", "CommandContext"),
		set("internal/render/export.go", "internal/render/clipboard.go",
			"internal/probe/terraform/probe.go", "internal/probe/gitevents/gitevents.go"),
		"a program wassup starts can do what wassup must not; add the file here once the command is known to read only"},
}

// httpWrites are the names of net/http that send something other than a
// read, and the client that carries no guard.
var httpWrites = set("MethodPost", "MethodPut", "MethodPatch", "MethodDelete",
	"Post", "PostForm", "Get", "Head", "DefaultClient")

// readsWithPost are the files that name a POST: the two that open a
// port-forward, and the client of SigNoz, which signs in and asks with one.
// Each builds its client on ReadOnlyExcept and lets through the requests it
// names and no other.
var readsWithPost = set("internal/probe/k8s/portforward.go", "internal/probe/k8s/clients.go",
	"internal/probe/signoz/client.go")

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestTheSourceHasNoWayToWrite(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	files := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				// A file that does not parse is somebody's work in progress
				// and fails the build, not this rule.
				t.Logf("%s: not checked: %v", rel, err)
				return nil
			}
			files++
			for _, v := range violations(fset, rel, f) {
				t.Error(v)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 50 {
		t.Fatalf("only %d files were checked: the source was not found", files)
	}
}

// The rules find what they are there for: each of these files has one way
// to write, and the last one has none.
func TestTheRulesFindAWrite(t *testing.T) {
	cases := []struct {
		name, rel, src string
		want           string // part of the violation; "" is none
	}{
		{"a delete in the cluster", "internal/probe/k8s/workload.go",
			`package k8s
func f() { c.Core.CoreV1().Pods(ns).Delete(ctx, name, opts) }`, "calls Delete"},
		{"a patch through the dynamic client", "internal/probe/k8s/cnpg.go",
			`package k8s
func f() { c.Dynamic.Resource(gvr).Namespace(ns).Patch(ctx, name, pt, data, opts) }`, "calls Patch"},
		{"a client of its own", "internal/probe/k8s/node.go",
			`package k8s
func f() { kubernetes.NewForConfig(cfg) }`, "calls NewForConfig"},
		{"the cluster from another package", "internal/app/runtime.go",
			`package app
import "k8s.io/client-go/kubernetes"`, "imports k8s.io/client-go/kubernetes"},
		{"a statement that is executed", "internal/probe/pgprobe/stats.go",
			`package pgprobe
func f() { conn.Exec(ctx, "VACUUM") }`, "calls Exec"},
		{"another database driver", "internal/probe/mysqlprobe/stats.go",
			`package mysqlprobe
import "database/sql"`, "imports database/sql"},
		{"a redis client without the hook", "internal/probe/redisprobe/list.go",
			`package redisprobe
func f() { redis.NewClient(opt) }`, "calls NewClient"},
		{"a post", "internal/probe/amqp/queue.go",
			`package amqp
func f() { http.NewRequest(http.MethodPost, u, nil) }`, "http.MethodPost"},
		{"a method by its name", "internal/probe/amqp/queue.go",
			`package amqp
func f() { http.NewRequest("DELETE", u, nil) }`, "the method DELETE"},
		{"a client without the guard", "internal/probe/hatchet/hatchet.go",
			`package hatchet
var c = &http.Client{Timeout: t}`, "http.Client without"},
		{"the default client", "internal/probe/hatchet/hatchet.go",
			`package hatchet
func f() { http.DefaultClient.Do(req) }`, "http.DefaultClient"},
		{"a program", "cmd/wassup/commands.go",
			`package main
func f() { exec.Command("kubectl", "apply", "-f", path) }`, "calls Command"},
		{"a post beside the client that guards it", "internal/probe/signoz/edge.go",
			`package signoz
func f() { http.NewRequest(http.MethodPost, u, nil) }`, "http.MethodPost"},
		{"the query of a source that answers to a post", "internal/probe/signoz/client.go",
			`package signoz
var c = &http.Client{Timeout: t, Transport: probe.ReadOnlyExcept(nil, reads)}
func f() { http.NewRequest(http.MethodPost, u, body) }`, ""},
		{"a probe that reads", "internal/probe/amqp/queue.go",
			`package amqp
var c = &http.Client{Timeout: t, Transport: probe.ReadOnly(tr)}
func f() { http.NewRequest(http.MethodGet, u, nil); pods.Pods(ns).List(sel); m.Delete(k) }`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, c.rel, c.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			got := violations(fset, c.rel, f)
			switch {
			case c.want == "" && len(got) > 0:
				t.Errorf("found %v, want nothing", got)
			case c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want)):
				t.Errorf("found %v, want one violation with %q", got, c.want)
			}
		})
	}
}

// violations applies the rules to one file.
func violations(fset *token.FileSet, rel string, f *ast.File) []string {
	var out []string
	t := reporter{&out}
	at := func(n ast.Node) string { return rel + ":" + strconv.Itoa(fset.Position(n.Pos()).Line) }

	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		for prefix, dir := range onlyIn {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			if dir == "" {
				t.Errorf("%s imports %s: nothing reaches a database or a cluster with it, write a probe with a guard of its own", at(imp), path)
			} else if !strings.HasPrefix(rel, dir) {
				t.Errorf("%s imports %s: only %s may, it holds the guard that keeps it read only", at(imp), path, dir)
			}
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			for _, rule := range calls {
				if strings.HasPrefix(rel, rule.dir) && rule.names[sel.Sel.Name] && !rule.files[rel] && isPackageOrClient(sel, rule.dir) {
					t.Errorf("%s calls %s: %s", at(x), sel.Sel.Name, rule.why)
				}
			}
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "http" && httpWrites[x.Sel.Name] {
				if x.Sel.Name == "MethodPost" && readsWithPost[rel] {
					return true
				}
				t.Errorf("%s uses http.%s: a probe sends GET and HEAD, through a client built on probe.ReadOnly", at(x), x.Sel.Name)
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				switch s, _ := strconv.Unquote(x.Value); s {
				case "POST", "PUT", "PATCH", "DELETE":
					t.Errorf("%s names the method %s: a probe sends GET and HEAD", at(x), s)
				}
			}
		case *ast.CompositeLit:
			if isHTTPClient(x.Type) && rel != "internal/probe/k8s/portforward.go" && !guarded(x) {
				t.Errorf("%s builds an http.Client without Transport: probe.ReadOnly(...)", at(x))
			}
		}
		return true
	})
	return out
}

// reporter collects the violations of one file.
type reporter struct{ out *[]string }

// Errorf records a violation.
func (r reporter) Errorf(format string, args ...any) {
	*r.out = append(*r.out, fmt.Sprintf(format, args...))
}

// isPackageOrClient narrows the rule about starting programs to the exec
// package: Command is a common name. The other rules apply to every call of
// that name in their directory.
func isPackageOrClient(sel *ast.SelectorExpr, dir string) bool {
	if dir != "" {
		return true
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "exec"
}

// isHTTPClient reports whether a composite literal is an http.Client.
func isHTTPClient(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Client" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

// guarded reports whether an http.Client literal has
// Transport: probe.ReadOnly(...) or probe.ReadOnlyExcept(...).
func guarded(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Transport" {
			continue
		}
		call, ok := kv.Value.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "probe" && strings.HasPrefix(sel.Sel.Name, "ReadOnly")
	}
	return false
}
