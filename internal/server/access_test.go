package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akhilcacharya/alexaproxy/internal/login"
	"github.com/akhilcacharya/alexaproxy/internal/store"
)

func TestAccess(t *testing.T) {
	t.Setenv(store.APIKeyEnv, "")
	dir := t.TempDir()
	st := &store.Store{Dir: dir}
	srv := &Server{Alexa: &Alexa{Store: st}, Login: login.NewManager(login.Options{})}
	h := srv.Handler()

	const (
		tailnetPeer = "100.101.102.103:5555"
		loopback    = "127.0.0.1:5555"
		lan         = "192.168.1.20:5555"
	)
	type req struct {
		name, remote, path string
		headers            map[string]string
		want               int
	}
	funnel := map[string]string{"Tailscale-Funnel-Request": "?1"}
	with := func(base map[string]string, k, v string) map[string]string {
		m := map[string]string{k: v}
		for bk, bv := range base {
			m[bk] = bv
		}
		return m
	}
	run := func(t *testing.T, cases []req) {
		for _, c := range cases {
			r := httptest.NewRequest("GET", c.path, nil)
			r.RemoteAddr = c.remote
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("%s: got %d, want %d (%s)", c.name, w.Code, c.want, w.Body.String())
			}
		}
	}

	t.Run("no key", func(t *testing.T) {
		run(t, []req{
			{"tailnet", tailnetPeer, "/settings", nil, 200},
			{"local", loopback, "/settings", nil, 200},
			{"lan refused", lan, "/settings", nil, 403},
			{"funnel refused without a key", loopback, "/settings", funnel, 403},
			{"funnel can read spec", loopback, "/openapi.json", funnel, 200},
		})
	})

	key, _, err := st.EnsureAPIKey(false)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("with key", func(t *testing.T) {
		run(t, []req{
			{"tailnet needs no key", tailnetPeer, "/settings", nil, 200},
			{"serve with tailnet identity", loopback, "/settings", map[string]string{"Tailscale-User-Login": "me@example.com"}, 200},
			{"local needs key", loopback, "/settings", nil, 401},
			{"local bearer", loopback, "/settings", map[string]string{"Authorization": "Bearer " + key}, 200},
			{"funnel no key", loopback, "/settings", funnel, 401},
			{"funnel wrong key", loopback, "/settings", with(funnel, "X-API-Key", "nope"), 401},
			{"funnel header key", loopback, "/settings", with(funnel, "X-API-Key", key), 200},
			{"funnel bearer lowercase", loopback, "/settings", with(funnel, "Authorization", "bearer "+key), 200},
			{"funnel public docs", loopback, "/agent", funnel, 200},
			{"lan still refused", lan, "/settings", map[string]string{"X-API-Key": key}, 403},
		})
	})

	t.Run("key rotation is picked up", func(t *testing.T) {
		newKey, _, err := st.EnsureAPIKey(true)
		if err != nil {
			t.Fatal(err)
		}
		// Make sure the mtime changes even on coarse filesystems.
		os.Chtimes(filepath.Join(dir, "api_key"), timeNowPlus(), timeNowPlus())
		run(t, []req{
			{"old key rejected", loopback, "/settings", with(funnel, "X-API-Key", key), 401},
			{"new key accepted", loopback, "/settings", with(funnel, "X-API-Key", newKey), 200},
		})
	})
}

func TestBaseURLFromFunnel(t *testing.T) {
	r := httptest.NewRequest("GET", "/openapi.json", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Host = "127.0.0.1:8787"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "box.tail1234.ts.net")
	if got := baseURL(r); got != "https://box.tail1234.ts.net" {
		t.Errorf("baseURL = %q", got)
	}
	r.RemoteAddr = "100.1.2.3:1" // not loopback: forwarded headers ignored
	if got := baseURL(r); got != "http://127.0.0.1:8787" {
		t.Errorf("baseURL from peer = %q", got)
	}
	_ = http.StatusOK
}

func timeNowPlus() (t time.Time) { return time.Now().Add(2 * time.Second) }
