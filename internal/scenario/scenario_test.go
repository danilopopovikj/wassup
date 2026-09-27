package scenario

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestScenarios replays every recorded fixture offline and checks the
// states, labels, cause and story against expected.yaml.
func TestScenarios(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "scenarios")
	dirs, err := Dirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 10 {
		t.Fatalf("want 10 scenarios, found %d", len(dirs))
	}
	for _, d := range dirs {
		d := d
		t.Run(filepath.Base(d), func(t *testing.T) {
			r, err := Load(d)
			if err != nil {
				t.Fatal(err)
			}
			snap := r.Play()
			if problems := r.Check(); len(problems) > 0 {
				var story []string
				if len(snap.Issues) > 0 {
					story = snap.Issues[0].Story
				}
				t.Errorf("%d mismatch(es):\n  %s\nstory:\n  %s", len(problems), strings.Join(problems, "\n  "), strings.Join(story, "\n  "))
			}
			if r.Expected.LensOn != nil && *r.Expected.LensOn && (r.FirstCritTick == 0 || r.FirstCritTick > 2) {
				t.Errorf("lens must turn on within 2 ticks, first crit at tick %d", r.FirstCritTick)
			}
		})
	}
}
