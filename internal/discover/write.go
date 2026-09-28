package discover

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/danilopopovikj/wassup/internal/model"
)

// ProposedDir is the directory under .wassup/ a proposal is written to.
const ProposedDir = "proposed"

// ignoredPaths is what git must not track in .wassup/: state is what
// wassup recorded on this machine, proposed is a draft that holds evidence
// read from the live cluster, and local.env is the one file that may hold
// credentials.
var ignoredPaths = []string{"state/", "proposed/", "local.env"}

// EnsureGitignore makes sure <dir>/.gitignore ignores state/, proposed/ and
// local.env, where dir is the .wassup directory. It creates the file when
// it is missing; when it exists it appends the lines that are missing and
// leaves the rest as the user wrote it.
func EnsureGitignore(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, ".gitignore")
	cur, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(cur), "\n") {
		// state, state/, /state and /state/ all ignore the directory
		have[strings.Trim(strings.TrimSpace(line), "/")] = true
	}
	var missing []string
	for _, d := range ignoredPaths {
		if !have[strings.Trim(d, "/")] {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	out := string(cur)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += strings.Join(missing, "\n") + "\n"
	return model.WriteAtomic(path, []byte(out))
}

// evidenceFile is what evidence.json holds.
type evidenceFile struct {
	Evidence   map[string][]Evidence `json:"evidence"`
	Confidence map[string]Confidence `json:"confidence,omitempty"`
	Unresolved []Link                `json:"unresolved"`
	Notes      []string              `json:"notes"`
	Findings   *Findings             `json:"findings"`
}

// WriteProposal writes a proposal under <dir>/proposed/, where dir is the
// .wassup directory: topology.yaml, bindings.yaml, evidence.json, which
// holds the evidence and the confidence per component and edge, the hosts
// nothing answered to, the notes and the findings the proposal was built
// from, and review.md, the same in one line per component and edge. It
// makes sure git ignores the directory before anything is written into it.
//
// Nothing written here holds a credential: findings keep the names of
// variables and the hosts they point at (see the redact package), and the
// proposal is built from the findings alone.
func WriteProposal(dir string, p *Proposal, f *Findings) error {
	if err := EnsureGitignore(dir); err != nil {
		return err
	}
	out := filepath.Join(dir, ProposedDir)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := model.WriteYAML(filepath.Join(out, "topology.yaml"), p.Topology); err != nil {
		return err
	}
	if err := model.WriteYAML(filepath.Join(out, "bindings.yaml"), p.Bindings); err != nil {
		return err
	}
	if err := model.WriteJSON(filepath.Join(out, "evidence.json"), evidenceFile{Evidence: p.Evidence, Confidence: p.Confidence, Unresolved: p.Unresolved, Notes: p.Notes, Findings: f}); err != nil {
		return err
	}
	return model.WriteAtomic(filepath.Join(out, ReviewFile), []byte(p.Review()))
}
