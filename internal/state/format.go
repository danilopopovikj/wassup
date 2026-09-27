package state

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Num formats a count the way the diagram speaks: 38, 1.2k, 2.4k, 3.1M.
func Num(v float64) string {
	av := math.Abs(v)
	switch {
	case av >= 1e9:
		return trim(fmt.Sprintf("%.1fG", v/1e9))
	case av >= 1e6:
		return trim(fmt.Sprintf("%.1fM", v/1e6))
	case av >= 1e3:
		return trim(fmt.Sprintf("%.1fk", v/1e3))
	case av >= 100 || av == math.Trunc(av):
		return fmt.Sprintf("%.0f", v)
	}
	return trim(fmt.Sprintf("%.1f", v))
}

func trim(s string) string {
	// 1.0k -> 1k
	if i := strings.Index(s, ".0"); i > 0 && (i+2 == len(s) || !isDigit(s[i+2])) {
		return s[:i] + s[i+2:]
	}
	return s
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// Pct formats a percentage in words: "30 percent".
func Pct(v float64) string { return fmt.Sprintf("%.0f percent", v) }

// Bytes formats bytes: 40 GB, 512 MB.
func Bytes(v float64) string {
	av := math.Abs(v)
	switch {
	case av >= 1<<40:
		return trim(fmt.Sprintf("%.1f TB", v/(1<<40)))
	case av >= 1<<30:
		return trim(fmt.Sprintf("%.1f GB", v/(1<<30)))
	case av >= 1<<20:
		return trim(fmt.Sprintf("%.1f MB", v/(1<<20)))
	case av >= 1<<10:
		return trim(fmt.Sprintf("%.1f kB", v/(1<<10)))
	}
	return fmt.Sprintf("%.0f B", v)
}

// Dur formats a duration in plain words: 18 min, 3 h, 2 d, 40 s.
func Dur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < 2*time.Hour:
		return fmt.Sprintf("%d min", int(math.Round(d.Minutes())))
	case d < 48*time.Hour:
		h := d.Hours()
		if h < 10 && h != math.Trunc(h) {
			return trim(fmt.Sprintf("%.1f h", h))
		}
		return fmt.Sprintf("%d h", int(math.Round(h)))
	}
	return fmt.Sprintf("%d d", int(math.Round(d.Hours()/24)))
}

// Clock formats a time as HH:MM in the time's own location.
func Clock(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("15:04")
}

// Day formats a time as MM-DD.
func Day(t time.Time) string { return t.Format("01-02") }

// Ago formats how long ago t was, relative to now.
func Ago(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return Dur(now.Sub(t))
}

// Lower lowercases the first rune of a label for use inside a sentence,
// unless the label looks like an acronym.
func Lower(s string) string {
	if s == "" {
		return s
	}
	if strings.ToUpper(s) == s && len(s) <= 5 {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// Plural returns the singular or plural form for n.
func Plural(n float64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
