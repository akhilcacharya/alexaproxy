package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akhilcacharya/alexaproxy/internal/login"
	"github.com/akhilcacharya/alexaproxy/internal/store"
)

// newTestServer returns a server with an empty state dir (not logged in).
func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	t.Setenv(store.APIKeyEnv, "")
	srv := &Server{
		Alexa:   &Alexa{Store: &store.Store{Dir: t.TempDir()}},
		Login:   login.NewManager(login.Options{Host: "127.0.0.1"}),
		Version: "test",
	}
	return srv, srv.Handler()
}

type response struct {
	code   int
	header http.Header
	body   string
	json   map[string]any
}

func do(t *testing.T, h http.Handler, method, target, contentType, body string) response {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "100.100.100.100:1234" // a tailnet peer
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	resp := response{code: w.Code, header: w.Header(), body: w.Body.String()}
	json.Unmarshal(w.Body.Bytes(), &resp.json)
	return resp
}

func TestNotLoggedIn(t *testing.T) {
	_, h := newTestServer(t)
	for _, path := range []string{"/devices", "/history"} {
		r := do(t, h, "GET", path, "", "")
		if r.code != 401 || r.json["login_required"] != true || r.json["how_to_fix"] == "" {
			t.Errorf("%s: %d %s", path, r.code, r.body)
		}
		if got := r.header.Get("X-Alexa-Auth"); got != AuthNotLoggedIn {
			t.Errorf("%s: X-Alexa-Auth = %q", path, got)
		}
	}
	r := do(t, h, "GET", "/status", "", "")
	auth, _ := r.json["auth"].(map[string]any)
	if r.code != 200 || r.json["ok"] != false || auth["state"] != AuthNotLoggedIn || r.json["how_to_fix"] == nil {
		t.Errorf("/status: %d %s", r.code, r.body)
	}
	if !strings.Contains(do(t, h, "GET", "/", "", "").body, "NOT LOGGED IN") {
		t.Error("GET / should warn when not logged in")
	}
}

