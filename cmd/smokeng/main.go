// Command smokeng is the single binary: master server (serve), config
// import/export, and the remote agent mode (DESIGN.md §2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/api"
	"github.com/timdebruijn/smokeng/internal/auth"
	"github.com/timdebruijn/smokeng/internal/config"
	"github.com/timdebruijn/smokeng/internal/probe"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/web"
)

// version is stamped at build time from the git tag:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// A build without it says so rather than claiming a version it does not have.
var version = "dev"

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
		fmt.Printf("smokeng %s %s/%s (%s)\n",
			version, runtime.GOOS, runtime.GOARCH, runtime.Version())
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
  agent key [--key PATH]                 create the agent's key if absent, print its public half
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
	allowUnknownAgents := fs.Bool("allow-unknown-agents", false,
		"accept `agents` entries that name no enrolled agent, reporting them as warnings")
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
			return errors.New("usage: smokeng config import [--db path] [--prune] [--allow-unknown-agents] FILE")
		}
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		var opts []config.Option
		if *allowUnknownAgents {
			opts = append(opts, config.AllowUnknownAgents())
		}
		sum, err := config.Import(ctx, st, data, *prune, opts...)
		if err != nil {
			return err
		}
		for _, w := range sum.Warnings {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
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
		// Slaves named in a SmokePing file are by definition not enrolled here
		// yet — the importer warns about each one — so the unknown-agent check
		// would refuse every migration it is meant to enable.
		sum, err := config.Apply(ctx, st, f, *prune, config.AllowUnknownAgents())
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
	externalURL := fs.String("external-url", "",
		"the address others reach this master at, e.g. https://smokeng.example.org. "+
			"Set it when a reverse proxy sits in front: the enrolment command shown in "+
			"the UI has to name the address an agent can reach, not the one whoever is "+
			"looking at the page happened to use")
	trustedProxies := fs.String("trusted-proxies", "",
		"comma-separated CIDRs whose X-Forwarded-For may be believed, e.g. 10.0.0.0/8. "+
			"Affects log lines only: nothing here is authorised on a client address, so "+
			"this buys accurate logs behind a proxy and nothing else")
	tlsCAFiles := fs.String("tls-ca-file", "",
		"comma-separated PEM files whose certificates https probes trust, in addition "+
			"to the system roots. Use this for an internal PKI: it keeps verification "+
			"on, where a target's tls_skip_verify turns it off")
	irttHMACKeys := fs.String("irtt-hmac-keys", "",
		"path to a TOML keyfile mapping irtt \"host:port\" endpoints to their shared "+
			"HMAC secrets, so only this prober may use those servers. The secrets stay "+
			"in this file and never touch the tree, the API or an export")
	defaultRole := fs.String("default-role", "viewer",
		"what an authenticated user holding no grant may do: `viewer` or none. "+
			"It is a setting rather than a consequence, so adding the first grant "+
			"does not silently lock out everyone who could already read")
	metricsPublic := fs.Bool("metrics-public", false,
		"serve /metrics without a session so Prometheus can scrape it")
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

	if *externalURL != "" {
		u, err := url.Parse(*externalURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("--external-url must be an absolute URL like https://smokeng.example.org, got %q", *externalURL)
		}
		if u.Path != "" && u.Path != "/" {
			return fmt.Errorf("--external-url is a base address and takes no path, got %q", *externalURL)
		}
		*externalURL = strings.TrimSuffix(*externalURL, "/")
	}

	trusted, err := api.ParseTrustedProxies(*trustedProxies)
	if err != nil {
		return fmt.Errorf("--trusted-proxies: %w", err)
	}

	if err := probe.TrustCAFiles(splitList(*tlsCAFiles)); err != nil {
		return fmt.Errorf("--tls-ca-file: %w", err)
	}
	if err := loadIRTTHMACKeys(*irttHMACKeys); err != nil {
		return fmt.Errorf("--irtt-hmac-keys: %w", err)
	}

	switch auth.Role(*defaultRole) {
	case auth.RoleViewer, auth.RoleNone:
	default:
		return fmt.Errorf("--default-role must be viewer or none, got %q", *defaultRole)
	}

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
			// Behind a proxy the browser is sent to the external address, not
			// the listen address, so derive it from the one the operator has
			// already told us about rather than making them repeat it.
			if *externalURL != "" {
				redirect = *externalURL + "/auth/callback"
			} else {
				redirect = "http://" + *listen + "/auth/callback"
			}
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
			//
			// The listen address alone is not enough to decide that. A proxy
			// terminating TLS on the same host leaves smokeng listening on
			// loopback while the browser is on https, and dropping Secure
			// there means the session cookie is one downgrade away from
			// travelling in the clear. When the external address is known, it
			// is what the browser actually used, so it decides.
			Insecure: cookieInsecure(*externalURL, *listen),
		}, key)
		if err != nil {
			return err
		}
		log.Printf("authentication enabled via %s", *oidcIssuer)
	}

	// Rules are always evaluated. They used to be skipped without a webhook,
	// on the grounds that there was nowhere to send the result — but firing
	// state and the transition log are both visible in the UI, so evaluating
	// is useful on its own and a missing webhook now means only that nothing
	// is posted anywhere.
	var notifier alert.Notifier
	if *webhook != "" {
		notifier = &alert.Webhook{URL: *webhook}
		log.Printf("alerting enabled, posting to %s", *webhook)
	} else {
		log.Printf("alerting evaluated but not delivered: no --alert-webhook is set")
	}
	alerts := alert.NewManager(st, notifier)

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
		Addr: *listen,
		Handler: api.New(st, api.Options{
			Alerts: alertViewOrNil(alerts), Auth: authOrNil(authenticator),
			Probe: eng, Version: version, MetricsPublic: *metricsPublic,
			AgentCAs:    probe.LocalCAPEMs(),
			DefaultRole: auth.Role(*defaultRole), ExternalURL: *externalURL,
			TrustedProxies: trusted,
		}, dist),
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

// cookieInsecure reports whether the session cookie may go out without the
// Secure attribute: only when nothing suggests a browser will ever carry it
// over TLS.
func cookieInsecure(externalURL, listen string) bool {
	if externalURL != "" {
		return !strings.HasPrefix(externalURL, "https://")
	}
	return isLoopback(listen)
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

// splitList turns a comma-separated flag into its non-empty entries. Trailing
// commas and stray spaces are an operator's typo, not a request to open a file
// named "".
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadIRTTHMACKeys reads a keyfile mapping irtt endpoints to their shared HMAC
// secrets and installs it. The file is TOML, one entry per line:
//
//	"resolver.gemeentex.nl:2112" = "the-shared-secret"
//	"10.20.0.5:2112"             = "another-secret"
//
// The endpoint is the target's configured host and port — the host as written
// in the target, not its resolved address — so it stays stable across DNS
// changes. An empty path installs nothing. The secrets live only in this file
// on this host (deploy it from a vault); they never enter the target tree, the
// API or an export.
func loadIRTTHMACKeys(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]string
	if err := toml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("not a TOML map of \"host:port\" = \"key\": %w", err)
	}
	m := make(map[string][]byte, len(raw))
	for endpoint, key := range raw {
		if key == "" {
			return fmt.Errorf("empty HMAC key for endpoint %q", endpoint)
		}
		m[endpoint] = []byte(key)
	}
	probe.SetIRTTHMACKeys(m)
	return nil
}
