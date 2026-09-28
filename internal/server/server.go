// Package server exposes Alexa over a small, self-documenting HTTP API.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akhilcacharya/alexaproxy/internal/login"
	"github.com/akhilcacharya/alexaproxy/internal/store"
)

type Server struct {
	Alexa         *Alexa
	Login         *login.Manager
	DefaultDevice string
	// AllowAll accepts clients outside the tailnet (they need the API key
	// if one is configured).
	AllowAll bool
	Version  string

	routes []route
	mux    *http.ServeMux

	settingsMu sync.Mutex
	settings   *store.Settings // loaded lazily from the state dir
}

// Handler builds the HTTP handler. Call once.
func (s *Server) Handler() http.Handler {
	s.mux = http.NewServeMux()
	s.register()
	for _, rt := range s.routes {
		pattern := rt.Path
		if pattern == "/" {
			pattern = "/{$}" // exact match, not a catch-all
		}
		s.mux.HandleFunc(rt.Method+" "+pattern, s.wrap(rt.handle))
	}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, &httpError{404, fmt.Sprintf("no route %s %s; GET / lists all endpoints, GET /agent explains them", r.Method, r.URL.Path)})
	})
	return logRequests(s.access(s.authHeader(s.mux)))
}

// httpError carries a status code through handler returns.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &httpError{400, fmt.Sprintf(format, args...)}
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, in input) (any, error)

func (s *Server) wrap(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, err := parseInput(r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		v, err := h(w, r, in)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if v != nil {
			writeJSON(w, http.StatusOK, v)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// loginRequired is the 401 body returned whenever Amazon credentials are
// missing or rejected, so callers (and agents) can tell the user what to do.
type loginRequired struct {
	OK            bool      `json:"ok"`
	Error         string    `json:"error"`
	LoginRequired bool      `json:"login_required"`
	Auth          AuthState `json:"auth"`
	HowToFix      string    `json:"how_to_fix"`
}

const howToFixLogin = "The Alexa login is missing or expired and a human must sign in. " +
	"POST /auth/login, then open the returned login_url in a browser on a tailnet device and sign in to Amazon " +
	"(signing in with the account's mobile number is most reliable; otherwise type the email by hand; " +
	"if offered a passkey, choose to sign in with a password). " +
	"GET /auth/login?wait=300 blocks until it finishes."

func (s *Server) writeError(w http.ResponseWriter, err error) {
	var he *httpError
	if !errors.As(err, &he) && isLoginProblem(err) { // handlers' explicit statuses win
		auth := s.Alexa.Auth()
		w.Header().Set("X-Alexa-Auth", auth.State)
		writeJSON(w, http.StatusUnauthorized, loginRequired{
			Error: err.Error(), LoginRequired: true, Auth: auth, HowToFix: howToFixLogin,
		})
		return
	}
	writeError(w, err)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway // most failures are Amazon-side
	var he *httpError
	if errors.As(err, &he) {
		status = he.status
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}

// authHeader tags every response with the current login state, so any
// caller can notice a lapsed login without polling /status.
func (s *Server) authHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Alexa-Auth", s.Alexa.Auth().State)
		next.ServeHTTP(w, r)
	})
}

// input merges query parameters with a form or JSON request body, so every
// endpoint can be called with whichever style is handiest from curl.
type input map[string]string

func (in input) str(key string) string { return strings.TrimSpace(in[key]) }

func (in input) boolean(key string) bool {
	b, _ := strconv.ParseBool(in.str(key))
	return b || in.str(key) == "1" || in.str(key) == "yes"
}

func (in input) integer(key string, def int) (int, error) {
	v := in.str(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, badRequest("%s must be an integer", key)
	}
	return n, nil
}

func (in input) require(key string) (string, error) {
	v := in.str(key)
	if v == "" {
		return "", badRequest("missing required parameter %q", key)
	}
	return v, nil
}

func parseInput(r *http.Request) (input, error) {
	in := input{}
	for k, v := range r.URL.Query() {
		in[k] = v[len(v)-1]
	}
	if r.Body == nil || r.Method == http.MethodGet {
		return in, nil
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, badRequest("read body: %v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return in, nil
	}
	switch {
	case ct == "application/json" || strings.HasPrefix(strings.TrimSpace(string(body)), "{"):
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, badRequest("invalid JSON body: %v", err)
		}
		for k, v := range m {
			switch v := v.(type) {
			case string:
				in[k] = v
			case nil:
			default:
				b, _ := json.Marshal(v)
				in[k] = string(b)
			}
		}
	case ct == "text/plain":
		in["text"] = string(body)
	default: // curl -d defaults to application/x-www-form-urlencoded
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if err := r.ParseForm(); err != nil {
			return nil, badRequest("invalid form body: %v", err)
		}
		for k, v := range r.PostForm {
			in[k] = v[len(v)-1]
		}
	}
	return in, nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		from := r.RemoteAddr
		if src := requestSource(r); src == fromFunnel || src == fromTailnet && r.Header.Get("X-Forwarded-For") != "" {
			from = src + ":" + r.Header.Get("X-Forwarded-For")
		}
		log.Printf("%s %s %s -> %d (%s)", from, r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}
