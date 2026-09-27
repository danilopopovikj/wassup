// Package render draws the diagram, panels and timeline, and hosts the
// Bubble Tea model.
package render

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Color is a palette slot that maps onto the terminal's 16 colors.
type Color uint8

// Palette slots.
const (
	ColDefault Color = iota
	ColGreen         // flowing
	ColGray          // idle, dim
	ColAmber         // waiting
	ColBlue          // processing
	ColRed           // blocked, failing
	ColAccent        // annotations (magenta)
	ColCyan          // selection hints
	ColWhite
)

// Style is a cell style.
type Style struct {
	Fg      Color
	Bold    bool
	Dim     bool
	Inverse bool
	Bg      Color
}

// Cell is one terminal cell.
type Cell struct {
	Ch rune
	St Style
}

// Canvas is a grid of cells.
type Canvas struct {
	W, H  int
	Cells []Cell
	// NoColor renders glyphs only.
	NoColor bool
}

// NewCanvas returns a blank canvas filled with spaces.
func NewCanvas(w, h int) *Canvas {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	c := &Canvas{W: w, H: h, Cells: make([]Cell, w*h)}
	for i := range c.Cells {
		c.Cells[i].Ch = ' '
	}
	return c
}

// In reports whether the cell is on the canvas.
func (c *Canvas) In(x, y int) bool { return x >= 0 && y >= 0 && x < c.W && y < c.H }

// Set writes one cell.
func (c *Canvas) Set(x, y int, ch rune, st Style) {
	if !c.In(x, y) {
		return
	}
	c.Cells[y*c.W+x] = Cell{Ch: ch, St: st}
}

// Get reads one cell.
func (c *Canvas) Get(x, y int) Cell {
	if !c.In(x, y) {
		return Cell{Ch: ' '}
	}
	return c.Cells[y*c.W+x]
}

// Text writes a string, clipped to maxW runes (0 = unlimited).
func (c *Canvas) Text(x, y int, s string, st Style, maxW int) int {
	n := 0
	for _, r := range s {
		if maxW > 0 && n >= maxW {
			break
		}
		c.Set(x+n, y, r, st)
		n++
	}
	return n
}

// Fill fills a rectangle with a rune.
func (c *Canvas) Fill(x, y, w, h int, ch rune, st Style) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			c.Set(xx, yy, ch, st)
		}
	}
}

// Border styles.
type BorderKind int

// Border kinds.
const (
	BorderRounded BorderKind = iota
	BorderDouble
	BorderDashed
	BorderHeavy
)

type borderSet struct{ tl, tr, bl, br, h, v rune }

var borders = map[BorderKind]borderSet{
	BorderRounded: {'╭', '╮', '╰', '╯', '─', '│'},
	BorderDouble:  {'╔', '╗', '╚', '╝', '═', '║'},
	BorderDashed:  {'╭', '╮', '╰', '╯', '╌', '╎'},
	BorderHeavy:   {'┏', '┓', '┗', '┛', '━', '┃'},
}

// Box draws a border. The interior is left untouched.
func (c *Canvas) Box(x, y, w, h int, kind BorderKind, st Style) {
	if w < 2 || h < 2 {
		return
	}
	b := borders[kind]
	c.Set(x, y, b.tl, st)
	c.Set(x+w-1, y, b.tr, st)
	c.Set(x, y+h-1, b.bl, st)
	c.Set(x+w-1, y+h-1, b.br, st)
	for xx := x + 1; xx < x+w-1; xx++ {
		c.Set(xx, y, b.h, st)
		c.Set(xx, y+h-1, b.h, st)
	}
	for yy := y + 1; yy < y+h-1; yy++ {
		c.Set(x, yy, b.v, st)
		c.Set(x+w-1, yy, b.v, st)
	}
}

// Dim applies dim to every cell in a rectangle (for the lens).
func (c *Canvas) Dim(x, y, w, h int) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			if c.In(xx, yy) {
				cell := &c.Cells[yy*c.W+xx]
				cell.St.Dim = true
				cell.St.Bold = false
			}
		}
	}
}

// Sub copies a rectangle of this canvas into a new one (viewport).
func (c *Canvas) Sub(x, y, w, h int) *Canvas {
	out := NewCanvas(w, h)
	for yy := 0; yy < h; yy++ {
		for xx := 0; xx < w; xx++ {
			out.Cells[yy*w+xx] = c.Get(x+xx, y+yy)
		}
	}
	out.NoColor = c.NoColor
	return out
}

var ansiColors = map[Color]string{
	ColGreen:  "2",
	ColGray:   "8",
	ColAmber:  "3",
	ColBlue:   "4",
	ColRed:    "1",
	ColAccent: "5",
	ColCyan:   "6",
	ColWhite:  "15",
}

var styleCache = map[Style]lipgloss.Style{}

func lipStyle(st Style, noColor bool) lipgloss.Style {
	if noColor {
		st.Fg = ColDefault
		st.Bg = ColDefault
	}
	if s, ok := styleCache[st]; ok {
		return s
	}
	s := lipgloss.NewStyle()
	if st.Fg != ColDefault {
		s = s.Foreground(lipgloss.Color(ansiColors[st.Fg]))
	}
	if st.Bg != ColDefault {
		s = s.Background(lipgloss.Color(ansiColors[st.Bg]))
	}
	if st.Bold {
		s = s.Bold(true)
	}
	if st.Dim {
		s = s.Faint(true)
	}
	if st.Inverse {
		s = s.Reverse(true)
	}
	styleCache[st] = s
	return s
}

// Lines renders the canvas to styled terminal lines.
func (c *Canvas) Lines() []string {
	out := make([]string, c.H)
	for y := 0; y < c.H; y++ {
		var b strings.Builder
		var run strings.Builder
		var cur Style
		first := true
		flush := func() {
			if run.Len() == 0 {
				return
			}
			if cur == (Style{}) || (c.NoColor && !cur.Bold && !cur.Dim && !cur.Inverse) {
				b.WriteString(run.String())
			} else {
				b.WriteString(lipStyle(cur, c.NoColor).Render(run.String()))
			}
			run.Reset()
		}
		for x := 0; x < c.W; x++ {
			cell := c.Cells[y*c.W+x]
			if first || cell.St != cur {
				flush()
				cur = cell.St
				first = false
			}
			run.WriteRune(cell.Ch)
		}
		flush()
		out[y] = b.String()
	}
	return out
}

// String renders the canvas joined by newlines.
func (c *Canvas) String() string { return strings.Join(c.Lines(), "\n") }

// Plain renders the canvas without any styling (for tests and .txt export).
func (c *Canvas) Plain() string {
	var b strings.Builder
	for y := 0; y < c.H; y++ {
		line := make([]rune, c.W)
		for x := 0; x < c.W; x++ {
			line[x] = c.Cells[y*c.W+x].Ch
		}
		b.WriteString(strings.TrimRight(string(line), " "))
		if y < c.H-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
