package store

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCredentialsRoundTrip(t *testing.T) {
	s := &Store{Dir: t.TempDir() + "/nested"}
	if _, err := s.Load(); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Load on empty dir = %v, want ErrNotLoggedIn", err)
	}
	if err := s.Save(&Credentials{RefreshToken: "Atnr|abc"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RefreshToken != "Atnr|abc" || c.AmazonDomain != "amazon.com" || c.AmazonLocal != "amazon.com" {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.SavedAt.IsZero() {
		t.Error("SavedAt not set")
	}
	assertMode(t, s.Path(), 0o600)
	assertMode(t, s.Dir, 0o700)

	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(); err != nil {
		t.Fatalf("second Delete should be a no-op: %v", err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("after Delete: %v", err)
	}
}

func TestLoadRejectsEmptyToken(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	os.WriteFile(s.Path(), []byte(`{"refresh_token": ""}`), 0o600)
	if _, err := s.Load(); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("got %v, want ErrNotLoggedIn", err)
	}
	os.WriteFile(s.Path(), []byte(`not json`), 0o600)
	if _, err := s.Load(); err == nil || errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("corrupt file should be a parse error, got %v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	st, err := s.LoadSettings()
	if err != nil || st.DefaultDevice != nil {
		t.Fatalf("empty settings = %+v, %v", st, err)
	}
	want := &DeviceRef{Name: "Kitchen", Serial: "G123"}
	if err := s.SaveSettings(&Settings{DefaultDevice: want}); err != nil {
		t.Fatal(err)
	}
	st, err = s.LoadSettings()
	if err != nil || st.DefaultDevice == nil || *st.DefaultDevice != *want {
		t.Fatalf("got %+v, %v", st.DefaultDevice, err)
	}
	assertMode(t, s.SettingsPath(), 0o600)
}

func TestAPIKey(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	s := &Store{Dir: t.TempDir()}
	if k := s.APIKey(); k != "" {
		t.Fatalf("no key configured, got %q", k)
	}
	k1, created, err := s.EnsureAPIKey(false)
	if err != nil || !created || !strings.HasPrefix(k1, "axp_") || len(k1) < 40 {
		t.Fatalf("EnsureAPIKey = %q, %v, %v", k1, created, err)
	}
	assertMode(t, s.APIKeyPath(), 0o600)
	if got := s.APIKey(); got != k1 {
		t.Errorf("APIKey = %q, want %q", got, k1)
	}
	again, created, _ := s.EnsureAPIKey(false)
	if again != k1 || created {
		t.Errorf("EnsureAPIKey should reuse the key")
	}
	k2, created, _ := s.EnsureAPIKey(true)
	if k2 == k1 || !created {
		t.Errorf("rotate should make a new key")
	}

	t.Setenv(APIKeyEnv, "from-env")
	if got := s.APIKey(); got != "from-env" {
		t.Errorf("env override: got %q", got)
	}
	t.Setenv(APIKeyEnv, "")

	if err := s.DeleteAPIKey(); err != nil {
		t.Fatal(err)
	}
	if got := s.APIKey(); got != "" {
		t.Errorf("after delete: %q", got)
	}
}

func TestMaskToken(t *testing.T) {
	if got := MaskToken("Atnr|0123456789"); got != "Atnr|012…" {
		t.Errorf("MaskToken = %q", got)
	}
	if got := MaskToken("short"); strings.Contains(got, "short") {
		t.Errorf("short tokens must be fully masked, got %q", got)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}
