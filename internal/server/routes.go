package server

import (
	"context"
	"net/http"
	"time"

	"github.com/akhilcacharya/alexaproxy/internal/api"
	"github.com/akhilcacharya/alexaproxy/internal/store"
)

// route is both the handler registration and its documentation: GET /,
// /agent and /openapi.json are all rendered from this table, so the docs
// can't drift from what's served.
type route struct {
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	OperationID string   `json:"operation_id"`
	Group       string   `json:"group"`
	Summary     string   `json:"summary"`
	Params      []param  `json:"params,omitempty"`
	Examples    []string `json:"examples,omitempty"` // "$BASE" is replaced with this server's URL
	// NeedsLogin marks endpoints that call Amazon and return 401 login_required when not logged in.
	NeedsLogin bool `json:"needs_login,omitempty"`

	handle handlerFunc
}

type param struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // string, integer, boolean
	Required bool   `json:"required,omitempty"`
	Desc     string `json:"description"`
}

func str(name, desc string) param { return param{Name: name, Type: "string", Desc: desc} }
func reqStr(name, desc string) param {
	return param{Name: name, Type: "string", Desc: desc, Required: true}
}
func integer(name, desc string) param { return param{Name: name, Type: "integer", Desc: desc} }
func boolean(name, desc string) param { return param{Name: name, Type: "boolean", Desc: desc} }

