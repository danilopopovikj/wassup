package discover

import (
	"fmt"
	"strings"

	"github.com/danilopopovikj/wassup/internal/discover/redact"
)

// ReviewFile is the name of the review next to the draft.
const ReviewFile = "review.md"

// Review is the draft as a person reads it in a minute: one line and one
// citation per component and per edge, what is surest first, then the hosts
// nothing answered to, then the notes. evidence.json holds all the evidence
// and is too long to read; this is what to check it against. Every line
// passes through redact.Text once more, like everything that is written.
func (p *Proposal) Review() string {
	var b strings.Builder
	line := func(format string, args ...any) {
		b.WriteString(redact.Text(fmt.Sprintf(format, args...)))
		b.WriteByte('\n')
	}
	line("# Review of the draft")
	line("")
	line("%s: %d components, %d edges. One line each, with the strongest evidence.", p.Topology.Name, len(p.Topology.Components), len(p.Topology.Edges))
	line("")
	line("- high: the live cluster shows it")
	line("- medium: a manifest, Helm values or Terraform declares it")
	line("- low: only code or a .env file mentions it, or it was concluded; check these against the citation")

	for _, level := range []string{High, Medium, Low} {
		var lines []string
		for _, c := range p.Topology.Components {
			if conf := p.confidence(c.ID); conf.Level == level {
				lines = append(lines, reviewLine(c.ID+" ("+c.Type+")", conf))
			}
		}
		for _, e := range p.Topology.Edges {
			if conf := p.confidence(e.ID()); conf.Level == level {
				lines = append(lines, reviewLine(e.From+" -> "+e.To+" ("+e.Kind+")", conf))
			}
		}
		if len(lines) == 0 {
			continue
		}
		line("")
		line("## %s", level)
		line("")
		for _, l := range lines {
			line("%s", l)
		}
	}
	if len(p.Unresolved) > 0 {
		line("")
		line("## unresolved hosts")
		line("")
		line("Something connects to these and nothing in the draft answers to them. Name the component or add it.")
		line("")
		for _, l := range p.Unresolved {
			cite := "no citation"
			if ev := strongestFirst(l.Evidence); len(ev) > 0 {
				cite = ev[0].String()
			}
			line("- %s -> %s, %s", l.From, l.Host, cite)
		}
	}
	if len(p.Notes) > 0 {
		line("")
		line("## notes")
		line("")
		for _, n := range p.Notes {
			line("- %s", n)
		}
	}
	return b.String()
}

// confidence returns the confidence of a component or an edge; a proposal
// that was not rated is rated from its evidence.
func (p *Proposal) confidence(id string) Confidence {
	if c, ok := p.Confidence[id]; ok {
		return c
	}
	return Rate(p.Evidence[id])
}

// reviewLine is one line of the review: what, how sure, why.
func reviewLine(what string, c Confidence) string {
	cite := c.Citation
	if cite == "" {
		cite = "no citation"
	}
	out := "- " + what + ", " + c.Level + ", " + cite
	if c.Caveat != "" {
		out += " (" + c.Caveat + ")"
	}
	return out
}
