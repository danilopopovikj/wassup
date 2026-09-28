package discover

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A small reader for .gitignore files. What git ignores is not part of the
// system: build output, local settings, editor state. It covers the patterns
// people write (comments, negation, directory-only, anchoring, *, ?, **,
// character classes) and leaves out what they do not (a global excludes
// file, attributes).

// ignoreRule is one line of a .gitignore file.
type ignoreRule struct {
	dir     string // absolute directory the pattern is relative to
	negate  bool   // the line began with !
	dirOnly bool   // the line ended with /
	re      *regexp.Regexp
}

// ignoreSet holds the rules that apply to a walk, the outermost file first.
// As in git, the last rule that matches a path decides.
type ignoreSet struct {
	rules []ignoreRule
}

// load reads an ignore file whose patterns are relative to dir. A missing
// file adds nothing.
func (s *ignoreSet) load(file, dir string) {
	fh, err := os.Open(file)
	if err != nil {
		return
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		if r, ok := parseIgnoreLine(sc.Text(), dir); ok {
			s.rules = append(s.rules, r)
		}
	}
}

// parseIgnoreLine turns one line into a rule; blank lines and comments are
// not rules.
func parseIgnoreLine(line, dir string) (ignoreRule, bool) {
	line = strings.TrimRight(line, "\r")
	// trailing spaces do not count unless escaped
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = strings.TrimSuffix(line, " ")
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	r := ignoreRule{dir: dir}
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return ignoreRule{}, false
	}
	// A slash at the start or in the middle ties the pattern to the
	// directory of the file; without one it matches at any depth.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	re, err := regexp.Compile(ignoreRegexp(line, anchored))
	if err != nil {
		return ignoreRule{}, false
	}
	r.re = re
	return r, true
}

// ignoreRegexp translates a gitignore pattern into a regular expression over
// a slash separated path relative to the pattern's directory.
func ignoreRegexp(pat string, anchored bool) string {
	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				atStart := i == 0 || pat[i-1] == '/'
				i++
				switch {
				case atStart && i+1 < len(pat) && pat[i+1] == '/':
					b.WriteString("(?:.*/)?") // "**/": any number of directories
					i++
				case atStart && i+1 == len(pat):
					b.WriteString(".*") // "/**": everything below
				default:
					b.WriteString("[^/]*") // "a**b" is an ordinary star
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pat[i+1:], ']')
			if end <= 0 {
				b.WriteString(`\[`)
				continue
			}
			class := pat[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		case '\\':
			if i+1 < len(pat) {
				i++
				b.WriteString(regexp.QuoteMeta(string(pat[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}

// ignored reports whether git would ignore the path. The caller does not
// descend into an ignored directory, which is what makes a file below it
// impossible to bring back with a negation, as in git.
func (s *ignoreSet) ignored(path string, isDir bool) bool {
	out := false
	for _, r := range s.rules {
		if r.dirOnly && !isDir {
			continue
		}
		rel, err := filepath.Rel(r.dir, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		if r.re.MatchString(filepath.ToSlash(rel)) {
			out = !r.negate
		}
	}
	return out
}

// gitTop returns the directory that holds .git for the repository dir is
// in, or "" when dir is not inside one.
func gitTop(dir string) string {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// newIgnoreSet loads what applies to a walk that starts at root: the
// repository's info/exclude, then the .gitignore of every directory from the
// top of the repository down to root. The .gitignore files below root are
// loaded by the walk as it enters each directory.
func newIgnoreSet(root string) *ignoreSet {
	s := &ignoreSet{}
	top := gitTop(root)
	if top == "" {
		s.load(filepath.Join(root, ".gitignore"), root)
		return s
	}
	if fi, err := os.Stat(filepath.Join(top, ".git")); err == nil && fi.IsDir() {
		s.load(filepath.Join(top, ".git", "info", "exclude"), top)
	}
	var chain []string
	for dir := root; ; dir = filepath.Dir(dir) {
		chain = append(chain, dir)
		if dir == top || filepath.Dir(dir) == dir {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		s.load(filepath.Join(chain[i], ".gitignore"), chain[i])
	}
	return s
}
