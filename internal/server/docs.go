package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// baseURL is the URL the caller used to reach us. Forwarded headers are
// only trusted from loopback, i.e. from tailscale serve/funnel or a local proxy.
func baseURL(r *http.Request) string {
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	if isLoopback(r) {
		if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
			scheme = p
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = h
		}
	}
	return scheme + "://" + host
}

func expand(ex, base string) string { return strings.ReplaceAll(ex, "$BASE", base) }

// authLine is a one-line human summary of the login state.
func (s *Server) authLine(base string) string {
	a := s.Alexa.Auth()
	switch a.State {
	case AuthOK:
		line := "OK — logged in to Amazon"
		if a.CheckedAt != nil {
			line += fmt.Sprintf(" (verified %s ago)", time.Since(*a.CheckedAt).Round(time.Minute))
		}
		return line
	case AuthUnchecked:
		return "login stored, not verified yet"
	case AuthInvalid:
		return "EXPIRED — Amazon rejected the stored login. Sign in again: curl -X POST " + base + "/auth/login"
	default:
		return "NOT LOGGED IN — start with: curl -X POST " + base + "/auth/login"
	}
}

func (s *Server) docsJSON(_ http.ResponseWriter, r *http.Request, _ input) (any, error) {
	base := baseURL(r)
	routes := make([]route, len(s.routes))
	for i, rt := range s.routes {
		rt.Examples = append([]string(nil), rt.Examples...)
		for j, ex := range rt.Examples {
			rt.Examples[j] = expand(ex, base)
		}
		routes[i] = rt
	}
	return map[string]any{
		"name":     "alexaproxy",
		"version":  s.Version,
		"base_url": base,
		"auth":     s.Alexa.Auth(),
		"input": "Parameters may be sent as query string, form body (curl -d), or JSON body. " +
			"A text/plain body is treated as the `text` parameter.",
		"output":    "JSON. Errors are {\"ok\": false, \"error\": \"...\"} with a non-2xx status.",
		"endpoints": routes,
	}, nil
}

