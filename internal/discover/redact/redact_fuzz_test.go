package redact

import (
	"strings"
	"testing"
)

// FuzzPassword puts any text where a password goes, in a URL, in its query,
// in an argument and under a credential's name, and checks that what is
// kept is the host or nothing.
func FuzzPassword(f *testing.F) {
	for _, seed := range []string{"//", "abcdef://x", "12345 http://zzzzzz", "s3cr3t", "p@ss/w0rd", "a b c", "12345 x", "x?y#z", "pa:ss", "%zz", "päss", "$(OTHER)", "a,b", "'quoted one'", "--flag", "k=v", "x@y.example"} {
		f.Add(seed)
	}
	const host = "postgresql://db.bookstore:5432"
	f.Fuzz(func(t *testing.T, pw string) {
		if pw == "" || strings.ContainsAny(pw, "\n\r") {
			t.Skip()
		}
		for name, got := range map[string]string{
			"user and password": EnvValue("DATABASE_URL", "postgresql://admin:"+pw+"@db.bookstore:5432/app"),
			"query":             EnvValue("DATABASE_URL", "postgresql://db.bookstore:5432/app?user=admin&password="+pw),
		} {
			if got != "" && got != host {
				t.Errorf("%s: %q kept %q", name, pw, got)
			}
		}
		url := "postgresql://admin:" + pw + "@db.bookstore:5432/app"
		for i, got := range Args([]string{url, "--target=" + url, "--dsn=" + url, "--dsn", url}) {
			want := []string{host, "--target=" + host, "--dsn=" + Mask, "--dsn", Mask}[i]
			if alt := strings.Replace(want, host, Mask, 1); got != want && got != alt {
				t.Errorf("argument %d: %q kept %q", i, pw, got)
			}
		}
		// in a line, where a blank ends a word, a password without one
		if !strings.ContainsAny(pw, urlEnd) {
			if got := Text("migrate --url " + url + " --verbose"); got != "migrate --url "+host+" --verbose" && got != "migrate --url "+Mask+" --verbose" {
				t.Errorf("text: %q kept in %q", pw, got)
			}
		}
		if got := EnvValue("DB_PASSWORD", pw); got != "" {
			t.Errorf("a password was kept under its own name: %q -> %q", pw, got)
		}
		args := Args([]string{"serve", "--password", pw, "--token=" + pw, "DB_PASSWORD=" + pw, "--port", "8000"})
		if len(args) != 7 || args[2] != Mask || args[3] != "--token="+Mask || args[4] != "DB_PASSWORD="+Mask || args[6] != "8000" {
			t.Errorf("arguments with %q: %q", pw, args)
		}
	})
}
