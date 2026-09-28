// Package login runs the browser-based Amazon login flow.
//
// It drives alexa-cookie-cli (https://github.com/adn77/alexa-cookie-cli), a
// small proxy that serves Amazon's sign-in page and captures the refresh
// token once you log in. We bind that proxy to the tailnet address so the
// login page can be opened from any device on the tailnet.
package login

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akhilcacharya/alexaproxy/internal/tailnet"
)

const cookieCLIVersion = "v5.0.1"

// Session states.
const (
	StatePreparing = "preparing"           // downloading/starting the login proxy
	StateWaiting   = "waiting_for_browser" // open URL and sign in
	StateVerifying = "verifying"           // token captured, checking it works
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// Validator checks a captured token and persists it. It returns the number
// of Echo devices visible to the account.
type Validator func(token, domain, local string) (devices int, err error)

// Options configures the Manager.
type Options struct {
	CookieCLI string        // path to alexa-cookie-cli; downloaded into BinDir if empty
	BinDir    string        // where to cache the downloaded binary
	Host      string        // host/IP browsers use to reach the login page; "auto" = Tailscale IP
	Bind      string        // IP the login proxy listens on (default: Host)
	Port      int           // port the login proxy listens on
	Timeout   time.Duration // how long to wait for the user to finish signing in
	Validate  Validator
}

// Session is a snapshot of a login attempt.
type Session struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	LoginURL  string    `json:"login_url"`
	Domain    string    `json:"domain"`
	Country   string    `json:"country"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Devices   int       `json:"devices,omitempty"`
	Error     string    `json:"error,omitempty"`
	Hint      string    `json:"hint,omitempty"`
}

// Done reports whether the session has finished, successfully or not.
func (s Session) Done() bool {
	return s.State == StateSucceeded || s.State == StateFailed || s.State == StateCanceled
}

type Manager struct {
	opts Options

	mu      sync.Mutex
	cur     *Session
	cancel  context.CancelFunc
	done    chan struct{} // closed when the current run goroutine exits
	changed chan struct{} // closed and replaced on every state change
}

func NewManager(opts Options) *Manager {
	if opts.Port == 0 {
		opts.Port = 8788
	}
	if opts.Host == "" {
		opts.Host = "auto"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	return &Manager{opts: opts, changed: make(chan struct{})}
}

// Current returns the most recent session, if any.
func (m *Manager) Current() (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return Session{}, false
	}
	return *m.cur, true
}

// Wait blocks until the current session finishes or ctx is done.
func (m *Manager) Wait(ctx context.Context) (Session, error) {
	for {
		// Copy the session under the lock; run() mutates it concurrently.
		m.mu.Lock()
		cur, ch := m.cur, m.changed
		var s Session
		if cur != nil {
			s = *cur
		}
		m.mu.Unlock()
		if cur == nil {
			return Session{}, errors.New("no login in progress")
		}
		if s.Done() {
			return s, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return s, ctx.Err()
		}
	}
}

// Start begins a login. If one is already in progress it is returned as-is.
func (m *Manager) Start(domain, country string) (Session, error) {
	if domain == "" {
		domain = "amazon.com"
	}
	if country == "" {
		country = domain
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil && !m.cur.Done() {
		return *m.cur, nil
	}

	host, bind, err := m.addrs()
	if err != nil {
		return Session{}, err
	}

	now := time.Now().UTC()
	s := &Session{
		ID:        randomID(),
		State:     StatePreparing,
		LoginURL:  fmt.Sprintf("http://%s/", net.JoinHostPort(host, strconv.Itoa(m.opts.Port))),
		Domain:    domain,
		Country:   country,
		StartedAt: now,
		ExpiresAt: now.Add(m.opts.Timeout),
		Hint:      "Open login_url in a browser on any tailnet device and sign in to Amazon.",
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.opts.Timeout)
	done := make(chan struct{})
	m.cur, m.cancel, m.done = s, cancel, done
	m.notifyLocked()

	go func() {
		defer close(done)
		m.run(ctx, s.ID, host, bind, domain, country)
	}()
	return *s, nil
}

// Cancel stops the in-progress login, if any, and waits briefly for the
// login proxy to exit so it doesn't outlive us.
func (m *Manager) Cancel() (Session, bool) {
	m.mu.Lock()
	if m.cur == nil || m.cur.Done() {
		m.mu.Unlock()
		return Session{}, false
	}
	m.cancel()
	m.cur.State = StateCanceled
	m.cur.Hint = ""
	m.notifyLocked()
	s, done := *m.cur, m.done
	m.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	return s, true
}

func (m *Manager) update(id string, fn func(*Session)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.cur.ID != id || m.cur.Done() {
		return
	}
	fn(m.cur)
	m.notifyLocked()
}

func (m *Manager) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *Manager) fail(id string, err error) {
	log.Printf("login %s: %v", id, err)
	m.update(id, func(s *Session) {
		s.State = StateFailed
		s.Error = err.Error()
		s.Hint = "Start a new login with POST /auth/login."
	})
}

func (m *Manager) run(ctx context.Context, id, host, bind, domain, country string) {
	defer func() {
		m.mu.Lock()
		if m.cur != nil && m.cur.ID == id {
			m.cancel()
		}
		m.mu.Unlock()
	}()

	bin, err := m.ensureCookieCLI(ctx)
	if err != nil {
		m.fail(id, err)
		return
	}

	args := []string{
		"-b", domain,
		"-p", country,
		"-H", host,
		"-B", bind,
		"-P", strconv.Itoa(m.opts.Port),
		"-A", "alexaproxy",
		"-q",
	}
	args = append(args, localeFlags(country)...)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.SysProcAttr = sysProcAttr()
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.fail(id, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.fail(id, err)
		return
	}
	if err := cmd.Start(); err != nil {
		m.fail(id, fmt.Errorf("start alexa-cookie-cli: %w", err))
		return
	}
	log.Printf("login %s: alexa-cookie-cli listening on %s:%d (open http://%s:%d/)", id, bind, m.opts.Port, host, m.opts.Port)

	var stderrTail tail
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			stderrTail.add(sc.Text())
			log.Printf("login %s: alexa-cookie-cli: %s", id, sc.Text())
		}
	}()
	go m.markReadyWhenListening(ctx, id, bind)

	out, _ := io.ReadAll(stdout)
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			m.fail(id, fmt.Errorf("timed out after %s waiting for Amazon sign-in", m.opts.Timeout))
		}
		return // canceled: state already set
	}

	token := extractToken(string(out))
	if token == "" {
		msg := "alexa-cookie-cli exited without producing a refresh token"
		if waitErr != nil {
			msg += ": " + waitErr.Error()
		}
		if t := stderrTail.String(); t != "" {
			msg += " (" + t + ")"
		}
		m.fail(id, errors.New(msg))
		return
	}

	m.update(id, func(s *Session) {
		s.State = StateVerifying
		s.Hint = "Token captured; verifying with Amazon."
	})
	n, err := m.opts.Validate(token, domain, country)
	if err != nil {
		m.fail(id, fmt.Errorf("captured token did not work: %w", err))
		return
	}
	m.update(id, func(s *Session) {
		s.State = StateSucceeded
		s.Devices = n
		s.Hint = "Logged in. Credentials saved."
	})
	log.Printf("login %s: succeeded (%d devices)", id, n)
}

func (m *Manager) markReadyWhenListening(ctx context.Context, id, bind string) {
	addr := net.JoinHostPort(bind, strconv.Itoa(m.opts.Port))
	if bind == "0.0.0.0" || bind == "::" {
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(m.opts.Port))
	}
	for ctx.Err() == nil {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			m.update(id, func(s *Session) {
				if s.State == StatePreparing {
					s.State = StateWaiting
				}
			})
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// addrs resolves where the login proxy listens and how browsers reach it.
// "auto" is resolved per login so a late-starting tailscaled isn't fatal.
func (m *Manager) addrs() (host, bind string, err error) {
	host, bind = m.opts.Host, m.opts.Bind
	if host == "auto" {
		ip, err := tailnet.IPv4()
		if err != nil {
			return "", "", fmt.Errorf("%w; set --login-host", err)
		}
		host = ip.String()
	}
	if bind == "" {
		bind = host
		if ip, err := netip.ParseAddr(host); err != nil || !ip.IsValid() {
			bind = "0.0.0.0" // a hostname: listen everywhere
		}
	}
	return host, bind, nil
}

// extractToken finds the Atnr| refresh token in alexa-cookie-cli output.
func extractToken(out string) string {
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "Atnr|") {
			return f
		}
	}
	return ""
}

func (m *Manager) ensureCookieCLI(ctx context.Context) (string, error) {
	if m.opts.CookieCLI != "" {
		return m.opts.CookieCLI, nil
	}
	if p, err := exec.LookPath("alexa-cookie-cli"); err == nil {
		return p, nil
	}
	path := filepath.Join(m.opts.BinDir, "alexa-cookie-cli-"+cookieCLIVersion)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	var platform string
	switch runtime.GOOS {
	case "linux":
		platform = "linux"
	case "darwin":
		platform = "macos"
	default:
		return "", fmt.Errorf("no prebuilt alexa-cookie-cli for %s; pass --cookie-cli", runtime.GOOS)
	}
	// Only x86-64 builds are published; Apple silicon runs them via Rosetta.
	if runtime.GOARCH != "amd64" && runtime.GOOS != "darwin" {
		return "", fmt.Errorf("no prebuilt alexa-cookie-cli for %s/%s; log in from an x86-64 Linux or macOS machine instead (see the README section on logging in from another machine), or build it from https://github.com/adn77/alexa-cookie-cli and pass --cookie-cli", runtime.GOOS, runtime.GOARCH)
	}
	url := fmt.Sprintf("https://github.com/adn77/alexa-cookie-cli/releases/download/%s/alexa-cookie-cli-%s-x64", cookieCLIVersion, platform)
	log.Printf("downloading %s", url)

	if err := os.MkdirAll(m.opts.BinDir, 0o700); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download alexa-cookie-cli: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download alexa-cookie-cli: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(m.opts.BinDir, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("download alexa-cookie-cli: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// localeFlags mirrors alexa-cli's mapping from marketplace to sign-in locale.
func localeFlags(domain string) []string {
	locales := map[string][2]string{
		"amazon.de":     {"de_DE", "de-DE"},
		"amazon.co.uk":  {"en_GB", "en-GB"},
		"amazon.co.jp":  {"ja_JP", "ja-JP"},
		"amazon.fr":     {"fr_FR", "fr-FR"},
		"amazon.it":     {"it_IT", "it-IT"},
		"amazon.es":     {"es_ES", "es-ES"},
		"amazon.com.au": {"en_AU", "en-AU"},
		"amazon.ca":     {"en_CA", "en-CA"},
		"amazon.com.br": {"pt_BR", "pt-BR"},
		"amazon.in":     {"en_IN", "en-IN"},
	}
	l, ok := locales[domain]
	if !ok {
		l = [2]string{"en_US", "en-US"}
	}
	return []string{"-a", l[0], "-L", l[1]}
}

func randomID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// tail keeps the last few lines of a stream for error messages.
type tail struct {
	mu    sync.Mutex
	lines []string
}

func (t *tail) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
	if len(t.lines) > 5 {
		t.lines = t.lines[1:]
	}
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, " | ")
}
