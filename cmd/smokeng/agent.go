package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/timdebruijn/smokeng/internal/agent"
	"github.com/timdebruijn/smokeng/internal/probe"
	"github.com/timdebruijn/smokeng/internal/store"
)

func agentCmd(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "add":
		return agentAdd(args[1:])
	case "list":
		return agentList(args[1:])
	case "remove":
		return agentSetState(args[1:], "remove")
	case "disable":
		return agentSetState(args[1:], "disable")
	case "enable":
		return agentSetState(args[1:], "enable")
	case "run":
		return agentRun(args[1:])
	default:
		return fmt.Errorf("unknown agent subcommand %q", args[0])
	}
}

// agentAdd enrols an agent on the master. Enrolment is manual by design:
// there is no bootstrap-token flow, because each agent is added once and the
// token machinery would be more to get wrong than it saves.
func agentAdd(args []string) error {
	fs := flag.NewFlagSet("agent add", flag.ExitOnError)
	dbPath := fs.String("db", "smokeng.db", "path to the SQLite database")
	name := fs.String("name", "", "agent name, as used in a target's agents setting")
	pubkey := fs.String("pubkey", "", "the agent's public key, as printed by 'smokeng agent run'")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *pubkey == "" {
		return errors.New("usage: smokeng agent add --name NAME --pubkey BASE64")
	}
	raw, err := base64.StdEncoding.DecodeString(*pubkey)
	if err != nil {
		return fmt.Errorf("public key is not valid base64: %w", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	rec, err := st.AddAgent(context.Background(), *name, ed25519.PublicKey(raw))
	if err != nil {
		return err
	}
	fmt.Printf("enrolled agent %q with id %d\n", rec.Name, rec.ID)
	fmt.Printf("assign targets to it by setting `agents` to include %q, "+
		"then start it with --agent-id %d\n", rec.Name, rec.ID)
	return nil
}

func agentList(args []string) error {
	fs := flag.NewFlagSet("agent list", flag.ExitOnError)
	dbPath := fs.String("db", "smokeng.db", "path to the SQLite database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	agents, err := st.ListAgents(context.Background())
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTATE\tLAST SEEN")
	for _, a := range agents {
		state := "enabled"
		if !a.Enabled {
			state = "disabled"
		}
		if a.ID == store.LocalAgentID {
			state += " (built in)"
		}
		seen := "never"
		if a.LastSeen != 0 {
			seen = time.Unix(a.LastSeen, 0).Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", a.ID, a.Name, state, seen)
	}
	return w.Flush()
}

func agentSetState(args []string, action string) error {
	fs := flag.NewFlagSet("agent "+action, flag.ExitOnError)
	dbPath := fs.String("db", "smokeng.db", "path to the SQLite database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: smokeng agent %s [--db path] AGENT-ID", action)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("bad agent id %q", fs.Arg(0))
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch action {
	case "remove":
		if err := st.RemoveAgent(ctx, id); err != nil {
			return err
		}
		fmt.Printf("removed agent %d; the measurements it submitted are kept\n", id)
	case "disable":
		if err := st.SetAgentEnabled(ctx, id, false); err != nil {
			return err
		}
		fmt.Printf("disabled agent %d; its submissions will be rejected\n", id)
	case "enable":
		if err := st.SetAgentEnabled(ctx, id, true); err != nil {
			return err
		}
		fmt.Printf("enabled agent %d\n", id)
	}
	return nil
}

// agentRun is the remote node: probe locally, buffer locally, push to the
// master.
func agentRun(args []string) error {
	fs := flag.NewFlagSet("agent run", flag.ExitOnError)
	master := fs.String("master", "", "master base URL, e.g. https://smokeng.example.org")
	agentID := fs.Int64("agent-id", 0, "this agent's id, as printed by 'smokeng agent add'")
	keyPath := fs.String("key", "probe.key", "path to this agent's private key")
	dbPath := fs.String("db", "smokeng-agent.db", "local buffer database")
	token := fs.String("token", "",
		"one-time enrolment token from the master; enrols this agent and records its id")
	insecure := fs.Bool("insecure-allow-http", false,
		"allow a plain-HTTP master URL; local development only. Note an enrolment "+
			"token is a usable credential, so this is not merely a transport preference")
	if err := fs.Parse(args); err != nil {
		return err
	}

	key, created, err := agent.LoadOrCreateKey(*keyPath)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("generated a new key in %s\n", *keyPath)
	}
	// Always print it: an operator enrolling this agent by hand needs it, and
	// it is public by definition.
	fmt.Printf("public key: %s\n", agent.PublicKey(key))

	// An id already recorded by a previous enrolment wins over the flag being
	// absent, so a unit file that carries --token does not try to spend it
	// again on every restart.
	if *agentID == 0 {
		if id, ok, err := agent.LoadAgentID(*keyPath); err != nil {
			return err
		} else if ok {
			*agentID = id
		}
	}
	if *master != "" && *agentID == 0 && *token != "" {
		id, err := agent.EnrolOnce(context.Background(), *master, *token, *keyPath, key, *insecure)
		if err != nil {
			return err
		}
		fmt.Printf("enrolled with id %d; recorded in %s, so --token is not needed again\n",
			id, agent.IDPath(*keyPath))
		*agentID = id
	}
	if *master == "" || *agentID == 0 {
		return errors.New("this agent is not enrolled yet. Either:\n" +
			"  mint a token in the UI under Agents, then rerun with --master URL --token smk_...\n" +
			"or enrol the key by hand on the master:\n" +
			"  smokeng agent add --name NAME --pubkey <the key above>\n" +
			"  then rerun with --master URL --agent-id ID")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	a, err := agent.New(agent.Config{
		Master: *master, AgentID: *agentID, KeyPath: *keyPath,
		DBPath: *dbPath, Insecure: *insecure,
	}, key, st)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The same probing engine the master runs locally: an agent measures
	// exactly as the master would, or the two are not comparable.
	eng, err := probe.NewEngine(st, nil)
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

	log.Printf("smokeng agent %d reporting to %s (buffer: %s)", *agentID, *master, *dbPath)
	err = a.Run(ctx)
	// Order matters on shutdown: the engine writes its final batch into the
	// buffer, and only then is there anything worth a last drain.
	<-engDone
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	a.Drain(drainCtx)

	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
