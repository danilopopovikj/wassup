package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danilopopovikj/wassup/internal/discover"
	"github.com/danilopopovikj/wassup/internal/model"
)

// localEnvName is the settings file of one machine, in the .wassup
// directory: the kubeconfig, the context and the credentials the bindings
// name. Git ignores it.
const localEnvName = "local.env"

// The variables wassup reads for itself. A flag wins over them.
const (
	envKubeconfig = "WASSUP_KUBECONFIG"
	envContext    = "WASSUP_CONTEXT"
)

// localEnvTemplate is what `wassup init` puts in local.env.
const localEnvTemplate = `# Settings of this machine for wassup. Git ignores this file; wassup reads it
# on every run, so nothing has to be exported. One NAME=value per line, the
# value as it is: no quoting and no encoding is needed, not even for a
# password with special characters. A variable that is already set in the
# environment wins over this file.

# The cluster to read.
# WASSUP_KUBECONFIG=/path/to/kubeconfig
# WASSUP_CONTEXT=production

# The credentials bindings.yaml names (password_env, token_env, dsn_env ...).
# BOOKSTORE_DB_PASSWORD=
# HATCHET_CLIENT_TOKEN=
`

// parseEnv reads NAME=value lines. Blank lines and lines that start with #
// are skipped, a leading "export " is dropped, and a value in a pair of
// quotes loses them. Nothing is expanded and nothing is unescaped: a value
// is a password more often than not, and it is taken as it is written.
func parseEnv(text string) (map[string]string, []string, error) {
	vals := map[string]string{}
	var order []string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, val, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			// The line is not printed: it may be a credential on its own.
			return nil, nil, fmt.Errorf("line %d is not NAME=value", n)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, dup := vals[name]; !dup {
			order = append(order, name)
		}
		vals[name] = val
	}
	return vals, order, sc.Err()
}

// loadEnvFile sets the variables of a file that the environment does not
// have yet and returns their names. A file that is missing is an error only
// when required.
func loadEnvFile(path string, required bool) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return nil, nil
		}
		return nil, err
	}
	vals, order, err := parseEnv(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var set []string
	for _, name := range order {
		if _, have := os.LookupEnv(name); have {
			continue
		}
		if err := os.Setenv(name, vals[name]); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, name, err)
		}
		set = append(set, name)
	}
	return set, nil
}

// wassupDir returns the .wassup directory of this run, or the one a first
// run would create when there is none yet.
func wassupDir() string {
	if dir, err := findDir(); err == nil {
		return dir
	}
	if flags.dir != "" {
		return flags.dir
	}
	return model.DirName
}

// loadSettings reads --env-file and then .wassup/local.env, and takes the
// kubeconfig and the context from the environment when no flag gave them.
// The order is the precedence: the environment, then --env-file, then
// local.env.
func loadSettings() error {
	if flags.envFile != "" {
		if _, err := loadEnvFile(flags.envFile, true); err != nil {
			return err
		}
	}
	dir := wassupDir()
	local := filepath.Join(dir, localEnvName)
	if _, err := os.Stat(local); err == nil {
		// Before it is read: a file with credentials that git would track is
		// worse than a run that stops here.
		if err := discover.EnsureGitignore(dir); err != nil {
			return fmt.Errorf("%s holds credentials and %s could not be made to ignore it: %w", local, filepath.Join(dir, ".gitignore"), err)
		}
		if _, err := loadEnvFile(local, false); err != nil {
			return err
		}
	}
	if flags.kubeconfig != "" || flags.kcontext != "" || os.Getenv(envKubeconfig) != "" || os.Getenv(envContext) != "" {
		flags.clusterChosen = true
	}
	if flags.kubeconfig == "" {
		flags.kubeconfig = os.Getenv(envKubeconfig)
	}
	if flags.kcontext == "" {
		flags.kcontext = os.Getenv(envContext)
	}
	return nil
}

// writeLocalEnvTemplate creates local.env, readable by its owner only, when
// there is none.
func writeLocalEnvTemplate(dir string) error {
	path := filepath.Join(dir, localEnvName)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(localEnvTemplate), 0o600)
}

// pathHint returns what to tell a person whose shell will not find the
// binary they just ran, or "" when the directory is on the PATH.
func pathHint() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return pathHintFor(filepath.Dir(exe), os.Getenv("PATH"), os.Getenv("SHELL"))
}

// pathHintFor is pathHint over its inputs.
func pathHintFor(dir, path, shell string) string {
	for _, p := range filepath.SplitList(path) {
		if p == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		if filepath.Clean(p) == filepath.Clean(dir) {
			return ""
		}
	}
	rc := "your shell profile"
	switch filepath.Base(shell) {
	case "zsh":
		rc = "~/.zshrc"
	case "bash":
		rc = "~/.bashrc"
	case "fish":
		return fmt.Sprintf("%s is not on your PATH, so `wassup` alone will not be found. Run: fish_add_path %s", dir, dir)
	}
	return fmt.Sprintf("%s is not on your PATH, so `wassup` alone will not be found. Add this line to %s and open a new terminal:\n  export PATH=\"%s:$PATH\"", dir, rc, dir)
}
