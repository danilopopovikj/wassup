package probe

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrReadOnly is the error of a request, a statement or a command that was
// refused because it could change its source. wassup only reads: what is
// refused never leaves the process.
var ErrReadOnly = errors.New("wassup is read only")

// ReadOnly wraps the transport of a probe's HTTP client so that only GET and
// HEAD are sent; nil wraps the default transport. Every probe builds its
// client on it. The methods a probe uses are constants today; the wrapper is
// there for the day somebody adds one that is not.
func ReadOnly(next http.RoundTripper) http.RoundTripper {
	return ReadOnlyExcept(next, nil)
}

// ReadOnlyExcept is ReadOnly with one way out: a request allow accepts is
// sent whatever its method. It is for a request that reads although its
// method says otherwise, such as the POST that opens a port-forward.
func ReadOnlyExcept(next http.RoundTripper, allow func(*http.Request) bool) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &readOnlyTransport{next: next, allow: allow}
}

// readOnlyTransport refuses the requests that do not read.
type readOnlyTransport struct {
	next  http.RoundTripper
	allow func(*http.Request) bool
}

// RoundTrip implements http.RoundTripper.
func (t *readOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead:
	default:
		if t.allow == nil || !t.allow(req) {
			// A round tripper closes the body of a request it does not send.
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, fmt.Errorf("%w: %s %s was not sent", ErrReadOnly, req.Method, req.URL.Path)
		}
	}
	return t.next.RoundTrip(req)
}

// WrappedRoundTripper returns the transport underneath, for the libraries
// that look through a wrapper for the TLS settings or to close idle
// connections.
func (t *readOnlyTransport) WrappedRoundTripper() http.RoundTripper { return t.next }
