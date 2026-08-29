// Command smokeng is the single binary: master server (serve), config
// import/export, and later the remote agent mode (DESIGN.md §2).
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
	"runtime"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"

	"smokeng/internal/alert"
	"smokeng/internal/api"
	"smokeng/internal/auth"
	"smokeng/internal/config"
	"smokeng/internal/probe"
	"smokeng/internal/store"
	"smokeng/web"
)

const version = "0.1.0-dev"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "config":
		if err := configCmd(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "agent":
		if err := agentCmd(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "version":
		fmt.Printf("smokeng %s (%s)\n", version, runtime.Version())
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: smokeng <command>

commands:
  serve                                  run the master: prober, API and web UI
  config import [--prune] FILE           sync the target tree from a TOML file
  config import-smokeping FILE           import a SmokePing Targets file
  config export                          write the target tree as TOML to stdout
  agent add --name N --pubkey K          enrol a remote agent on the master
  agent list                             list enrolled agents
  agent enable|disable|remove ID         change an agent's standing
  agent run --master URL --agent-id ID   run as a remote measurement node
  version                                print version`)
}

func configCmd(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("config "+sub, flag.ExitOnError)
	dbPath := fs.String("db", "smokeng.db", "path to the SQLite database")
	prune := fs.Bool("prune", false, "delete targets absent from the file instead of disabling them")
	alsoIPv6 := fs.Bool("also-ipv6", false,
		"import-smokeping: also create a v6 target for every hostname (address families are separate targets)")
	dryRun := fs.Bool("dry-run", false, "import-smokeping: print the translated config instead of writing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch sub {
	case "import":
		if fs.NArg() != 1 {
			return errors.New("usage: smokeng config import [--db path] [--prune] FILE")
		}
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		sum, err := config.Import(ctx, st, data, *prune)
		if err != nil {
			return err
		}
		fmt.Println(sum)
		return nil
	case "import-smokeping":
		if fs.NArg() != 1 {
			return errors.New("usage: smokeng config import-smokeping [--db path] [--also-ipv6] [--dry-run] TARGETS-FILE")
		}
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		f, warnings, err := config.ParseSmokePing(data, *alsoIPv6)
		// Warnings name what SmokePing expressed that smokeng will not, so
		// they go to stderr even when the import itself fails.
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		if err != nil {
			return err
		}
		if *dryRun {
			out, err := toml.Marshal(f)
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(out)
			return err
		}
		sum, err := config.Apply(ctx, st, f, *prune)
		if err != nil {
			return err
		}
		fmt.Println(sum)
		return nil
	case "export":
		data, err := config.Export(ctx, st)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err
	default:
		return fmt.Errorf("unknown config subcommand %q", sub)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", "smokeng.db", "path to the SQLite database")
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	insecure := fs.Bool("i-know-this-is-unauthenticated", false,
		"allow listening on a non-loopback address without authentication (DESIGN.md §7.1)")
	webhook := fs.String("alert-webhook", "",
		"POST firing and resolved alerts to this URL in Alertmanager's v2 format")
	oidcIssuer := fs.String("oidc-issuer", "", "OIDC issuer URL; enables authentication")
	oidcClientID := fs.String("oidc-client-id", "", "OIDC client id")
	oidcSecret := fs.String("oidc-client-secret", "", "OIDC client secret")
	oidcRedirect := fs.String("oidc-redirect-url", "",
		"OIDC redirect URL, e.g. https://smokeng.example.org/auth/callback")
	oidcAdminClaim := fs.String("oidc-admin-claim", "groups",
		"ID-token claim listing the user's groups")
	oidcAdminValue := fs.String("oidc-admin-value", "",
		"membership in this group grants admin; empty means every authenticated user is an admin")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *oidcIssuer == "" && !isLoopback(*listen) && !*insecure {
		return fmt.Errorf("refusing to listen on non-loopback %q without authentication: "+
			"configure --oidc-issuer, or pass --i-know-this-is-unauthenticated to override", *listen)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var authenticator *auth.Authenticator
	if *oidcIssuer != "" {
		key, err := st.SessionKey(ctx)
		if err != nil {
			return err
		}
		redirect := *oidcRedirect
		if redirect == "" {
			redirect = "http://" + *listen + "/auth/callback"
		}
		authenticator, err = auth.New(ctx, auth.Config{
			Issuer:       *oidcIssuer,
			ClientID:     *oidcClientID,
			ClientSecret: *oidcSecret,
			RedirectURL:  redirect,
			AdminClaim:   *oidcAdminClaim,
			AdminValue:   *oidcAdminValue,
			// Cookies may only skip the Secure attribute where the browser
			// would refuse them anyway: local development over plain HTTP.
			Insecure: isLoopback(*listen),
		}, key)
		if err != nil {
			return err
		}
		log.Printf("authentication enabled via %s", *oidcIssuer)
	}

	// Without a webhook there is nowhere to send alerts, so rules are not
	// evaluated at all rather than firing into a void.
	var alerts *alert.Manager
	if *webhook != "" {
		alerts = alert.NewManager(st, &alert.Webhook{URL: *webhook})
		log.Printf("alerting enabled, posting to %s", *webhook)
	}

	eng, err := probe.NewEngine(st, alerterOrNil(alerts))
	if err != nil {
		return err
	}
	engDone := make(chan struct{})
	go func() {
		defer close(engDone)
		if err := eng.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("probe engine: %v", err)
		}
	}()

	dist, err := web.Dist()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:    *listen,
		Handler: api.New(st, alertViewOrNil(alerts), authOrNil(authenticator), dist),
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("smokeng %s listening on http://%s (db: %s)", version, *listen, *dbPath)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Printf("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		<-engDone // engine flushes its last batch before the store closes
	}
	return nil
}

// A nil *alert.Manager is not a nil interface, so the conversion has to be
// explicit or every "is alerting configured" check silently succeeds.
func alerterOrNil(m *alert.Manager) probe.Alerter {
	if m == nil {
		return nil
	}
	return m
}

func alertViewOrNil(m *alert.Manager) api.AlertView {
	if m == nil {
		return nil
	}
	return m
}

func authOrNil(a *auth.Authenticator) api.Authenticator {
	if a == nil {
		return nil
	}
	return a
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
