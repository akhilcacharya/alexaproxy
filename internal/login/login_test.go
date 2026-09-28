package login

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCookieCLI writes a shell script standing in for alexa-cookie-cli.
func fakeCookieCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alexa-cookie-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type validator struct {
	mu     sync.Mutex
	tokens []string
	err    error
}

func (v *validator) validate(token, domain, local string) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tokens = append(v.tokens, token+" "+domain+" "+local)
	return 3, v.err
}

func newTestManager(t *testing.T, script string, v *validator, timeout time.Duration) *Manager {
	return NewManager(Options{
		CookieCLI: fakeCookieCLI(t, script),
		Host:      "127.0.0.1",
		Port:      1, // nothing listens; state goes straight from preparing to verifying
		Timeout:   timeout,
		Validate:  v.validate,
	})
}

func wait(t *testing.T, m *Manager) Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := m.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v (state %s)", err, s.State)
	}
	return s
}

func TestLoginSucceeds(t *testing.T) {
	v := &validator{}
	m := newTestManager(t, `echo "some banner"; echo "Atnr|fake-token"`, v, time.Minute)
	s, err := m.Start("amazon.com", "amazon.co.uk")
	if err != nil {
		t.Fatal(err)
	}
	if s.LoginURL != "http://127.0.0.1:1/" || s.Country != "amazon.co.uk" {
		t.Errorf("unexpected session: %+v", s)
	}
	again, _ := m.Start("", "")
	if again.ID != s.ID {
		t.Error("Start while a login is running should return the same session")
	}
	s = wait(t, m)
	if s.State != StateSucceeded || s.Devices != 3 {
		t.Fatalf("got %+v", s)
	}
	if want := []string{"Atnr|fake-token amazon.com amazon.co.uk"}; !reflect.DeepEqual(v.tokens, want) {
		t.Errorf("validated %v, want %v", v.tokens, want)
	}
}

func TestLoginFailures(t *testing.T) {
	cases := []struct {
		name, script string
		validErr     error
		timeout      time.Duration
		wantErr      string
	}{
		{"no token", `echo "not a token"`, nil, time.Minute, "without producing a refresh token"},
		{"helper crashes", `echo boom >&2; exit 3`, nil, time.Minute, "exit status 3"},
		{"token rejected", `echo "Atnr|bad"`, errors.New("InvalidToken"), time.Minute, "InvalidToken"},
		{"timeout", `exec sleep 30`, nil, 300 * time.Millisecond, "timed out"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestManager(t, c.script, &validator{err: c.validErr}, c.timeout)
			if _, err := m.Start("", ""); err != nil {
				t.Fatal(err)
			}
			s := wait(t, m)
			if s.State != StateFailed || !strings.Contains(s.Error, c.wantErr) {
				t.Fatalf("got state %s error %q, want failed with %q", s.State, s.Error, c.wantErr)
			}
			if !s.Done() {
				t.Error("failed session should be Done")
			}
		})
	}
}

func TestLoginCancel(t *testing.T) {
	m := newTestManager(t, `exec sleep 30`, &validator{}, time.Minute)
	if _, ok := m.Cancel(); ok {
		t.Error("Cancel with nothing running should report false")
	}
	if _, err := m.Start("", ""); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s, ok := m.Cancel()
	if !ok || s.State != StateCanceled {
		t.Fatalf("Cancel = %+v, %v", s, ok)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Cancel took %s; the helper should be killed promptly", d)
	}
	if cur, _ := m.Current(); cur.State != StateCanceled {
		t.Errorf("state after cancel = %s", cur.State)
	}
	// A new login can start after a cancel.
	next, err := m.Start("", "")
	if err != nil || next.ID == s.ID {
		t.Errorf("restart after cancel: %+v, %v", next, err)
	}
	m.Cancel()
}

func TestExtractToken(t *testing.T) {
	for out, want := range map[string]string{
		"Atnr|abc\n":                      "Atnr|abc",
		"Please open ...\nAtnr|abc def\n": "Atnr|abc",
		"no token here":                   "",
		"":                                "",
	} {
		if got := extractToken(out); got != want {
			t.Errorf("extractToken(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestLocaleFlags(t *testing.T) {
	if got := localeFlags("amazon.de"); !reflect.DeepEqual(got, []string{"-a", "de_DE", "-L", "de-DE"}) {
		t.Errorf("amazon.de: %v", got)
	}
	if got := localeFlags("amazon.example"); !reflect.DeepEqual(got, []string{"-a", "en_US", "-L", "en-US"}) {
		t.Errorf("fallback: %v", got)
	}
}
