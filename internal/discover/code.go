package discover

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/danilopopovikj/wassup/internal/model"
)

// codeExt lists source files the code scanner reads.
var codeExt = map[string]bool{".py": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".go": true, ".rb": true, ".java": true, ".kt": true, ".rs": true, ".ex": true, ".exs": true, ".php": true, ".cs": true, ".toml": true, ".ini": true, ".cfg": true, ".json": true}

var (
	celeryTaskQueueRe = regexp.MustCompile(`queue\s*=\s*["']([A-Za-z0-9_.-]+)["']`)
	celeryAppRe       = regexp.MustCompile(`\bCelery\s*\(`)
	celeryRouteRe     = regexp.MustCompile(`["']queue["']\s*:\s*["']([A-Za-z0-9_.-]+)["']`)
	hatchetWorkflowRe = regexp.MustCompile(`(?:@hatchet\.(?:workflow|task|durable_task)|hatchet\.(?:workflow|task)\(|Workflow\(|new Hatchet\(|hatchet\.worker\()`)
	hatchetNameRe     = regexp.MustCompile(`(?:workflow|task|durable_task)\(\s*name\s*=\s*["']([A-Za-z0-9_.:-]+)["']`)
	hatchetOnCronRe   = regexp.MustCompile(`on_crons\s*=\s*\[\s*["']([^"']+)["']`)
	electricShapeRe   = regexp.MustCompile(`(?:/v1/shape\?[^"'` + "`" + `\s]*table=|useShape\([^)]*table:\s*|ShapeStream\([^)]*table:\s*|params:\s*\{\s*table:\s*)["']?([A-Za-z0-9_."]+)`)
	externalURLRe     = regexp.MustCompile(`https?://([a-z0-9.-]+\.(?:com|io|net|org|dev|app|ai|co|cloud|run|sh))(?:[/:]|\b)`)
	bucketNameRe      = regexp.MustCompile(`(?i)\b(?:bucket|bucket_name|bucketName)\s*[=:]\s*["']([a-z0-9][a-z0-9.-]{1,61}[a-z0-9])["']`)
	objectClientRe    = regexp.MustCompile(`boto3\.(?:client|resource)\(\s*["']s3["']|new S3Client\(|\bMinio\(|from ["']@aws-sdk/client-s3["']|s3fs\.S3FileSystem\(|minio\.New\(`)
	envRefRe          = regexp.MustCompile(`(?:os\.environ(?:\.get)?\(|os\.getenv\(|process\.env\.|env\(|ENV\[|System\.getenv\()\s*["']?([A-Z][A-Z0-9_]{3,})`)
)

// externalSkip lists hosts that are documentation or build-time, not runtime dependencies.
var externalSkip = map[string]bool{"github.com": false, "www.w3.org": true, "schema.org": true, "json-schema.org": true, "registry.npmjs.org": true, "pypi.org": true, "files.pythonhosted.org": true, "proxy.golang.org": true, "sum.golang.org": true, "fonts.googleapis.com": true, "fonts.gstatic.com": true, "cdn.jsdelivr.net": true, "cdnjs.cloudflare.com": true, "unpkg.com": true, "example.com": true, "localhost": true, "docs.hatchet.run": true, "electric-sql.com": true, "kubernetes.io": true, "hub.docker.com": true, "ghcr.io": true, "docker.io": true, "quay.io": true, "goreleaser.com": true, "golang.org": true, "pkg.go.dev": true, "developer.mozilla.org": true, "stackoverflow.com": true, "reactjs.org": true, "nextjs.org": true, "vercel.com": true, "tailwindcss.com": true}

