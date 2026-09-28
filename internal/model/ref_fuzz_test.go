package model

import (
	"strings"
	"testing"
)

// FuzzParseRef feeds ParseRef what a person might paste into a prompt. A ref
// that parses must survive the trip the TUI sends it on: copied as a string
// and parsed again, it names the same element at the same moment.
func FuzzParseRef(f *testing.F) {
	for _, s := range []string{
		"wassup://workload/api",
		"wassup://workload/api@2026-09-27T09:52:00Z",
		"wassup://workload/api@09:52",
		"wassup://edge/api->db",
		"wassup://group/data",
		"wassup://nothing",
		"api",
		"api->db",
		"  api@bad  ",
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseRef(s)
		if err != nil {
			if r != (Ref{}) {
				t.Fatalf("ParseRef(%q) failed and still returned %+v", s, r)
			}
			return
		}
		if r.ID == "" {
			t.Fatalf("ParseRef(%q) accepted a ref without an id", s)
		}
		// A bare id has no kind until the caller resolves it against the
		// topology, so only full refs are expected to round trip.
		if !strings.HasPrefix(strings.TrimSpace(s), RefScheme) {
			return
		}
		// String leaves the zero time out and writes four digits for the
		// year; a timestamp outside of that cannot be written back.
		if strings.Contains(s, "@") {
			if y := r.At.UTC().Year(); r.At.IsZero() || y < 0 || y > 9999 {
				return
			}
		}
		again, err := ParseRef(r.String())
		if err != nil {
			t.Fatalf("ParseRef(%q) gave %q, which does not parse: %v", s, r.String(), err)
		}
		if again.Kind != r.Kind || again.ID != r.ID || !again.At.Equal(r.At) {
			t.Fatalf("ParseRef(%q) gave %+v, and %+v after a round trip", s, r, again)
		}
	})
}
