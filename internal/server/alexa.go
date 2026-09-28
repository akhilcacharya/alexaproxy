package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/akhilcacharya/alexaproxy/internal/api"
	"github.com/akhilcacharya/alexaproxy/internal/store"
)

// clientMaxAge forces a fresh cookie exchange periodically so long-running
// processes don't sit on stale session cookies.
const clientMaxAge = 6 * time.Hour

// Auth states, reported by /status and the X-Alexa-Auth response header.
const (
	AuthOK          = "ok"            // credentials work
	AuthUnchecked   = "unchecked"     // credentials exist but haven't been tried yet
	AuthNotLoggedIn = "not_logged_in" // no credentials stored
	AuthInvalid     = "invalid"       // Amazon rejected the stored credentials
)

// AuthState is the service's best current knowledge of the Amazon login.
type AuthState struct {
	State     string     `json:"state"`
	Message   string     `json:"message"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// OK reports whether Alexa calls are expected to work.
func (a AuthState) OK() bool { return a.State == AuthOK || a.State == AuthUnchecked }

// Alexa owns the (non-thread-safe) upstream client and serializes access.
type Alexa struct {
	Store *store.Store

	mu        sync.Mutex
	client    *api.Client
	createdAt time.Time

	stateMu   sync.Mutex
	auth      AuthState
	devices   []api.Device
	devicesAt time.Time
}

// Do runs fn with an authenticated client. If fn fails with what looks like
// an auth error, the client is rebuilt from the refresh token and fn retried.
// The outcome updates the reported auth state.
func (a *Alexa) Do(fn func(*api.Client) error) error {
	err := a.do(fn)
	a.record(err)
	return err
}

func (a *Alexa) do(fn func(*api.Client) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	c, err := a.clientLocked()
	if err != nil {
		return err
	}
	err = fn(c)
	if err == nil || !isAuthError(err) {
		return err
	}
	a.client = nil
	if c, err = a.clientLocked(); err != nil {
		return err
	}
	return fn(c)
}

func (a *Alexa) clientLocked() (*api.Client, error) {
	if a.client != nil && time.Since(a.createdAt) < clientMaxAge {
		return a.client, nil
	}
	creds, err := a.Store.Load()
	if err != nil {
		return nil, err
	}
	c, err := api.NewClientWithLocal(creds.RefreshToken, creds.AmazonDomain, creds.AmazonLocal)
	if err != nil {
		return nil, err
	}
	a.client, a.createdAt = c, time.Now()
	return c, nil
}

// record updates the auth state from the result of an Amazon call. Errors
// that aren't about credentials (network, device not found...) are ignored.
func (a *Alexa) record(err error) {
	now := time.Now().UTC()
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	switch {
	case err == nil:
		a.auth = AuthState{State: AuthOK, Message: "Logged in to Amazon.", CheckedAt: &now}
	case errors.Is(err, store.ErrNotLoggedIn):
		a.auth = AuthState{State: AuthNotLoggedIn, Message: "No Amazon login stored.", CheckedAt: &now}
	case isAuthError(err):
		a.auth = AuthState{State: AuthInvalid, Message: "Amazon rejected the stored login; sign in again.",
			CheckedAt: &now, LastError: err.Error()}
	}
}

// Auth returns the current auth state.
func (a *Alexa) Auth() AuthState {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.auth.State == "" {
		// Nothing has run yet; answer from disk.
		if _, err := a.Store.Load(); err != nil {
			return AuthState{State: AuthNotLoggedIn, Message: "No Amazon login stored."}
		}
		return AuthState{State: AuthUnchecked, Message: "Login stored but not verified yet."}
	}
	return a.auth
}

// Devices lists Echo devices and remembers them for the docs pages.
func (a *Alexa) Devices() ([]api.Device, error) {
	var devs []api.Device
	err := a.Do(func(c *api.Client) error {
		var err error
		devs, err = c.GetDevices()
		return err
	})
	if err == nil {
		a.stateMu.Lock()
		a.devices, a.devicesAt = devs, time.Now()
		a.stateMu.Unlock()
	}
	return devs, err
}

// CachedDevices returns the last device list seen, without calling Amazon.
func (a *Alexa) CachedDevices() []api.Device {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.devices
}

// Monitor verifies the login now and then every interval, so /status can
// report an expired login before a real request trips over it.
func (a *Alexa) Monitor(ctx context.Context, interval time.Duration) {
	check := func() {
		if _, err := a.Store.Load(); err != nil {
			a.record(err)
			return
		}
		if _, err := a.Devices(); err != nil {
			log.Printf("login check: %v", err)
		}
	}
	check()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// Login verifies a refresh token against Amazon and, if it works, saves it
// and makes it the active credential. Returns the number of Echo devices.
func (a *Alexa) Login(token, domain, local string) (int, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "Atnr|") {
		return 0, errors.New(`refresh token should start with "Atnr|"`)
	}
	if domain == "" {
		domain = "amazon.com"
	}
	if local == "" {
		local = domain
	}
	c, err := api.NewClientWithLocal(token, domain, local)
	if err != nil {
		return 0, err
	}
	devices, err := c.GetDevices()
	if err != nil {
		return 0, fmt.Errorf("list devices: %w", err)
	}
	err = a.Store.Save(&store.Credentials{RefreshToken: token, AmazonDomain: domain, AmazonLocal: local})
	if err != nil {
		return 0, err
	}

	a.mu.Lock()
	a.client, a.createdAt = c, time.Now()
	a.mu.Unlock()
	a.record(nil)
	a.stateMu.Lock()
	a.devices, a.devicesAt = devices, time.Now()
	a.stateMu.Unlock()
	return len(devices), nil
}

// Logout forgets the stored and in-memory credentials.
func (a *Alexa) Logout() error {
	a.mu.Lock()
	a.client = nil
	a.mu.Unlock()
	err := a.Store.Delete()
	a.record(store.ErrNotLoggedIn)
	a.stateMu.Lock()
	a.devices = nil
	a.stateMu.Unlock()
	return err
}

func isAuthError(err error) bool {
	s := err.Error()
	for _, needle := range []string{"API error 401", "API error 403", "authentication failed", "CSRF"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// isLoginProblem reports whether err means the user has to sign in again.
func isLoginProblem(err error) bool {
	return errors.Is(err, store.ErrNotLoggedIn) || isAuthError(err)
}

// fixMojibake repairs names Amazon stores double-encoded, e.g. "Samâ\u0080\u0099s"
// for "Sam’s": UTF-8 bytes that were decoded as Latin-1 and re-encoded.
func fixMojibake(s string) string {
	b := make([]byte, 0, len(s))
	nonASCII := false
	for _, r := range s {
		if r > 0xff {
			return s
		}
		if r > 0x7f {
			nonASCII = true
		}
		b = append(b, byte(r))
	}
	if !nonASCII || !utf8.Valid(b) {
		return s
	}
	return string(b)
}

// deviceName is the display name for a device.
func deviceName(d api.Device) string { return fixMojibake(d.AccountName) }

// findDevice matches by serial or exact name, then case-insensitive substring.
func findDevice(devices []api.Device, query string) (*api.Device, error) {
	for i, d := range devices {
		if d.SerialNumber == query || d.AccountName == query || deviceName(d) == query {
			return &devices[i], nil
		}
	}
	q := strings.ToLower(query)
	var matches []int
	for i, d := range devices {
		if strings.Contains(strings.ToLower(deviceName(d)), q) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return nil, &httpError{404, fmt.Sprintf("no device matches %q; see GET /devices", query)}
	case 1:
		return &devices[matches[0]], nil
	}
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = fmt.Sprintf("%s (serial %s)", deviceName(devices[m]), devices[m].SerialNumber)
	}
	return nil, &httpError{409, fmt.Sprintf("%q matches several devices; use a more specific name or a serial: %s",
		query, strings.Join(names, ", "))}
}
