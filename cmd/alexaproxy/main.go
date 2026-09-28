// Command alexaproxy exposes Alexa as a small HTTP API for your tailnet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/akhilcacharya/alexaproxy/internal/login"
	"github.com/akhilcacharya/alexaproxy/internal/server"
	"github.com/akhilcacharya/alexaproxy/internal/store"
	"github.com/akhilcacharya/alexaproxy/internal/tailnet"
)

var version = "dev"

const usage = `alexaproxy — control Alexa over HTTP from your tailnet

Usage:
  alexaproxy serve   [flags]   run the HTTP API (what systemd runs)
  alexaproxy login   [flags]   sign in to Amazon and store credentials
  alexaproxy status  [flags]   show stored credentials (--verify to test them)
  alexaproxy logout  [flags]   delete stored credentials
  alexaproxy apikey  [flags]   print (creating if needed) the API key for public/Funnel access
  alexaproxy version

Run "alexaproxy <command> -h" for flags. Once serving, GET / documents the API.
`

func main() {
	if version == "dev" {
		// `go install ...@v1.2.3` records the module version in the binary.
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
	}
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "login":
		err = runLogin(args)
	case "status":
		err = runStatus(args)
	case "logout":
		err = runLogout(args)
	case "apikey":
		err = runAPIKey(args)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// commonFlags are shared by every subcommand that touches credentials.
type commonFlags struct {
	stateDir string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.stateDir, "state-dir", store.DefaultDir(),
		"where credentials are stored (env ALEXAPROXY_STATE_DIR, or systemd's STATE_DIRECTORY)")
}

func (c *commonFlags) store() *store.Store { return &store.Store{Dir: c.stateDir} }

// loginFlags configure the browser login proxy.
type loginFlags struct {
	host      string
	port      int
	cookieCLI string
	timeout   time.Duration
}

func (l *loginFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&l.host, "login-host", "auto",
		`address browsers use to reach the login page; "auto" = this machine's Tailscale IP`)
	fs.IntVar(&l.port, "login-port", 8788, "port for the temporary Amazon login page")
	fs.StringVar(&l.cookieCLI, "cookie-cli", "", "path to alexa-cookie-cli (downloaded automatically if unset)")
	fs.DurationVar(&l.timeout, "login-timeout", 10*time.Minute, "how long to wait for you to finish signing in")
}