func (s *Server) docsText(w http.ResponseWriter, r *http.Request, _ input) (any, error) {
	base := baseURL(r)
	var b strings.Builder
	fmt.Fprintf(&b, "alexaproxy %s — control Alexa over HTTP from your tailnet\n", s.Version)
	fmt.Fprintf(&b, "%s\n\n", strings.Repeat("=", 60))

	fmt.Fprintf(&b, "Login:    %s\n", s.authLine(base))
	if trusted(r) {
		def := "none (set one: curl " + base + "/settings/default-device -d device=NAME)"
		if d := s.defaultDevice(); d != nil {
			def = d.Name
		}
		fmt.Fprintf(&b, "Default device: %s\n", def)
	}
	fmt.Fprintf(&b, "Base URL: %s\n", base)
	fmt.Fprintf(&b, "Input:    query string, form (curl -d / --data-urlencode), or JSON body\n")
	fmt.Fprintf(&b, "Output:   JSON; errors are {\"ok\": false, \"error\": ...} with non-2xx status\n")
	fmt.Fprintf(&b, "Login state is in every response's X-Alexa-Auth header (ok / not_logged_in / invalid)\n")
	fmt.Fprintf(&b, "AI agents: read %s/agent   OpenAPI: %s/openapi.json\n", base, base)

	b.WriteString(`
Logging in
----------
  1. curl -X POST ` + base + `/auth/login
  2. Open the "login_url" it returns in a browser on any tailnet device and
     sign in to Amazon (use -d country=amazon.co.uk etc. outside the US).
     Sign in with your mobile number if you can (most reliable; otherwise
     type the email by hand), and pick "sign in with password" over passkeys.
  3. curl "` + base + `/auth/login?wait=300"   # blocks until it succeeds
  The refresh token is saved to disk and reused across restarts.
`)

	group := ""
	for _, rt := range s.routes {
		if rt.Group != group {
			group = rt.Group
			fmt.Fprintf(&b, "\n%s\n%s\n", group, strings.Repeat("-", len(group)))
		}
		fmt.Fprintf(&b, "\n%s %s\n", rt.Method, rt.Path)
		fmt.Fprintf(&b, "  %s\n", rt.Summary)
		for _, p := range rt.Params {
			req := ""
			if p.Required {
				req = " (required)"
			}
			fmt.Fprintf(&b, "    %-14s %s%s\n", p.Name, p.Desc, req)
		}
		for _, ex := range rt.Examples {
			fmt.Fprintf(&b, "  $ %s\n", expand(ex, base))
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(b.String()))
	return nil, nil
}

// agentGuide is written for LLM agents: current state first, then how to
// pick an endpoint, then the reference. It is Markdown served as text/plain
// so it reads cleanly through curl and fetch tools alike.
func (s *Server) agentGuide(w http.ResponseWriter, r *http.Request, _ input) (any, error) {
	base := baseURL(r)
	auth := s.Alexa.Auth()
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	p("# alexaproxy: Alexa control API\n\n")
	p("This HTTP API controls the user's Amazon Alexa / Echo devices. Base URL: `%s`\n", base)
	p("It is only reachable from the user's Tailscale network (tailnet). Use `curl` or any HTTP client.\n\n")

	p("## Current status\n\n")
	p("- **Login:** `%s` (%s)\n", auth.State, s.authLine(base))
	if !auth.OK() {
		p("- **Alexa calls will fail until a human signs in.** Do not retry them. Tell the user:\n")
		p("  1. Run `curl -X POST %s/auth/login` (you may do this for them) and give them the `login_url` it returns.\n", base)
		p("  2. They open it in a browser on a tailnet device and sign in to Amazon ")
		p("(signing in with the account's mobile number is most reliable; otherwise type the email by hand; ")
		p("if offered a passkey, choose to sign in with a password).\n")
		p("  3. `curl \"%s/auth/login?wait=300\"` returns `\"state\": \"succeeded\"` when done.\n", base)
	}
	if d := s.defaultDevice(); d != nil && trusted(r) {
		p("- **Default device:** %s — used when `device` is omitted.\n", d.Name)
	} else if d != nil {
		p("- **Default device:** set (see `GET /settings` with the API key) — `device` may be omitted.\n")
	} else {
		p("- **Default device:** none set — every speak/command/ask call needs `device`.\n")
	}
	p("- To change the default device (e.g. when the user says \"use the kitchen Echo from now on\"): ")
	p("`POST %s/settings/default-device` with `device=<name>`. Clear it with `DELETE` on the same path.\n\n", base)

	if devs := s.Alexa.CachedDevices(); len(devs) > 0 && !trusted(r) {
		p("## Devices\n\nCall this page with the API key to see the device list, or use `GET %s/devices`.\n\n", base)
	} else if len(devs) > 0 {
		p("## Devices\n\n")
		p("Pass a name, any unique part of one, or the serial as `device`. ")
		p("Duplicate names must be addressed by serial. Live list: `GET %s/devices`.\n\n", base)
		sorted := toDeviceJSON(devs)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Online && !sorted[j].Online })
		p("| name | serial | online | kind |\n|---|---|---|---|\n")
		for _, d := range sorted {
			online := "no"
			if d.Online {
				online = "yes"
			}
			p("| %s | %s | %s | %s |\n", d.Name, d.Serial, online, d.Family)
		}
		p("\n")
	}

	p("## Which endpoint to use\n\n")
	p("- **Get information** (weather, time, calendar, is the door locked, thermostat setting): ")
	p("`POST /ask` — returns Alexa's answer as text in `response`. The device also says it out loud.\n")
	p("- **Do something** (lights, plugs, thermostat, music, timers, routines): `POST /command` with the phrase ")
	p("you would say to Alexa, minus the word \"Alexa\". It doesn't return Alexa's reply; use `/ask` if you need confirmation.\n")
	p("- **Say exact words** on one device: `POST /speak`. On every device at once: `POST /announce` ")
	p("(only when the user asks for a house-wide announcement).\n")
	p("- **What happened recently:** `GET /history`.\n\n")

	p("## Calling conventions\n\n")
	if s.Alexa.Store.APIKey() != "" {
		p("- **Authentication:** send the API key as `Authorization: Bearer <key>` (or `X-API-Key: <key>`). ")
		p("Requests without it get HTTP 401 with `\"api_key_required\": true`. ")
		p("Connections made directly over the owner's tailnet don't need it. Never put the key in a URL.\n")
	}
	p("- Send parameters as form fields (`curl -d key=value`, `--data-urlencode` for free text) or a JSON body.\n")
	p("- Responses are JSON. Success has `\"ok\": true`. Failures have `\"ok\": false` and an `error` message.\n")
	p("- HTTP 401 with `\"login_required\": true` means the Amazon login is missing or expired: ")
	p("stop and relay `how_to_fix` to the user.\n")
	p("- HTTP 404/409 on `device` means no match / several matches; the error lists candidates.\n")
	p("- Every response has an `X-Alexa-Auth` header: `ok`, `unchecked`, `not_logged_in`, or `invalid`.\n")
	p("- `GET /status` summarizes login state and devices without contacting Amazon.\n\n")

	p("## Endpoints\n")
	for _, rt := range s.routes {
		if rt.Group == "Docs" {
			continue
		}
		p("\n### `%s %s`\n\n%s\n", rt.Method, rt.Path, rt.Summary)
		if len(rt.Params) > 0 {
			p("\n")
			for _, pr := range rt.Params {
				req := ""
				if pr.Required {
					req = ", required"
				}
				p("- `%s` (%s%s): %s\n", pr.Name, pr.Type, req, pr.Desc)
			}
		}
		for _, ex := range rt.Examples {
			p("\n```\n%s\n```\n", expand(ex, base))
		}
	}
	p("\nFull machine-readable spec: `GET %s/openapi.json`\n", base)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(b.String()))
	return nil, nil
}

