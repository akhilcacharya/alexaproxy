package server

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/akhilcacharya/alexaproxy/internal/tailnet"
)

// Where a request came from, which decides whether it needs the API key.
const (
	fromTailnet = "tailnet" // direct connection from a tailnet peer, or via `tailscale serve` with a user identity
	fromFunnel  = "funnel"  // public internet via Tailscale Funnel
	fromLocal   = "local"   // loopback: this machine, or a local reverse proxy
	fromOther   = "other"   // anything else (LAN, etc.)
)

type ctxKey int

const trustedKey ctxKey = 0

// trusted reports whether the caller may see account details (device names,
// serials) on the public docs pages.
func trusted(r *http.Request) bool {
	t, _ := r.Context().Value(trustedKey).(bool)
	return t
}

func requestSource(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return fromOther
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fromOther
	}
	addr = addr.Unmap()
	switch {
	case addr.IsLoopback():
		// tailscaled's serve/funnel proxy connects over loopback and strips
		// any client-supplied copies of these headers before setting them.
		if r.Header.Get("Tailscale-Funnel-Request") != "" {
			return fromFunnel
		}
		if r.Header.Get("Tailscale-User-Login") != "" {
			return fromTailnet
		}
		return fromLocal
	case tailnet.Contains(addr):
		return fromTailnet
	}
	return fromOther
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

func hasValidKey(r *http.Request, key string) bool {
	got := r.Header.Get("X-API-Key")
	if auth := r.Header.Get("Authorization"); got == "" && auth != "" {
		if scheme, token, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
			got = strings.TrimSpace(token)
		}
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}

// publicPaths are readable by anyone who can reach the server, so tools like
// Muse can fetch the spec before they have credentials.
var publicPaths = map[string]bool{
	"/": true, "/agent": true, "/llms.txt": true, "/openapi.json": true, "/docs.json": true, "/healthz": true,
}

const apiKeyHint = "Send the API key as 'Authorization: Bearer <key>' or 'X-API-Key: <key>'. " +
	"The server's owner can print it with `alexaproxy apikey`."

// access enforces who may call the API:
//   - tailnet peers: always allowed (the tailnet is the trust boundary)
//   - Funnel (public internet): API key required; refused if none is configured
//   - loopback / other proxies: allowed without a key only if no key is configured
//   - anything else: refused unless --allow-all, then treated like loopback
func (s *Server) access(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src := requestSource(r)
		key := s.Alexa.Store.APIKey()
		authorized := key != "" && hasValidKey(r, key)
		if src == fromOther && !s.AllowAll {
			writeError(w, &httpError{403, "only reachable from the tailnet or localhost"})
			return
		}

		var trust bool
		switch src {
		case fromTailnet:
			trust = true
		case fromFunnel:
			trust = authorized
		default:
			trust = key == "" || authorized
		}

		if !trust && !publicPaths[r.URL.Path] {
			if src == fromFunnel && key == "" {
				writeJSON(w, http.StatusForbidden, map[string]any{"ok": false,
					"error": "public (Tailscale Funnel) requests are refused until the server owner creates an API key " +
						"with `alexaproxy apikey`"})
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="alexaproxy"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false,
				"error": "missing or invalid API key", "api_key_required": true, "hint": apiKeyHint})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), trustedKey, trust)))
	})
}