func (l *loginFlags) manager(st *store.Store, validate login.Validator) *login.Manager {
	return login.NewManager(login.Options{
		CookieCLI: l.cookieCLI,
		BinDir:    st.BinDir(),
		Host:      l.host,
		Port:      l.port,
		Timeout:   l.timeout,
		Validate:  validate,
	})
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var common commonFlags
	var lf loginFlags
	common.register(fs)
	lf.register(fs)
	addr := fs.String("addr", ":8787", "listen address")
	device := fs.String("default-device", os.Getenv("ALEXAPROXY_DEFAULT_DEVICE"),
		"device used when a request omits `device` (env ALEXAPROXY_DEFAULT_DEVICE)")
	allowAll := fs.Bool("allow-all", false, "accept requests from any address, not just tailnet + localhost")
	checkEvery := fs.Duration("check-interval", 15*time.Minute, "how often to re-verify the Amazon login in the background")
	fs.Parse(args)
	log.SetFlags(log.LstdFlags)

	st := common.store()
	alexa := &server.Alexa{Store: st}
	mgr := lf.manager(st, alexa.Login)

	srv := &server.Server{
		Alexa:         alexa,
		Login:         mgr,
		DefaultDevice: *device,
		AllowAll:      *allowAll,
		Version:       version,
	}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if _, err := st.Load(); errors.Is(err, store.ErrNotLoggedIn) {
		log.Printf("no credentials in %s yet — POST /auth/login to sign in", st.Dir)
	} else if err != nil {
		log.Printf("warning: %v", err)
	} else {
		log.Printf("using credentials from %s", st.Path())
	}
	if ip, err := tailnet.IPv4(); err == nil {
		_, port, _ := net.SplitHostPort(*addr)
		log.Printf("tailnet URL: http://%s/", net.JoinHostPort(ip.String(), port))
	}
	log.Printf("listening on %s", *addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go alexa.Monitor(ctx, *checkEvery)
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Printf("shutting down")
	mgr.Cancel()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	var common commonFlags
	var lf loginFlags
	common.register(fs)
	lf.register(fs)
	token := fs.String("token", "", "save this refresh token (Atnr|...) instead of logging in via browser")
	importCLI := fs.Bool("import-alexa-cli", false, "import the token from ~/.alexa-cli/config.json (alexacli auth)")
	domain := fs.String("domain", "amazon.com", "Amazon auth domain (amazon.co.jp for Japan)")
	country := fs.String("country", "", "marketplace, e.g. amazon.co.uk (default: same as --domain)")
	fs.Parse(args)

	st := common.store()
	alexa := &server.Alexa{Store: st}

	switch {
	case *importCLI:
		c, err := store.ImportAlexaCLI()
		if err != nil {
			return err
		}
		return finishTokenLogin(alexa, c.RefreshToken, c.AmazonDomain, c.AmazonLocal)
	case *token != "":
		return finishTokenLogin(alexa, *token, *domain, *country)
	}

	mgr := lf.manager(st, alexa.Login)
	sess, err := mgr.Start(*domain, *country)
	if err != nil {
		return err
	}
	fmt.Printf("Open this in a browser on any device on your tailnet and sign in to Amazon:\n\n    %s\n\n", sess.LoginURL)
	fmt.Printf("Waiting up to %s (Ctrl-C to cancel)...\n", lf.timeout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sess, err = mgr.Wait(ctx)
	if err != nil {
		mgr.Cancel()
		return err
	}
	if sess.State != login.StateSucceeded {
		return fmt.Errorf("login %s: %s", sess.State, sess.Error)
	}
	fmt.Printf("Logged in (%d devices). Credentials saved to %s\n", sess.Devices, st.Path())
	return nil
}

func finishTokenLogin(alexa *server.Alexa, token, domain, country string) error {
	n, err := alexa.Login(token, domain, country)
	if err != nil {
		return err
	}
	fmt.Printf("Logged in (%d devices). Credentials saved to %s\n", n, alexa.Store.Path())
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	verify := fs.Bool("verify", false, "check the token against Amazon")
	fs.Parse(args)

	st := common.store()
	c, err := st.Load()
	if errors.Is(err, store.ErrNotLoggedIn) {
		fmt.Printf("Not logged in (looked in %s). Run: alexaproxy login\n", st.Path())
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("Credentials: %s\nToken:       %s\nDomain:      %s\nCountry:     %s\nSaved:       %s\n",
		st.Path(), store.MaskToken(c.RefreshToken), c.AmazonDomain, c.AmazonLocal, c.SavedAt.Local().Format(time.RFC1123))
	if *verify {
		alexa := &server.Alexa{Store: st}
		// Re-saving an unchanged token is harmless and exercises the same path as login.
		n, err := alexa.Login(c.RefreshToken, c.AmazonDomain, c.AmazonLocal)
		if err != nil {
			fmt.Printf("Valid:       no (%v)\n", err)
			return nil
		}
		fmt.Printf("Valid:       yes (%d devices)\n", n)
	}
	return nil
}

func runLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	fs.Parse(args)
	st := common.store()
	if err := st.Delete(); err != nil {
		return err
	}
	fmt.Printf("Removed %s\n", st.Path())
	return nil
}

func runAPIKey(args []string) error {
	fs := flag.NewFlagSet("apikey", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	rotate := fs.Bool("rotate", false, "replace the key with a new one (the old one stops working immediately)")
	remove := fs.Bool("delete", false, "delete the key (Funnel requests are then refused; tailnet access is unaffected)")
	fs.Parse(args)
	st := common.store()

	if os.Getenv(store.APIKeyEnv) != "" {
		fmt.Fprintf(os.Stderr, "note: $%s is set and overrides %s\n", store.APIKeyEnv, st.APIKeyPath())
	}
	if *remove {
		if err := st.DeleteAPIKey(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Removed %s\n", st.APIKeyPath())
		return nil
	}
	key, created, err := st.EnsureAPIKey(*rotate)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(os.Stderr, "Created a new API key in %s (a running server picks it up automatically).\n", st.APIKeyPath())
	}
	fmt.Println(key)
	return nil
}
