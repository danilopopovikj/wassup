package render

import (
	"fmt"
	"time"

	"github.com/danilopopovikj/wassup/internal/history"
	"github.com/danilopopovikj/wassup/internal/model"
)

// TimelineSpan is how much history the strip shows.
const TimelineSpan = 6 * time.Hour

// drawTimeline paints the 3-row strip: markers, severity bars, time labels.
func drawTimeline(c *Canvas, x, y, w int, ring *history.Ring, events []model.Event, now time.Time, scrubAt time.Time, scrubbing bool) {
	if w < 10 {
		return
	}
	labelW := 7
	barX := x + labelW
	barW := w - labelW
	c.Text(x, y, "marks", Style{Fg: ColGray}, labelW)
	c.Text(x, y+1, "6h", Style{Fg: ColGray}, labelW)
	c.Text(x, y+2, "", Style{}, labelW)
	buckets := ring.Buckets(now, TimelineSpan, barW)
	start := now.Add(-TimelineSpan)
	colOf := func(t time.Time) int {
		if t.Before(start) || t.After(now) {
			return -1
		}
		i := int(float64(barW) * float64(t.Sub(start)) / float64(TimelineSpan))
		if i >= barW {
			i = barW - 1
		}
		return i
	}
	for i, b := range buckets {
		ch := '▁'
		st := Style{Fg: ColGray, Dim: true}
		if b.Has {
			switch b.Worst {
			case model.Crit:
				ch, st = '█', Style{Fg: ColRed}
			case model.Warn:
				ch, st = '▆', Style{Fg: ColAmber}
			default:
				ch, st = '▃', Style{Fg: ColGreen}
			}
		}
		c.Set(barX+i, y+1, ch, st)
	}
	// Markers: letters on the top row.
	for _, e := range events {
		col := colOf(e.At)
		if col < 0 {
			continue
		}
		c.Text(barX+col, y, e.Letter(), Style{Fg: ColCyan, Bold: true}, 1)
	}
	// Time labels every ~24 columns.
	step := 24
	for col := 0; col < barW; col += step {
		t := start.Add(time.Duration(float64(TimelineSpan) * float64(col) / float64(barW)))
		lbl := t.Format("15:04")
		if col+len(lbl) <= barW {
			c.Text(barX+col, y+2, lbl, Style{Fg: ColGray, Dim: true}, len(lbl))
		}
	}
	nowLbl := "now " + now.Format("15:04")
	if barW > len(nowLbl)+1 {
		c.Text(barX+barW-len(nowLbl), y+2, nowLbl, Style{Fg: ColGray}, len(nowLbl))
	}
	if scrubbing {
		col := colOf(scrubAt)
		if col >= 0 {
			c.Set(barX+col, y+1, '┃', Style{Fg: ColWhite, Bold: true, Inverse: true})
			lbl := fmt.Sprintf("▲ %s", scrubAt.Format("15:04:05"))
			lx := barX + col - 1
			if lx+len(lbl) > x+w {
				lx = x + w - len(lbl)
			}
			c.Text(lx, y+2, lbl, Style{Fg: ColWhite, Bold: true}, len(lbl))
		}
	}
}
