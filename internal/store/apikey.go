package store

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// APIKeyEnv overrides the key file when set.
const APIKeyEnv = "ALEXAPROXY_API_KEY"

func (s *Store) APIKeyPath() string { return filepath.Join(s.Dir, "api_key") }

// APIKey returns the configured API key, or "" if none. The file is re-read
// when it changes, so `alexaproxy apikey --rotate` applies without a restart.
func (s *Store) APIKey() string {
	if k := strings.TrimSpace(os.Getenv(APIKeyEnv)); k != "" {
		return k
	}
	return s.keyCache().get(s.APIKeyPath())
}

// EnsureAPIKey returns the stored key, generating one if needed (or always,
// with rotate).
func (s *Store) EnsureAPIKey(rotate bool) (key string, created bool, err error) {
	if !rotate {
		data, err := os.ReadFile(s.APIKeyPath())
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data)), false, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false, err
	}
	key = "axp_" + base64.RawURLEncoding.EncodeToString(b)
	if err := s.writeFile(s.APIKeyPath(), []byte(key+"\n")); err != nil {
		return "", false, err
	}
	return key, true, nil
}

// DeleteAPIKey removes the key file. It is not an error if none exists.
func (s *Store) DeleteAPIKey() error {
	err := os.Remove(s.APIKeyPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type keyCache struct {
	mu    sync.Mutex
	mtime time.Time
	size  int64
	key   string
}

var keyCaches sync.Map // state dir -> *keyCache

func (s *Store) keyCache() *keyCache {
	c, _ := keyCaches.LoadOrStore(s.Dir, &keyCache{})
	return c.(*keyCache)
}

func (c *keyCache) get(path string) string {
	fi, err := os.Stat(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.key, c.mtime, c.size = "", time.Time{}, 0
		return ""
	}
	if !fi.ModTime().Equal(c.mtime) || fi.Size() != c.size {
		data, err := os.ReadFile(path)
		if err != nil {
			return c.key
		}
		c.key, c.mtime, c.size = strings.TrimSpace(string(data)), fi.ModTime(), fi.Size()
	}
	return c.key
}