// scanCode reads a source file for queue names, workflows, shapes and
// external hosts. Results attach to the "code" pseudo candidate of the
// service they belong to only when the file lives under a directory named
// like a known workload; otherwise they land on the repo-level notes.
func scanCode(a *accumulator, path, rel string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() > 1<<20 {
		return
	}
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer fh.Close()
	owner := ownerFor(rel)
	// Celery is a Python library. Its patterns are plain enough
	// ("queue": "name") to match a dependency list in any other file.
	python := strings.EqualFold(filepath.Ext(path), ".py")
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<21)
	line := 0
	seenHosts := map[string]bool{}
	for sc.Scan() {
		line++
		text := sc.Text()
		ev := func(note string) Evidence { return Evidence{Source: "code", File: rel, Line: line, Note: note} }
		if python && celeryAppRe.MatchString(text) {
			a.note("celery app in %s:%d", rel, line)
		}
		for _, m := range celeryMatches(python, celeryTaskQueueRe, text) {
			q := m[1]
			qc := Candidate{ID: model.SlugifyID(q + "-queue"), Type: "queue", Label: labelFor(q) + " queue", Name: q, Extra: map[string]string{"celery": "true", "queue": q}, Evidence: []Evidence{ev("celery task routed to queue " + q)}}
			a.add(qc)
			if owner != "" {
				a.link(Link{From: owner, To: qc.ID, Kind: "queue", Evidence: []Evidence{ev(owner + " enqueues to " + q)}})
			}
		}
		for _, m := range celeryMatches(python, celeryRouteRe, text) {
			q := m[1]
			a.add(Candidate{ID: model.SlugifyID(q + "-queue"), Type: "queue", Label: labelFor(q) + " queue", Name: q, Extra: map[string]string{"celery": "true", "queue": q}, Evidence: []Evidence{ev("celery task_routes queue " + q)}})
		}
		if hatchetWorkflowRe.MatchString(text) {
			name := ""
			if m := hatchetNameRe.FindStringSubmatch(text); m != nil {
				name = m[1]
			}
			if name != "" {
				jc := Candidate{ID: model.SlugifyID(name), Type: "scheduledjob", Label: labelFor(name), Name: name, Extra: map[string]string{"hatchet_workflow": name}, Evidence: []Evidence{ev("hatchet workflow " + name)}}
				a.add(jc)
			} else {
				a.note("hatchet client or workflow in %s:%d (name not on this line)", rel, line)
			}
		}
		if m := hatchetOnCronRe.FindStringSubmatch(text); m != nil {
			a.note("hatchet cron %q in %s:%d", m[1], rel, line)
		}
		for _, m := range electricShapeRe.FindAllStringSubmatch(text, -1) {
			table := strings.Trim(m[1], `"`)
			a.note("electric shape on table %s in %s:%d", table, rel, line)
			c := a.add(Candidate{ID: "electric", Type: "syncengine", Label: "Electric", Extra: map[string]string{}, Evidence: []Evidence{ev("shape on " + table)}})
			if c != nil {
				if c.Extra == nil {
					c.Extra = map[string]string{}
				}
				c.Extra["tables"] = strings.TrimSpace(c.Extra["tables"] + " " + table)
			}
			if owner != "" {
				a.link(Link{From: owner, To: "electric", Kind: "http", Label: "shapes", Evidence: []Evidence{ev(owner + " reads shape " + table)}})
			}
		}
		if objectClientRe.MatchString(text) {
			a.note("object storage client in %s:%d", rel, line)
		}
		for _, m := range bucketNameRe.FindAllStringSubmatch(text, -1) {
			name := strings.ToLower(m[1])
			bc := Candidate{ID: bucketID(name), Type: "storage", Label: name + " bucket", Name: name, Extra: map[string]string{"bucket": name}, Evidence: []Evidence{ev("bucket " + name)}}
			a.add(bc)
			if owner != "" {
				a.link(Link{From: owner, To: bc.ID, Kind: "tcp", Label: "objects", Evidence: []Evidence{ev(owner + " uses bucket " + name)}})
			}
		}
		for _, m := range externalURLRe.FindAllStringSubmatch(text, -1) {
			host := strings.ToLower(m[1])
			if externalSkip[host] || seenHosts[host] || strings.HasPrefix(host, "www.") || !usableHost(host) {
				continue // documentation, a sample domain, a tunnel to a laptop
			}
			if !strings.HasPrefix(host, "api.") && !strings.Contains(host, ".api.") && !strings.HasPrefix(host, "hooks.") && !strings.HasPrefix(host, "sqs.") && !strings.HasPrefix(host, "storage.") {
				continue // links and docs, not dependencies
			}
			seenHosts[host] = true
			from := owner
			if from == "" {
				from = "repo"
			}
			a.link(Link{From: from, Host: host, Kind: "external", Evidence: []Evidence{ev("calls " + host)}})
		}
	}
}

// celeryMatches applies a celery pattern to a line of a Python file, and to
// nothing else.
func celeryMatches(python bool, re *regexp.Regexp, text string) [][]string {
	if !python {
		return nil
	}
	return re.FindAllStringSubmatch(text, -1)
}

// ownerFor guesses which workload a source file belongs to from its path:
// apps/api/..., services/worker/..., cmd/api/...
func ownerFor(rel string) string {
	parts := strings.Split(rel, string(os.PathSeparator))
	for i, p := range parts[:max(0, len(parts)-1)] {
		switch p {
		case "apps", "services", "cmd", "packages", "src", "internal":
			if i+1 < len(parts)-1 {
				return model.SlugifyID(parts[i+1])
			}
		}
	}
	return ""
}

// scanDotenv reads .env style files for DSNs (values are often placeholders
// in .env.example, which is fine: hosts are what we need).
func scanDotenv(a *accumulator, path, rel string) {
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		k, v, ok := strings.Cut(text, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		for _, d := range dsnRe.FindAllString(v, -1) {
			if h, ok := parseDSN(d); ok {
				a.link(Link{From: "repo", Host: h.Host, Kind: edgeKindFor(h.Scheme, ""), Evidence: []Evidence{{Source: "dotenv", File: rel, Line: line, Note: strings.TrimSpace(k)}}})
			}
		}
	}
}