func (s *Server) register() {
	s.routes = []route{
		{
			Method: "GET", Path: "/", OperationID: "docs", Group: "Docs",
			Summary:  "Plain-text API reference with copy-pasteable curl examples (this page).",
			Examples: []string{`curl $BASE/`},
			handle:   s.docsText,
		},
		{
			Method: "GET", Path: "/agent", OperationID: "agentGuide", Group: "Docs",
			Summary: "Guide for AI agents, in Markdown: live login status, device names, which endpoint to use when, " +
				"and how to handle errors. Point an assistant here first.",
			Examples: []string{`curl $BASE/agent`},
			handle:   s.agentGuide,
		},
		{
			Method: "GET", Path: "/llms.txt", OperationID: "llmsTxt", Group: "Docs",
			Summary: "Same as /agent, at the conventional llms.txt path.",
			handle:  s.agentGuide,
		},
		{
			Method: "GET", Path: "/openapi.json", OperationID: "openapi", Group: "Docs",
			Summary:  "OpenAPI 3.1 spec, for tools that import API definitions.",
			Examples: []string{`curl $BASE/openapi.json`},
			handle:   s.openAPI,
		},
		{
			Method: "GET", Path: "/docs.json", OperationID: "docsJSON", Group: "Docs",
			Summary: "The endpoint table as plain JSON.",
			handle:  s.docsJSON,
		},

		{
			Method: "GET", Path: "/status", OperationID: "getStatus", Group: "Status",
			Summary: "Is the Alexa login working? Returns auth.state: ok, unchecked, not_logged_in, or invalid " +
				"(Amazon rejected it). Every response also carries this in the X-Alexa-Auth header. " +
				"Login is re-verified in the background periodically.",
			Params:   []param{boolean("verify", "true to check with Amazon right now instead of using the last result")},
			Examples: []string{`curl $BASE/status`, `curl "$BASE/status?verify=true"`},
			handle:   s.status,
		},
		{
			Method: "GET", Path: "/healthz", OperationID: "healthz", Group: "Status",
			Summary: "Liveness check. Does not contact Amazon.",
			handle: func(http.ResponseWriter, *http.Request, input) (any, error) {
				return map[string]any{"ok": true, "version": s.Version}, nil
			},
		},

		{
			Method: "GET", Path: "/auth/status", OperationID: "getAuthStatus", Group: "Auth",
			Summary:  "Details of the stored credentials (masked token, domain, when saved).",
			Params:   []param{boolean("verify", "true to test the token against Amazon (slower)")},
			Examples: []string{`curl $BASE/auth/status`},
			handle:   s.authStatus,
		},
		{
			Method: "POST", Path: "/auth/login", OperationID: "startLogin", Group: "Auth",
			Summary: "Start a browser login. Returns login_url: a human opens it on a tailnet device and signs in " +
				"to Amazon; the refresh token is captured and saved automatically.",
			Params: []param{
				str("domain", "Amazon auth domain (default amazon.com; amazon.co.jp for Japan)"),
				str("country", "Marketplace, e.g. amazon.co.uk, amazon.de (default: same as domain)"),
			},
			Examples: []string{
				`curl -X POST $BASE/auth/login`,
				`curl -X POST $BASE/auth/login -d country=amazon.co.uk`,
			},
			handle: s.loginStart,
		},
		{
			Method: "GET", Path: "/auth/login", OperationID: "getLogin", Group: "Auth",
			Summary: "State of the current browser login " +
				"(preparing, waiting_for_browser, verifying, succeeded, failed, canceled).",
			Params:   []param{integer("wait", "seconds to block until the login finishes (max 600)")},
			Examples: []string{`curl "$BASE/auth/login?wait=300"`},
			handle:   s.loginStatus,
		},
		{
			Method: "DELETE", Path: "/auth/login", OperationID: "cancelLogin", Group: "Auth",
			Summary:  "Cancel an in-progress browser login.",
			Examples: []string{`curl -X DELETE $BASE/auth/login`},
			handle:   s.loginCancel,
		},
		{
			Method: "POST", Path: "/auth/token", OperationID: "setToken", Group: "Auth",
			Summary: "Save a refresh token you already have (e.g. from `alexacli auth`). It is verified first.",
			Params: []param{
				reqStr("refresh_token", "Atnr|... token"),
				str("domain", "Amazon auth domain (default amazon.com)"),
				str("country", "Marketplace domain (default: same as domain)"),
			},
			Examples: []string{`curl $BASE/auth/token --data-urlencode 'refresh_token=Atnr|...'`},
			handle:   s.authToken,
		},
		{
			Method: "POST", Path: "/auth/logout", OperationID: "logout", Group: "Auth",
			Summary:  "Delete stored credentials.",
			Examples: []string{`curl -X POST $BASE/auth/logout`},
			handle:   s.authLogout,
		},

		{
			Method: "GET", Path: "/settings", OperationID: "getSettings", Group: "Settings",
			Summary: "Current settings, including default_device (null if none). " +
				"default_device.source is \"settings\" (set over this API) or \"flag\" (server command line).",
			Examples: []string{`curl $BASE/settings`},
			handle:   s.getSettings,
		},
		{
			Method: "POST", Path: "/settings/default-device", OperationID: "setDefaultDevice", Group: "Settings",
			NeedsLogin: true,
			Summary: "Set the device used when `device` is omitted from speak/command/ask. " +
				"The name is resolved against the account now and stored by serial; persists across restarts.",
			Params:   []param{reqStr("device", "device name, unique part of a name, or serial (see /devices)")},
			Examples: []string{`curl $BASE/settings/default-device -d device=Kitchen`},
			handle:   s.setDefaultDevice,
		},
		{
			Method: "DELETE", Path: "/settings/default-device", OperationID: "clearDefaultDevice", Group: "Settings",
			Summary:  "Clear the default device, so `device` is required again (unless the server has --default-device).",
			Examples: []string{`curl -X DELETE $BASE/settings/default-device`},
			handle:   s.clearDefaultDevice,
		},

		{
			Method: "GET", Path: "/devices", OperationID: "listDevices", Group: "Alexa", NeedsLogin: true,
			Summary:  "List Echo devices on the account. Use a name (or unique part of one) or serial as `device` elsewhere.",
			Examples: []string{`curl $BASE/devices`},
			handle:   s.devices,
		},
		{
			Method: "POST", Path: "/speak", OperationID: "speak", Group: "Alexa", NeedsLogin: true,
			Summary: "Make one device say the given text out loud, word for word (text-to-speech).",
			Params: []param{
				reqStr("text", "exactly what to say"),
				s.deviceParam(),
			},
			Examples: []string{`curl $BASE/speak -d device=Kitchen --data-urlencode 'text=Dinner is ready'`},
			handle:   s.speak,
		},
		{
			Method: "POST", Path: "/announce", OperationID: "announce", Group: "Alexa", NeedsLogin: true,
			Summary:  "Announce text on every device in the house at once.",
			Params:   []param{reqStr("text", "what to announce")},
			Examples: []string{`curl $BASE/announce --data-urlencode 'text=Leaving in 5 minutes'`},
			handle:   s.announce,
		},
		{
			Method: "POST", Path: "/command", OperationID: "command", Group: "Alexa", NeedsLogin: true,
			Summary: "Send a voice command as if spoken to the device: smart home, music, timers, routines. " +
				"Fire-and-forget; use /ask to get the reply.",
			Params: []param{
				reqStr("text", `the command as you'd say it, without "Alexa"`),
				s.deviceParam(),
			},
			Examples: []string{`curl $BASE/command -d device=Kitchen --data-urlencode 'text=turn off the living room lights'`},
			handle:   s.command,
		},
		{
			Method: "POST", Path: "/ask", OperationID: "ask", Group: "Alexa", NeedsLogin: true,
			Summary: "Send a voice command and return Alexa's reply as text. " +
				"The device also says the reply out loud.",
			Params: []param{
				reqStr("text", `the question as you'd say it, without "Alexa"`),
				s.deviceParam(),
				integer("timeout", "seconds to wait for the reply (default 15, max 60)"),
			},
			Examples: []string{`curl $BASE/ask -d device=Kitchen --data-urlencode "text=what's the weather"`},
			handle:   s.ask,
		},
		{
			Method: "GET", Path: "/history", OperationID: "history", Group: "Alexa", NeedsLogin: true,
			Summary:  "Recent voice activity (last 24h): what was said and how Alexa responded.",
			Params:   []param{integer("limit", "max records (default 10)")},
			Examples: []string{`curl "$BASE/history?limit=5"`},
			handle:   s.history,
		},
	}
}

