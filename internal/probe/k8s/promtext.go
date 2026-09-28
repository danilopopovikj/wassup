package k8s

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// series is one line of a metrics page: the labels of a time series and its
// value.
type series struct {
	labels map[string]string
	value  float64
}

// maxMetricLine bounds one line of a metrics page. A line is a name, its
// labels and a number; one longer than this is not a metrics page.
const maxMetricLine = 1 << 20

// seriesOf returns the series of one metric from a page in the Prometheus
// text format (`name{label="value",...} 12 [timestamp]`). Comments, other
// metrics and values that are not a finite number are left out. A line of
// the metric that cannot be read is an error: a page that is half understood
// would add up to a number that means nothing.
func seriesOf(page []byte, metric string) ([]series, error) {
	var out []series
	sc := bufio.NewScanner(bytes.NewReader(page))
	sc.Buffer(make([]byte, 0, 64*1024), maxMetricLine)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		rest, ok := strings.CutPrefix(line, metric)
		if !ok || rest == "" || line[0] == '#' {
			continue
		}
		// the name ends here, or this is a longer name that starts the same
		if rest[0] != '{' && rest[0] != ' ' && rest[0] != '\t' {
			continue
		}
		s, err := parseSeries(rest)
		if err != nil {
			return nil, fmt.Errorf("line %d of the metrics page: %w", n, err)
		}
		if math.IsNaN(s.value) || math.IsInf(s.value, 0) {
			continue
		}
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("the metrics page: %w", err)
	}
	return out, nil
}

// parseSeries reads what follows the metric's name: the labels, when there
// are any, and the value. A timestamp after the value is ignored.
func parseSeries(rest string) (series, error) {
	s := series{labels: map[string]string{}}
	if rest[0] == '{' {
		end, err := parseLabels(rest[1:], s.labels)
		if err != nil {
			return series{}, err
		}
		rest = rest[1+end:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return series{}, fmt.Errorf("a series without a value")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return series{}, fmt.Errorf("value %q is not a number", fields[0])
	}
	s.value = v
	return s, nil
}

// parseLabels reads `name="value",...}` into labels and returns how much of
// s it took, the closing brace included. A value may hold \\, \" and \n.
func parseLabels(s string, labels map[string]string) (int, error) {
	i := 0
	skip := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == ',') {
			i++
		}
	}
	for {
		skip()
		if i >= len(s) {
			return 0, fmt.Errorf("labels without a closing brace")
		}
		if s[i] == '}' {
			return i + 1, nil
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq <= 0 {
			return 0, fmt.Errorf("a label without a value")
		}
		name := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		if i >= len(s) || s[i] != '"' {
			return 0, fmt.Errorf("the value of label %q is not quoted", name)
		}
		i++
		var b strings.Builder
		closed := false
		for i < len(s) && !closed {
			switch c := s[i]; {
			case c == '\\' && i+1 < len(s):
				switch s[i+1] {
				case 'n':
					b.WriteByte('\n')
				default: // \\ and \" stand for themselves
					b.WriteByte(s[i+1])
				}
				i += 2
			case c == '"':
				closed = true
				i++
			default:
				b.WriteByte(c)
				i++
			}
		}
		if !closed {
			return 0, fmt.Errorf("the value of label %q has no closing quote", name)
		}
		labels[name] = b.String()
	}
}
