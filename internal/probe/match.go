package probe

import (
	"fmt"
	"math"
	"regexp"
	"time"
)

// Matchers reads a map of label to regular expression from a spec, such as
// {code: "5.."}. An expression has to match the whole value. It is how a
// binding names the series of a counter it means, whatever the source of
// the counter is.
func Matchers(spec map[string]any, key string) (map[string]*regexp.Regexp, error) {
	raw, ok := spec[key]
	if !ok || raw == nil {
		return nil, nil
	}
	pairs := map[string]string{}
	switch m := raw.(type) {
	case map[string]string:
		pairs = m
	case map[string]any:
		for k, v := range m {
			pairs[k] = fmt.Sprint(v)
		}
	case map[any]any:
		for k, v := range m {
			pairs[fmt.Sprint(k)] = fmt.Sprint(v)
		}
	default:
		return nil, fmt.Errorf("%s must map a label to an expression, such as {code: \"5..\"}", key)
	}
	out := make(map[string]*regexp.Regexp, len(pairs))
	for label, expr := range pairs {
		re, err := regexp.Compile("^(?:" + expr + ")$")
		if err != nil {
			return nil, fmt.Errorf("%s: the expression for label %q: %w", key, label, err)
		}
		out[label] = re
	}
	return out, nil
}

// Matches reports whether labels carry every label the matchers name, with
// a value the expression accepts. No matchers match everything.
func Matches(labels map[string]string, m map[string]*regexp.Regexp) bool {
	for label, re := range m {
		v, ok := labels[label]
		if !ok || !re.MatchString(v) {
			return false
		}
	}
	return true
}

// StrictDur reads a duration a spec may set and refuses one it cannot read,
// where Dur falls back to the default in silence.
func StrictDur(spec map[string]any, key string, def time.Duration) (time.Duration, error) {
	v, ok := spec[key]
	if !ok || v == nil || v == "" {
		return def, nil
	}
	s, isString := v.(string)
	if !isString {
		return 0, fmt.Errorf("%s must be a duration such as 15s", key)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: %q is not a duration such as 15s", key, s)
	}
	return d, nil
}

// Round rounds a rate to a number of decimals. Three keep a request a
// minute apart from none: 0.017 per second.
func Round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}