func (s *Server) deviceParam() param {
	return str("device", "device name, unique part of a name, or serial (see /devices); "+
		"optional when a default device is set (see /settings)")
}

// ---- status ----

func (s *Server) status(_ http.ResponseWriter, r *http.Request, in input) (any, error) {
	if in.boolean("verify") {
		s.Alexa.Devices() // result lands in the auth state
	}
	auth := s.Alexa.Auth()
	out := map[string]any{
		"ok":             auth.OK(),
		"auth":           auth,
		"default_device": s.defaultDevice(),
		"version":        s.Version,
		"api_key_set":    s.Alexa.Store.APIKey() != "",
		"you_came_from":  requestSource(r),
	}
	if !auth.OK() {
		out["how_to_fix"] = howToFixLogin
	}
	if sess, ok := s.Login.Current(); ok && !sess.Done() {
		out["login_in_progress"] = sess
	}
	if devs := s.Alexa.CachedDevices(); devs != nil {
		out["devices"] = toDeviceJSON(devs)
	}
	return out, nil
}

// ---- auth ----

func (s *Server) authStatus(_ http.ResponseWriter, _ *http.Request, in input) (any, error) {
	st := s.Alexa.Store
	creds, err := st.Load()
	if err == store.ErrNotLoggedIn {
		out := map[string]any{"logged_in": false, "credentials_path": st.Path(),
			"hint": "POST /auth/login to sign in"}
		if sess, ok := s.Login.Current(); ok && !sess.Done() {
			out["login"] = sess
		}
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"logged_in":        true,
		"credentials_path": st.Path(),
		"token":            store.MaskToken(creds.RefreshToken),
		"domain":           creds.AmazonDomain,
		"country":          creds.AmazonLocal,
		"saved_at":         creds.SavedAt,
	}
	if in.boolean("verify") {
		devs, err := s.Alexa.Devices()
		if err != nil {
			out["valid"], out["error"] = false, err.Error()
			out["hint"] = "token may have expired; POST /auth/login to sign in again"
		} else {
			out["valid"], out["devices"] = true, len(devs)
		}
	}
	return out, nil
}

func (s *Server) loginStart(w http.ResponseWriter, _ *http.Request, in input) (any, error) {
	sess, err := s.Login.Start(in.str("domain"), in.str("country"))
	if err != nil {
		return nil, err
	}
	writeJSON(w, http.StatusAccepted, sess)
	return nil, nil
}

func (s *Server) loginStatus(_ http.ResponseWriter, r *http.Request, in input) (any, error) {
	wait, err := in.integer("wait", 0)
	if err != nil {
		return nil, err
	}
	if _, ok := s.Login.Current(); !ok {
		return nil, &httpError{404, "no login has been started; POST /auth/login"}
	}
	if wait > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(min(wait, 600))*time.Second)
		defer cancel()
		s.Login.Wait(ctx)
	}
	sess, _ := s.Login.Current()
	return sess, nil
}

func (s *Server) loginCancel(http.ResponseWriter, *http.Request, input) (any, error) {
	sess, ok := s.Login.Cancel()
	if !ok {
		return nil, &httpError{404, "no login in progress"}
	}
	return sess, nil
}

