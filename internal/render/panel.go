package render

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/state"
)

// drawPanel paints the right-hand panel.
func (m *Model) drawPanel(c *Canvas) {
	px, py, pw, ph := m.panelRect()
	if pw < 12 {
		return
	}
	c.Fill(px, py, pw, ph, ' ', Style{})
	for y := py; y < py+ph; y++ {
		c.Set(px, y, '│', Style{Fg: ColGray})
	}
	lines := m.panelLines(pw - 3)
	if m.panelScroll > len(lines)-ph+1 {
		m.panelScroll = max(0, len(lines)-ph+1)
	}
	for i := 0; i < ph; i++ {
		li := i + m.panelScroll
		if li >= len(lines) {
			break
		}
		text, st := styledLine(lines[li])
		c.Text(px+2, py+i, ellipsis(text, pw-3), st, pw-3)
	}
	if len(lines) > ph {
		c.Text(px+pw-6, py+ph-1, fmt.Sprintf("%d/%d", min(m.panelScroll+ph, len(lines)), len(lines)), Style{Fg: ColGray}, 6)
	}
}

// Lines may start with a style marker: "!!" bold, "##" heading, "..": dim,
// "~~" amber, "!!!" red, "++" green, "@@" accent.
func styledLine(s string) (string, Style) {
	switch {
	case strings.HasPrefix(s, "!!!"):
		return s[3:], Style{Fg: ColRed, Bold: true}
	case strings.HasPrefix(s, "!!"):
		return s[2:], Style{Bold: true}
	case strings.HasPrefix(s, "##"):
		return s[2:], Style{Fg: ColCyan, Bold: true}
	case strings.HasPrefix(s, ".."):
		return s[2:], Style{Fg: ColGray}
	case strings.HasPrefix(s, "~~"):
		return s[2:], Style{Fg: ColAmber}
	case strings.HasPrefix(s, "++"):
		return s[2:], Style{Fg: ColGreen}
	case strings.HasPrefix(s, "@@"):
		return s[2:], Style{Fg: ColAccent}
	}
	return s, Style{}
}

func (m *Model) panelLines(width int) []string {
	switch m.panelMode {
	case panelFindings:
		return m.findingsLines(width)
	case panelEvents:
		return m.eventsLines()
	case panelAnnotations:
		return m.annotationLines(width)
	case panelHelp:
		return helpLines()
	}
	return m.detailLines(width)
}

func stateLine(es model.ElementState) string {
	col := "!!"
	switch es.State {
	case model.Failing, model.Blocked:
		col = "!!!"
	case model.Waiting:
		col = "~~"
	case model.Flowing:
		col = "++"
	}
	s := col + es.State.Glyph() + " " + es.Label
	if es.Marker != "" {
		s += " [" + string(es.Marker) + "]"
	}
	return s
}

