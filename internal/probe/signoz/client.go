// Package signoz implements the SigNoz probe of wassup: signoz.edge reads
// the rate of a counter SigNoz holds, such as the calls one service makes
// to another, and the share of them that failed.
//
// SigNoz answers a query to a POST, and hands out a session to a POST of
// the user's name and password. Both read: the first runs a query, the
// second changes nothing a probe could use to change anything else. They
// are the two requests the guard of this package lets through beside GET;
// every other request is refused before it is sent.
package signoz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/danilopopovikj/wassup/internal/probe"
)

const (
	// contextPath says which organizations a user may sign in to.
	contextPath = "/api/v2/sessions/context"
	// loginPath trades a name and a password for a session.
	loginPath = "/api/v2/sessions/email_password"
	// queryPath runs a query and returns its result.
	queryPath = "/api/v5/query_range"

	// keyHeader carries an API key, where a binding has one.
	keyHeader = "SIGNOZ-API-KEY"

	defaultUserEnv     = "SIGNOZ_USER"
	defaultPasswordEnv = "SIGNOZ_PASSWORD"

	// requestTimeout bounds one request.
	requestTimeout = 15 * time.Second
	// maxBody bounds what is read of an answer.
	maxBody = 16 << 20
)

// reads reports whether a request that is not a GET reads all the same: the
// sign-in and the query, and nothing else.
func reads(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return strings.HasSuffix(r.URL.Path, loginPath) || strings.HasSuffix(r.URL.Path, queryPath)
}

// newHTTPClient builds the one client of this package.
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: requestTimeout, Transport: probe.ReadOnlyExcept(nil, reads)}
}

// apiError is an answer of SigNoz that is not a success.
type apiError struct {
	Status int
	Path   string
	Msg    string
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("SigNoz %s: HTTP %d", e.Path, e.Status)
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

// errConfig is an error of the binding or of the machine's settings: asking
// again does not make it go away.
type errConfig struct{ error }

// misconfigured reports whether an error means the binding can never work
// (no credentials, credentials refused) rather than a moment's trouble.
func misconfigured(err error) bool {
	var ce errConfig
	if errors.As(err, &ce) {
		return true
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden
	}
	return false
}

// credentials name where a session comes from: an API key, or a user and a
// password. They hold the names of environment variables; the values are
// read when they are sent and kept nowhere.
type credentials struct {
	tokenEnv, userEnv, passwordEnv string
}

// credentialsOf reads the names from a spec. A binding that names a key
// uses the key.
func credentialsOf(spec map[string]any) credentials {
	if env := probe.Str(spec, "token_env", ""); env != "" {
		return credentials{tokenEnv: env}
	}
	return credentials{
		userEnv:     probe.Str(spec, "user_env", defaultUserEnv),
		passwordEnv: probe.Str(spec, "password_env", defaultPasswordEnv),
	}
}

// env reads a variable that has to be set.
func env(name, field string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return "", errConfig{fmt.Errorf("environment variable %s (%s) is not set; it belongs in .wassup/local.env", name, field)}
	}
	return v, nil
}

// session is a signed-in client of one SigNoz. The token lives in memory
// and is asked for again when SigNoz no longer takes it.
type session struct {
	base  string
	creds credentials
	http  *http.Client

	mu    sync.Mutex
	token string
}

var (
	sessionsMu sync.Mutex
	sessions   = map[string]*session{}
)

// sessionFor returns the session the bindings of one SigNoz and one set of
// credentials share, so that ten bindings sign in once.
func sessionFor(base string, creds credentials) *session {
	key := base + "\x00" + creds.tokenEnv + "\x00" + creds.userEnv + "\x00" + creds.passwordEnv
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	s := sessions[key]
	if s == nil {
		s = &session{base: base, creds: creds, http: newHTTPClient()}
		sessions[key] = s
	}
	return s
}

// do sends one request and decodes the data of the answer into out.
func (s *session) do(ctx context.Context, method, path string, query url.Values, body any, header http.Header, out any) error {
	u := s.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, payload)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wassup-signoz")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeErr := json.Unmarshal(raw, &envelope)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &apiError{Status: resp.StatusCode, Path: path}
		if decodeErr == nil {
			ae.Msg = envelope.Error.Message
		}
		return ae
	}
	if decodeErr != nil {
		return fmt.Errorf("SigNoz %s: the answer is not JSON: %w", path, decodeErr)
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("SigNoz %s: the answer holds no data", path)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("SigNoz %s: decode: %w", path, err)
	}
	return nil
}

// signIn asks for a session with the user and the password of the
// environment. The caller holds s.mu.
func (s *session) signIn(ctx context.Context) error {
	user, err := env(s.creds.userEnv, "user_env")
	if err != nil {
		return err
	}
	password, err := env(s.creds.passwordEnv, "password_env")
	if err != nil {
		return err
	}
	var who struct {
		Orgs []struct {
			ID string `json:"id"`
		} `json:"orgs"`
	}
	if err := s.do(ctx, http.MethodGet, contextPath, url.Values{"email": {user}, "ref": {s.base}}, nil, nil, &who); err != nil {
		return err
	}
	if len(who.Orgs) == 0 || who.Orgs[0].ID == "" {
		return errConfig{fmt.Errorf("SigNoz knows no organization for the user in %s", s.creds.userEnv)}
	}
	var got struct {
		AccessToken string `json:"accessToken"`
	}
	login := map[string]string{"email": user, "password": password, "orgId": who.Orgs[0].ID}
	if err := s.do(ctx, http.MethodPost, loginPath, nil, login, nil, &got); err != nil {
		return err
	}
	if got.AccessToken == "" {
		return fmt.Errorf("SigNoz %s: the answer holds no token", loginPath)
	}
	s.token = got.AccessToken
	return nil
}

// authorize returns the header that carries the session, signing in first
// when there is none.
func (s *session) authorize(ctx context.Context) (http.Header, error) {
	if s.creds.tokenEnv != "" {
		key, err := env(s.creds.tokenEnv, "token_env")
		if err != nil {
			return nil, err
		}
		return http.Header{keyHeader: {key}}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == "" {
		if err := s.signIn(ctx); err != nil {
			return nil, err
		}
	}
	return http.Header{"Authorization": {"Bearer " + s.token}}, nil
}

// forget drops a token SigNoz refused, unless somebody replaced it already.
func (s *session) forget(h http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if "Bearer "+s.token == h.Get("Authorization") {
		s.token = ""
	}
}

// query runs one query. A session that ran out is asked for once more.
func (s *session) query(ctx context.Context, body, out any) error {
	for attempt := 0; ; attempt++ {
		h, err := s.authorize(ctx)
		if err != nil {
			return err
		}
		err = s.do(ctx, http.MethodPost, queryPath, nil, body, h, out)
		var ae *apiError
		if attempt == 0 && s.creds.tokenEnv == "" && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			s.forget(h)
			continue
		}
		return err
	}
}