// openAPI renders the route table as an OpenAPI 3.1 document.
func (s *Server) openAPI(_ http.ResponseWriter, r *http.Request, _ input) (any, error) {
	base := baseURL(r)
	keyed := s.Alexa.Store.APIKey() != ""
	paths := map[string]map[string]any{}
	for _, rt := range s.routes {
		op := map[string]any{
			"operationId": rt.OperationID,
			"summary":     rt.Summary,
			"tags":        []string{rt.Group},
		}
		responses := map[string]any{
			"200": map[string]any{"description": "Success. JSON unless this is a docs page."},
			"400": errResponse("Missing or invalid parameter."),
		}
		if rt.NeedsLogin {
			responses["401"] = map[string]any{
				"description": "Amazon login missing or expired; a human must sign in via POST /auth/login.",
				"content": map[string]any{"application/json": map[string]any{
					"schema": map[string]any{"$ref": "#/components/schemas/LoginRequired"}}},
			}
			responses["502"] = errResponse("Amazon returned an error.")
		}
		if hasParam(rt, "device") {
			responses["404"] = errResponse("No device matches.")
			responses["409"] = errResponse("Several devices match; use a more specific name or serial.")
		}
		if keyed && !publicPaths[rt.Path] {
			op["security"] = []any{map[string]any{"bearerAuth": []string{}}, map[string]any{"apiKeyHeader": []string{}}}
			responses["401"] = map[string]any{
				"description": "Missing/invalid API key (api_key_required: true), or the Amazon login is missing/expired " +
					"(login_required: true; a human must sign in via POST /auth/login).",
				"content": map[string]any{"application/json": map[string]any{
					"schema": map[string]any{"$ref": "#/components/schemas/LoginRequired"}}},
			}
		}
		op["responses"] = responses

		if len(rt.Params) > 0 {
			if rt.Method == "GET" || rt.Method == "DELETE" {
				var ps []any
				for _, p := range rt.Params {
					ps = append(ps, map[string]any{
						"name": p.Name, "in": "query", "required": p.Required,
						"description": p.Desc, "schema": map[string]any{"type": p.Type},
					})
				}
				op["parameters"] = ps
			} else {
				props := map[string]any{}
				var required []string
				for _, p := range rt.Params {
					props[p.Name] = map[string]any{"type": p.Type, "description": p.Desc}
					if p.Required {
						required = append(required, p.Name)
					}
				}
				schema := map[string]any{"type": "object", "properties": props}
				if len(required) > 0 {
					schema["required"] = required
				}
				op["requestBody"] = map[string]any{
					"required": len(required) > 0,
					"content": map[string]any{
						"application/json":                  map[string]any{"schema": schema},
						"application/x-www-form-urlencoded": map[string]any{"schema": schema},
					},
				}
			}
		}
		if paths[rt.Path] == nil {
			paths[rt.Path] = map[string]any{}
		}
		paths[rt.Path][strings.ToLower(rt.Method)] = op
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "alexaproxy",
			"version": s.Version,
			"description": "Control Amazon Alexa devices over HTTP. Read GET /agent for guidance on choosing endpoints. " +
				"Every response carries an X-Alexa-Auth header with the login state.",
		},
		"servers": []any{map[string]any{"url": base}},
		"paths":   paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth":   map[string]any{"type": "http", "scheme": "bearer", "description": "API key from `alexaproxy apikey`"},
				"apiKeyHeader": map[string]any{"type": "apiKey", "in": "header", "name": "X-API-Key"},
			},
			"schemas": map[string]any{
				"Error": map[string]any{"type": "object", "properties": map[string]any{
					"ok": map[string]any{"type": "boolean"}, "error": map[string]any{"type": "string"}}},
				"LoginRequired": map[string]any{"type": "object", "properties": map[string]any{
					"ok":               map[string]any{"type": "boolean"},
					"error":            map[string]any{"type": "string"},
					"login_required":   map[string]any{"type": "boolean"},
					"api_key_required": map[string]any{"type": "boolean"},
					"how_to_fix":       map[string]any{"type": "string"},
					"auth": map[string]any{"type": "object", "properties": map[string]any{
						"state":   map[string]any{"type": "string", "enum": []string{AuthOK, AuthUnchecked, AuthNotLoggedIn, AuthInvalid}},
						"message": map[string]any{"type": "string"},
					}},
				}},
			}},
	}, nil
}

func errResponse(desc string) map[string]any {
	return map[string]any{"description": desc, "content": map[string]any{"application/json": map[string]any{
		"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}}
}

func hasParam(rt route, name string) bool {
	for _, p := range rt.Params {
		if p.Name == name {
			return true
		}
	}
	return false
}
