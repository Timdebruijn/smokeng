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

	"smokeng/internal/api"
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
		fmt.Fprintln(os.Stderr, "smokeng config import/export: not implemented yet (DESIGN.md §7.3)")
		os.Exit(1)
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
  serve      run the master: prober, API and web UI
  config     import/export the target tree as TOML (not implemented yet)
  version    print version`)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", "smokeng.db", "path to the SQLite database")
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	insecure := fs.Bool("i-know-this-is-unauthenticated", false,
		"allow listening on a non-loopback address without authentication (DESIGN.md §7.1)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !isLoopback(*listen) && !*insecure {
		return fmt.Errorf("refusing to listen on non-loopback %q: smokeng has no authentication before v0.3; "+
			"pass --i-know-this-is-unauthenticated to override", *listen)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	dist, err := web.Dist()
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *listen, Handler: api.New(st, dist)}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("smokeng %s listening on http://%s (db: %s)", version, *listen, *dbPath)
	log.Printf("note: probing engine not implemented yet — serving API and UI only")

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case sig := <-sigc:
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
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