func TestInputStyles(t *testing.T) {
	// A missing `text` is reported before any Amazon call, so this exercises
	// parsing without credentials.
	_, h := newTestServer(t)
	for name, c := range map[string]struct{ ct, target, body string }{
		"form":       {"application/x-www-form-urlencoded", "/speak", "device=Kitchen"},
		"json":       {"application/json", "/speak", `{"device": "Kitchen", "timeout": 5}`},
		"json no ct": {"", "/speak", `{"device": "Kitchen"}`},
		"query":      {"", "/speak?device=Kitchen", ""},
	} {
		r := do(t, h, "POST", c.target, c.ct, c.body)
		if r.code != 400 || !strings.Contains(r.body, `missing required parameter \"text\"`) {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	if r := do(t, h, "POST", "/speak", "application/json", `{bad`); r.code != 400 || !strings.Contains(r.body, "invalid JSON") {
		t.Errorf("bad JSON: %d %s", r.code, r.body)
	}
	// text/plain bodies become `text`; with no device and no default we get
	// the default-device hint instead.
	r := do(t, h, "POST", "/speak", "text/plain", "hello there")
	if r.code != 400 || !strings.Contains(r.body, "no default device is set") {
		t.Errorf("text/plain: %d %s", r.code, r.body)
	}
}

func TestSettings(t *testing.T) {
	srv, h := newTestServer(t)
	if r := do(t, h, "GET", "/settings", "", ""); r.code != 200 || r.json["default_device"] != nil {
		t.Fatalf("empty settings: %d %s", r.code, r.body)
	}
	if r := do(t, h, "POST", "/settings/default-device", "", ""); r.code != 400 {
		t.Errorf("set without device: %d %s", r.code, r.body)
	}
	if r := do(t, h, "DELETE", "/settings/default-device", "", ""); r.code != 200 {
		t.Errorf("clear: %d %s", r.code, r.body)
	}

	// A stored default wins over the flag.
	srv.DefaultDevice = "FromFlag"
	r := do(t, h, "GET", "/settings", "", "")
	if d, _ := r.json["default_device"].(map[string]any); d["name"] != "FromFlag" || d["source"] != "flag" {
		t.Errorf("flag default: %s", r.body)
	}
	if err := srv.saveDefaultDevice(&store.DeviceRef{Name: "Kitchen", Serial: "G1"}); err != nil {
		t.Fatal(err)
	}
	r = do(t, h, "GET", "/settings", "", "")
	if d, _ := r.json["default_device"].(map[string]any); d["serial"] != "G1" || d["source"] != "settings" {
		t.Errorf("stored default: %s", r.body)
	}
	// ...and it was persisted.
	st, err := srv.Alexa.Store.LoadSettings()
	if err != nil || st.DefaultDevice == nil || st.DefaultDevice.Serial != "G1" {
		t.Errorf("not persisted: %+v %v", st, err)
	}
}

func TestUnknownRoute(t *testing.T) {
	_, h := newTestServer(t)
	if r := do(t, h, "GET", "/nope", "", ""); r.code != 404 || !strings.Contains(r.body, "GET / lists all endpoints") {
		t.Errorf("%d %s", r.code, r.body)
	}
	if r := do(t, h, "GET", "/speak", "", ""); r.code != 404 && r.code != 405 {
		t.Errorf("wrong method: %d", r.code)
	}
}

// Every route must show up in every docs format, since they're all
// generated from the same table.
func TestDocsCoverEveryRoute(t *testing.T) {
	srv, h := newTestServer(t)
	text := do(t, h, "GET", "/", "", "").body
	agent := do(t, h, "GET", "/agent", "", "").body
	spec := do(t, h, "GET", "/openapi.json", "", "")
	if spec.json["openapi"] != "3.1.0" {
		t.Fatalf("openapi: %s", spec.body[:min(200, len(spec.body))])
	}
	paths, _ := spec.json["paths"].(map[string]any)
	ids := map[string]bool{}
	for _, rt := range srv.routes {
		if !strings.Contains(text, rt.Method+" "+rt.Path+"\n") {
			t.Errorf("GET / is missing %s %s", rt.Method, rt.Path)
		}
		if rt.Group != "Docs" && !strings.Contains(agent, "`"+rt.Method+" "+rt.Path+"`") {
			t.Errorf("/agent is missing %s %s", rt.Method, rt.Path)
		}
		op, _ := paths[rt.Path].(map[string]any)[strings.ToLower(rt.Method)].(map[string]any)
		if op == nil {
			t.Errorf("openapi is missing %s %s", rt.Method, rt.Path)
			continue
		}
		id, _ := op["operationId"].(string)
		if id == "" || ids[id] {
			t.Errorf("%s %s: operationId %q missing or duplicated", rt.Method, rt.Path, id)
		}
		ids[id] = true
		for _, p := range rt.Params {
			if p.Type == "" {
				t.Errorf("%s %s param %s has no type", rt.Method, rt.Path, p.Name)
			}
		}
	}
	if !strings.Contains(agent, "Alexa calls will fail until a human signs in") {
		t.Error("/agent should tell agents that login is required")
	}
}

func TestOpenAPISecurityFollowsAPIKey(t *testing.T) {
	srv, h := newTestServer(t)
	spec := do(t, h, "GET", "/openapi.json", "", "")
	if strings.Contains(spec.body, `"security"`) {
		t.Error("no API key configured: operations should not require security")
	}
	if _, _, err := srv.Alexa.Store.EnsureAPIKey(false); err != nil {
		t.Fatal(err)
	}
	spec = do(t, h, "GET", "/openapi.json", "", "")
	paths := spec.json["paths"].(map[string]any)
	speak := paths["/speak"].(map[string]any)["post"].(map[string]any)
	if speak["security"] == nil {
		t.Error("with an API key, /speak should declare security")
	}
	docs := paths["/openapi.json"].(map[string]any)["get"].(map[string]any)
	if docs["security"] != nil {
		t.Error("the spec itself must stay public")
	}
}
