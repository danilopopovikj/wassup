package render

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Cell size of the SVG grid so the picture matches the terminal.
const (
	svgCellW = 8
	svgCellH = 16
)

var svgColors = map[Color]string{
	ColDefault: "#d0d0d0",
	ColGreen:   "#3fb950",
	ColGray:    "#8b949e",
	ColAmber:   "#d29922",
	ColBlue:    "#58a6ff",
	ColRed:     "#f85149",
	ColAccent:  "#d2a8ff",
	ColCyan:    "#39c5cf",
	ColWhite:   "#ffffff",
}

// SVG renders the canvas as an SVG document on an 8 by 16 pixel cell grid.
func (c *Canvas) SVG() string {
	var b strings.Builder
	w, h := c.W*svgCellW, c.H*svgCellH
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`, w, h, w, h)
	b.WriteString(`<rect width="100%" height="100%" fill="#0d1117"/>`)
	b.WriteString(`<g font-family="JetBrains Mono, Menlo, DejaVu Sans Mono, monospace" font-size="13" xml:space="preserve">`)
	for y := 0; y < c.H; y++ {
		x := 0
		for x < c.W {
			cell := c.Get(x, y)
			// One element per run of non-space cells sharing a style, pinned to
			// the cell grid with textLength; renderers collapse whitespace, so
			// spaces never go inside an element.
			if cell.Ch == ' ' && !cell.St.Inverse {
				x++
				continue
			}
			run := []rune{cell.Ch}
			j := x + 1
			for j < c.W && c.Get(j, y).St == cell.St && (c.Get(j, y).Ch != ' ' || cell.St.Inverse) {
				run = append(run, c.Get(j, y).Ch)
				j++
			}
			col := svgColors[cell.St.Fg]
			if cell.St.Dim {
				col = dimHex(col)
			}
			px, py := x*svgCellW, y*svgCellH
			if cell.St.Inverse {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`, px, py, len(run)*svgCellW, svgCellH, col)
				col = "#0d1117"
			}
			text := strings.TrimRight(string(run), " ")
			if text != "" {
				attrs := fmt.Sprintf(`x="%d" y="%d" fill="%s" textLength="%d" lengthAdjust="spacingAndGlyphs"`, px, py+12, col, len([]rune(text))*svgCellW)
				if cell.St.Bold {
					attrs += ` font-weight="bold"`
				}
				fmt.Fprintf(&b, `<text %s>%s</text>`, attrs, html.EscapeString(text))
			}
			x = j
		}
	}
	b.WriteString(`</g></svg>`)
	return b.String()
}

func dimHex(hex string) string {
	if len(hex) != 7 {
		return hex
	}
	var r, g, bl int
	fmt.Sscanf(hex[1:], "%02x%02x%02x", &r, &g, &bl)
	return fmt.Sprintf("#%02x%02x%02x", r/2, g/2, bl/2)
}

// ExportResult lists the files an export wrote.
type ExportResult struct {
	SVG string
	PNG string
	TXT string
	Err string
}

// Export writes <dir>/<timestamp>.svg, .txt and, when a converter is
// installed, .png.
func Export(dir string, c *Canvas, story []string, now time.Time) ExportResult {
	stamp := now.Format("20060102-150405")
	base := filepath.Join(dir, stamp)
	res := ExportResult{}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Err = err.Error()
		return res
	}
	svg := c.SVG()
	if err := os.WriteFile(base+".svg", []byte(svg), 0o644); err != nil {
		res.Err = err.Error()
		return res
	}
	res.SVG = base + ".svg"
	txt := c.Plain()
	if len(story) > 0 {
		txt += "\n\n" + strings.Join(story, "\n")
	}
	if err := os.WriteFile(base+".txt", []byte(txt+"\n"), 0o644); err == nil {
		res.TXT = base + ".txt"
	}
	if png, err := toPNG(res.SVG, base+".png"); err == nil {
		res.PNG = png
	} else {
		res.Err = err.Error()
	}
	return res
}

// toPNG converts with resvg or rsvg-convert when one is installed.
func toPNG(svgPath, pngPath string) (string, error) {
	if p, err := exec.LookPath("resvg"); err == nil {
		if out, err := exec.Command(p, svgPath, pngPath).CombinedOutput(); err != nil {
			return "", fmt.Errorf("resvg: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return pngPath, nil
	}
	if p, err := exec.LookPath("rsvg-convert"); err == nil {
		if out, err := exec.Command(p, "-o", pngPath, svgPath).CombinedOutput(); err != nil {
			return "", fmt.Errorf("rsvg-convert: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return pngPath, nil
	}
	return "", fmt.Errorf("no PNG converter found (install resvg or rsvg-convert); SVG written")
}
