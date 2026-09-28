// Package store persists Amazon credentials for alexaproxy.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrNotLoggedIn is returned by Load when no credentials have been saved.
var ErrNotLoggedIn = errors.New("not logged in")

// Credentials is what gets written to credentials.json.
type Credentials struct {
	RefreshToken string    `json:"refresh_token"`
	AmazonDomain string    `json:"amazon_domain"` // auth domain, e.g. amazon.com
	AmazonLocal  string    `json:"amazon_local"`  // marketplace domain, e.g. amazon.co.uk
	SavedAt      time.Time `json:"saved_at"`
}

// Store reads and writes credentials in a single directory.
type Store struct {
	Dir string
}

// DefaultDir picks the state directory: $ALEXAPROXY_STATE_DIR, then
// systemd's $STATE_DIRECTORY, then ~/.config/alexaproxy.
func DefaultDir() string {
	if d := os.Getenv("ALEXAPROXY_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		return d
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "alexaproxy")
	}
	return ".alexaproxy"
}

func (s *Store) Path() string { return filepath.Join(s.Dir, "credentials.json") }

// BinDir holds downloaded helper binaries (alexa-cookie-cli).
func (s *Store) BinDir() string { return filepath.Join(s.Dir, "bin") }

func (s *Store) Load() (*Credentials, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotLoggedIn
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path(), err)
	}
	if c.RefreshToken == "" {
		return nil, ErrNotLoggedIn
	}
	if c.AmazonDomain == "" {
		c.AmazonDomain = "amazon.com"
	}
	if c.AmazonLocal == "" {
		c.AmazonLocal = c.AmazonDomain
	}
	return &c, nil
}

// Save writes credentials atomically with 0600 permissions.
func (s *Store) Save(c *Credentials) error {
	if c.SavedAt.IsZero() {
		c.SavedAt = time.Now().UTC()
	}
	return s.writeJSON(s.Path(), c)
}

// writeJSON atomically replaces path with v as indented JSON, mode 0600.
func (s *Store) writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return s.writeFile(path, append(data, '\n'))
}

// writeFile atomically replaces path with data, mode 0600.
func (s *Store) writeFile(path string, data []byte) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Delete removes stored credentials. It is not an error if none exist.
func (s *Store) Delete() error {
	err := os.Remove(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// DeviceRef identifies an Echo device. Serial is what requests use; Name is
// kept for display.
type DeviceRef struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
}

// Settings are runtime preferences changeable over the API.
type Settings struct {
	DefaultDevice *DeviceRef `json:"default_device,omitempty"`
}

func (s *Store) SettingsPath() string { return filepath.Join(s.Dir, "settings.json") }

// LoadSettings returns saved settings, or empty settings if none are saved.
func (s *Store) LoadSettings() (*Settings, error) {
	var st Settings
	data, err := os.ReadFile(s.SettingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return &st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.SettingsPath(), err)
	}
	return &st, nil
}

func (s *Store) SaveSettings(st *Settings) error { return s.writeJSON(s.SettingsPath(), st) }

// ImportAlexaCLI reads ~/.alexa-cli/config.json written by `alexacli auth`.
func ImportAlexaCLI() (*Credentials, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".alexa-cli", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.RefreshToken == "" {
		return nil, fmt.Errorf("%s has no refresh_token", path)
	}
	return &c, nil
}

// MaskToken shows only the first few characters of a token.
func MaskToken(t string) string {
	if len(t) <= 8 {
		return "********"
	}
	return t[:8] + "…"
}