func (m *Model) detailLines(width int) []string {
	id := m.selected
	t := &m.cfg.Topology
	var out []string
	es, ok := m.selectedState()
	now := m.rt.Now()
	if e, isEdge := t.Edge(id); isEdge {
		out = append(out, "##"+t.LabelOf(e.From)+" → "+t.LabelOf(e.To), ".."+e.Kind+" edge · "+id)
		if ok {
			out = append(out, stateLine(es))
			out = append(out, fmt.Sprintf("..severity %s · since %s", es.Severity, sinceText(now, es.Since)))
			if es.Rate > 0 {
				out = append(out, fmt.Sprintf("rate      %s %s", state.Num(es.Rate), es.Unit))
			}
			if es.Queued > 0 {
				out = append(out, fmt.Sprintf("queued    %s", state.Num(es.Queued)))
			}
			if es.ErrorRate > 0 {
				out = append(out, fmt.Sprintf("errors    %s", state.Pct(es.ErrorRate)))
			}
		}
		out = append(out, "", "##ends")
		for _, end := range []string{e.From, e.To} {
			if s, ok := m.view.Components[end]; ok {
				out = append(out, fmt.Sprintf("  %s %-14s %s", s.State.Glyph(), t.LabelOf(end), ellipsis(s.Label, width-20)))
			}
		}
	} else if comp, isComp := t.Component(id); isComp {
		out = append(out, "##"+comp.DisplayLabel(), ".."+comp.Type+" · "+id+groupText(comp))
		if ok {
			out = append(out, stateLine(es))
			out = append(out, fmt.Sprintf("..severity %s · since %s", es.Severity, sinceText(now, es.Since)))
			if !es.LastData.IsZero() {
				out = append(out, fmt.Sprintf("..last data %s ago", state.Ago(now, es.LastData)))
			}
		}
		if comp.Owner != "" {
			out = append(out, "owner     "+comp.Owner)
		}
		if comp.Notes != "" && comp.Parent == "" {
			out = append(out, "notes     "+comp.Notes)
		}
		if len(comp.RunsOn) > 0 {
			out = append(out, "runs on   "+strings.Join(comp.RunsOn, ", "))
		}
		if ok && len(es.Gauges) > 0 {
			out = append(out, "", "##gauges")
			for _, g := range es.Gauges {
				pfx := ""
				switch g.Level {
				case "amber":
					pfx = "~~"
				case "red":
					pfx = "!!!"
				}
				line := fmt.Sprintf("  %-8s %s", g.Name, g.Value)
				if g.Trend != "" {
					line += " " + map[string]string{"up": "↑", "down": "↓"}[g.Trend]
				}
				if g.Detail != "" {
					line += " · " + g.Detail
				}
				out = append(out, pfx+line)
			}
		}
	} else if gr, isGroup := t.Group(id); isGroup {
		out = append(out, "##"+gr.Label, ".."+gr.Kind+" group · "+id)
		for _, cid := range t.GroupDescendants(id) {
			if s, ok := m.view.Components[cid]; ok {
				out = append(out, fmt.Sprintf("  %s %-14s %s", s.State.Glyph(), t.LabelOf(cid), ellipsis(s.Label, width-20)))
			}
		}
		return out
	} else {
		return []string{"..nothing selected"}
	}
	if ok {
		if len(es.Notes) > 0 {
			out = append(out, "", "##notes")
			for _, n := range es.Notes {
				out = append(out, "  "+n)
			}
		}
		if len(es.Conditions) > 0 {
			out = append(out, "", "##conditions")
			for _, cnd := range es.Conditions {
				line := "  " + cnd.Kind
				if cnd.Ref != "" {
					line += " " + cnd.Ref
				}
				if !cnd.Since.IsZero() {
					line += " · since " + state.Clock(cnd.Since)
				}
				out = append(out, "!!!"+line)
				if cnd.Detail != "" {
					out = append(out, "    "+cnd.Detail)
				}
			}
		}
		if len(es.Metrics) > 0 {
			out = append(out, "", "##metrics")
			keys := make([]string, 0, len(es.Metrics))
			for k := range es.Metrics {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				out = append(out, fmt.Sprintf("  %-20s %s", k, fmtMetric(k, es.Metrics[k])))
			}
		}
	}
	// related edges
	var rel []string
	for _, e := range t.Edges {
		if e.From == id || e.To == id {
			s := m.view.Edges[e.ID()]
			rel = append(rel, fmt.Sprintf("  %s %s", s.State.Glyph(), ellipsis(t.LabelOf(e.From)+" → "+t.LabelOf(e.To)+": "+s.Label, width-4)))
		}
	}
	if len(rel) > 0 {
		out = append(out, "", "##edges")
		out = append(out, rel...)
	}
	// events touching the element
	var evs []string
	all := m.rt.Events()
	for i := len(all) - 1; i >= 0 && len(evs) < 5; i-- {
		if all[i].Target == id {
			evs = append(evs, fmt.Sprintf("  %s %s %s", all[i].Letter(), state.Clock(all[i].At), all[i].Summary))
		}
	}
	if len(evs) > 0 {
		out = append(out, "", "##changes")
		out = append(out, evs...)
	}
	// findings
	var fs []string
	for _, f := range m.cfg.Findings.Findings {
		if f.Component == id {
			fs = append(fs, fmt.Sprintf("  %s %s", sevTag(f.Severity), f.Title))
		}
	}
	if len(fs) > 0 {
		out = append(out, "", "##findings")
		out = append(out, fs...)
	}
	// bindings
	var bs []string
	for _, spec := range m.cfg.Bindings.Components[id] {
		bs = append(bs, "  "+spec.Kind()+" "+specSummary(spec))
	}
	for _, spec := range m.cfg.Bindings.Edges[id] {
		bs = append(bs, "  "+spec.Kind()+" "+specSummary(spec))
	}
	if len(bs) > 0 {
		out = append(out, "", "##probes")
		out = append(out, bs...)
	} else if _, isGroup := t.Group(id); !isGroup {
		out = append(out, "", "~~no probe bound: this element is unbound")
	}
	// detail
	if ok && len(es.Detail) > 0 {
		out = append(out, "", "##detail")
		keys := make([]string, 0, len(es.Detail))
		for k := range es.Detail {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := es.Detail[k]
			switch vv := v.(type) {
			case []string:
				out = append(out, "  "+k+":")
				for _, s := range vv {
					out = append(out, "    "+s)
				}
			case []any:
				out = append(out, "  "+k+":")
				for _, s := range vv {
					out = append(out, "    "+fmt.Sprint(s))
				}
			default:
				out = append(out, fmt.Sprintf("  %-14s %v", k, v))
			}
		}
	}
	out = append(out, "", "..ref  "+m.selectedRef(), "..c copies the ref · C the summary · y this panel")
	return out
}

func groupText(c model.Component) string {
	if c.Group == "" {
		return ""
	}
	return " · in " + c.Group
}

func sinceText(now, t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return state.Clock(t) + " (" + state.Ago(now, t) + ")"
}

func fmtMetric(k string, v float64) string {
	switch {
	case strings.HasSuffix(k, "_pct") || k == "error_rate" || k == "timeout_rate" || k == "hit_rate":
		return fmt.Sprintf("%.0f%%", v)
	case strings.HasSuffix(k, "_bytes"):
		return state.Bytes(v)
	case strings.HasSuffix(k, "_s"):
		return state.Dur(time.Duration(v * float64(time.Second)))
	case strings.HasSuffix(k, "_ms"):
		return fmt.Sprintf("%.0f ms", v)
	}
	return state.Num(v)
}