func (s *Server) authToken(_ http.ResponseWriter, _ *http.Request, in input) (any, error) {
	token, err := in.require("refresh_token")
	if err != nil {
		return nil, err
	}
	n, err := s.Alexa.Login(token, in.str("domain"), in.str("country"))
	if err != nil {
		return nil, &httpError{400, "token rejected: " + err.Error()}
	}
	return map[string]any{"ok": true, "devices": n, "credentials_path": s.Alexa.Store.Path()}, nil
}

func (s *Server) authLogout(http.ResponseWriter, *http.Request, input) (any, error) {
	if err := s.Alexa.Logout(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// ---- alexa ----

type deviceJSON struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	Family string `json:"family"`
	Type   string `json:"type"`
	Online bool   `json:"online"`
}

func toDeviceJSON(devs []api.Device) []deviceJSON {
	out := make([]deviceJSON, 0, len(devs))
	for _, d := range devs {
		out = append(out, deviceJSON{deviceName(d), d.SerialNumber, d.DeviceFamily, d.DeviceType, d.Online})
	}
	return out
}

func (s *Server) devices(http.ResponseWriter, *http.Request, input) (any, error) {
	devs, err := s.Alexa.Devices()
	if err != nil {
		return nil, err
	}
	return toDeviceJSON(devs), nil
}

// withDevice resolves the device parameter and runs fn with it.
func (s *Server) withDevice(in input, fn func(*api.Client, *api.Device) error) (string, error) {
	query := in.str("device")
	if query == "" {
		def := s.defaultDevice()
		if def == nil {
			return "", badRequest("missing parameter \"device\" and no default device is set; " +
				"pass device=... (see GET /devices) or set one with POST /settings/default-device")
		}
		query = def.Serial
		if query == "" {
			query = def.Name // from --default-device
		}
	}
	var name string
	err := s.Alexa.Do(func(c *api.Client) error {
		devs, err := c.GetDevices()
		if err != nil {
			return err
		}
		d, err := findDevice(devs, query)
		if err != nil {
			return err
		}
		name = deviceName(*d)
		return fn(c, d)
	})
	return name, err
}

func (s *Server) speak(_ http.ResponseWriter, _ *http.Request, in input) (any, error) {
	text, err := in.require("text")
	if err != nil {
		return nil, err
	}
	name, err := s.withDevice(in, func(c *api.Client, d *api.Device) error {
		return c.SequenceCommand(d, "speak:"+text)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "device": name, "text": text}, nil
}

func (s *Server) announce(_ http.ResponseWriter, _ *http.Request, in input) (any, error) {
	text, err := in.require("text")
	if err != nil {
		return nil, err
	}
	err = s.Alexa.Do(func(c *api.Client) error {
		devs, err := c.GetDevices()
		if err != nil {
			return err
		}
		if len(devs) == 0 {
			return &httpError{404, "no devices on this account"}
		}
		return c.SequenceCommand(&devs[0], "announcement:"+text)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "text": text}, nil
}

func (s *Server) command(_ http.ResponseWriter, _ *http.Request, in input) (any, error) {
	text, err := in.require("text")
	if err != nil {
		return nil, err
	}
	name, err := s.withDevice(in, func(c *api.Client, d *api.Device) error {
		return c.SequenceCommand(d, "textcommand:"+text)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "device": name, "text": text}, nil
}

func (s *Server) ask(_ http.ResponseWriter, _ *http.Request, in input) (any, error) {
	text, err := in.require("text")
	if err != nil {
		return nil, err
	}
	timeout, err := in.integer("timeout", 15)
	if err != nil {
		return nil, err
	}
	var reply string
	name, err := s.withDevice(in, func(c *api.Client, d *api.Device) error {
		var askErr error
		reply, askErr = c.Ask(d, text, time.Duration(min(timeout, 60))*time.Second)
		return askErr
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "device": name, "question": text, "response": reply}, nil
}

func (s *Server) history(_ http.ResponseWriter, _ *http.Request, in input) (any, error) {
	limit, err := in.integer("limit", 10)
	if err != nil {
		return nil, err
	}
	var recs []api.HistoryRecord
	err = s.Alexa.Do(func(c *api.Client) error {
		// GetDevices populates the customer ID the history API needs.
		if _, err := c.GetDevices(); err != nil {
			return err
		}
		end := time.Now()
		var histErr error
		recs, histErr = c.GetCustomerHistoryRecords(end.Add(-24*time.Hour).UnixMilli(), end.UnixMilli())
		return histErr
	})
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(recs) > limit {
		recs = recs[:limit]
	}
	return recs, nil
}
