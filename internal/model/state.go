package model

// State is one of the six states every node and edge is always in.
type State string

// The six states. Nothing else may introduce a new state.
const (
	Flowing    State = "flowing"
	Idle       State = "idle"
	Waiting    State = "waiting"
	Processing State = "processing"
	Blocked    State = "blocked"
	Failing    State = "failing"
)

// AllStates in display order.
var AllStates = []State{Flowing, Idle, Waiting, Processing, Blocked, Failing}

// Marker describes wassup itself, not the system. It sits outside the six states.
type Marker string

// Markers.
const (
	MarkerNone    Marker = ""
	MarkerUnbound Marker = "unbound" // no probe returned data
	MarkerStale   Marker = "stale"   // last data older than 3 ticks
	MarkerNoData  Marker = "nodata"  // the rate source is down, so idle would be a lie
	// MarkerUnmetered is an element that is bound and in order while nothing
	// counts what goes through it: it is there and answers, and whether it is
	// busy is not known. It must not look idle, and it must not look broken.
	MarkerUnmetered Marker = "unmetered"
)

// Glyph is the one glyph per state.
func (s State) Glyph() string {
	switch s {
	case Flowing:
		return "●"
	case Idle:
		return "○"
	case Waiting:
		return "≡"
	case Processing:
		return "◐"
	case Blocked:
		return "⊘"
	case Failing:
		return "✕"
	}
	return "?"
}

// Severity is separate from state.
type Severity int

// Severities.
const (
	Info Severity = iota
	Warn
	Crit
)

func (s Severity) String() string {
	switch s {
	case Warn:
		return "warn"
	case Crit:
		return "crit"
	}
	return "info"
}

// MarshalJSON writes the severity as a word.
func (s Severity) MarshalJSON() ([]byte, error) { return []byte(`"` + s.String() + `"`), nil }

// UnmarshalJSON reads a word or a number.
func (s *Severity) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case `"warn"`, "1":
		*s = Warn
	case `"crit"`, "2":
		*s = Crit
	default:
		*s = Info
	}
	return nil
}

// Max returns the worse of two severities.
func (s Severity) Max(o Severity) Severity {
	if o > s {
		return o
	}
	return s
}

// SeverityOf returns the base severity of a state: failing and blocked are
// always crit, waiting is warn.
func SeverityOf(s State) Severity {
	switch s {
	case Failing, Blocked:
		return Crit
	case Waiting:
		return Warn
	}
	return Info
}