func sevTag(s string) string {
	switch s {
	case "critical":
		return "!!!CRIT"
	case "high":
		return "!!!high"
	case "medium":
		return "~~med "
	}
	return "..low "
}

func specSummary(spec model.ProbeSpec) string {
	keys := make([]string, 0, len(spec))
	for k := range spec {
		if k == "probe" || strings.HasPrefix(k, "_") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, spec[k]))
	}
	return strings.Join(parts, " ")
}

func (m *Model) findingsLines(width int) []string {
	fs := m.cfg.Findings.Findings
	out := []string{fmt.Sprintf("##findings · %d", len(fs)), "..from findings.yaml, written by Claude Code scan"}
	if len(fs) == 0 {
		return append(out, "", "..none. Run the Scan workflow of the wassup skill to populate findings.yaml.")
	}
	order := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}
	sorted := append([]model.Finding(nil), fs...)
	sort.SliceStable(sorted, func(i, j int) bool { return order[sorted[i].Severity] < order[sorted[j].Severity] })
	for _, f := range sorted {
		out = append(out, "", sevTag(f.Severity)+" "+f.Title)
		meta := f.ID
		if f.Component != "" {
			meta += " · " + f.Component
		}
		if f.File != "" {
			meta += " · " + f.File
			if f.Line > 0 {
				meta += fmt.Sprintf(":%d", f.Line)
			}
		}
		out = append(out, ".."+meta)
		if f.Evidence != "" {
			out = append(out, wrap("  "+f.Evidence, width)...)
		}
		if f.SuggestedFix != "" {
			out = append(out, wrap("++  fix: "+f.SuggestedFix, width)...)
		}
	}
	return out
}

func (m *Model) eventsLines() []string {
	evs := m.rt.Events()
	out := []string{fmt.Sprintf("##changes · %d", len(evs)), "..D deploy · T terraform · N node · C cert · S scale · W switchover"}
	for i := len(evs) - 1; i >= 0 && i >= len(evs)-60; i-- {
		e := evs[i]
		line := fmt.Sprintf("  %s %s %-12s %s", e.Letter(), e.At.Format("01-02 15:04"), e.Target, e.Summary)
		if e.Author != "" {
			line += " · " + e.Author
		}
		out = append(out, line)
	}
	if len(evs) == 0 {
		out = append(out, "", "..no change markers yet")
	}
	return out
}

func (m *Model) annotationLines(width int) []string {
	live := m.annot.Live(m.rt.Now())
	out := []string{fmt.Sprintf("##annotations · %d", len(live)), "..written by `wassup annotate` · x clears the selected one, X all"}
	for _, an := range live {
		out = append(out, "", "@@"+an.ID+" · "+an.Source+" · "+an.Confidence)
		out = append(out, wrap("  "+an.Note, width)...)
		out = append(out, "..  "+strings.Join(an.Path, " → "))
	}
	if len(live) == 0 {
		out = append(out, "", "..none. Claude Code draws here with: wassup annotate --path <ref,...> --note <text>")
	}
	return out
}

func helpLines() []string {
	return []string{
		"##keys",
		"  ←↑→↓ hjkl   move selection",
		"  HJKL / shift+arrows   pan the diagram",
		"  enter        open detail   tab  toggle panel",
		"  i            issue lens    n    next issue",
		"  t            timeline scrub, [ ] step 1 min, { } 10 min, esc live",
		"  click strip  jump the scrub cursor",
		"  c            copy wassup:// ref   C  ref + summary   y  panel text",
		"  f e a        findings · changes · annotations",
		"  x X          clear selected / all annotations",
		"  s            export svg/png/txt, copies the png path",
		"  g            collapse or expand the selected group",
		"  r R          reset layout of selection / everything",
		"  / text       filter by name",
		"  + -          resize the panel split",
		"  q            quit",
		"",
		"##mouse",
		"  click selects, drag moves a box, drag the bottom-right corner resizes",
		"  click a group title to collapse it; positions are saved to layout.json",
		"",
		"##states",
		"++  ● flowing   work is moving",
		"..  ○ idle      bound, healthy, nothing happening",
		"~~  ≡ waiting   work is queued at the destination",
		"  ◐ processing a long unit of work is running",
		"!!!  ⊘ blocked   traffic cannot pass",
		"!!!  ✕ failing   errors or crashes",
		"..  ┄ unbound   no probe returned data    ◷ stale  data older than 3 ticks",
	}
}

func wrap(s string, width int) []string {
	if width < 10 {
		return []string{s}
	}
	words := strings.Fields(s)
	var out []string
	var cur string
	indent := "  "
	for _, w := range words {
		if cur == "" {
			cur = w
			continue
		}
		if len([]rune(cur))+1+len([]rune(w)) > width {
			out = append(out, cur)
			cur = indent + w
			continue
		}
		cur += " " + w
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
