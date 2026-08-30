package main

import (
	"bytes"
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
	case "key":
		return agentKey(args[1:])
	default:
		return fmt.Errorf("unknown agent subcommand %q", args[0])
	}
}

// agentKey creates the agent's private key if it does not exist yet and prints
// the public half, without starting anything.
//
// The manual route is to start the agent, read its public key off the console
// and stop it again. That is fine by hand and awkward from a provisioning
// tool, which needs the key before it can write the unit that would print it.
// Splitting the two makes "generate identity" and "start measuring" separate
// steps, which is what they always were.
//
// It is deliberately idempotent: run against an existing key it prints that
// key and changes nothing, so re-running a playbook does not silently issue a
// new identity and orphan every measurement signed with the old one.
func agentKey(args []string) error {
	fs := flag.NewFlagSet("agent key", flag.ExitOnError)
	keyPath := fs.String("key", "probe.key", "path to this agent's private key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	priv, created, err := agent.LoadOrCreateKey(*keyPath)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(os.Stderr, "created a new private key at %s\n", *keyPath)
	}
	// The public key alone on stdout, so a caller can capture it without
	// having to parse around a sentence.
	fmt.Println(agent.PublicKey(priv))
	return nil
}

// agentAdd enrols an agent on the master by pasting in its public key. This
// is the manual path, for anyone who would rather not have a token in
// flight at all; `smokeng agent run --token` (see agentRun) is the shorter
// route for provisioning more than a couple of agents.
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

	ctx := context.Background()
	// Enrolling an agent that is already enrolled with this very key is a
	// no-op, not an error. Provisioning tools run the same step every pass,
	// and failing the second run would mean either a hand-written "only if
	// absent" guard around it or a playbook that cannot be run twice.
	//
	// The same name with a *different* key is the opposite: either a key was
	// regenerated and every measurement signed with the old one is about to
	// stop verifying, or something is trying to take over an identity. Both
	// deserve a stop rather than a silent overwrite.
	existing, err := st.ListAgents(ctx)
	if err != nil {
		return err
	}
	for _, a := range existing {
		if a.Name != *name {
			continue
		}
		if !bytes.Equal(a.PubKey, raw) {
			return fmt.Errorf("agent %q is already enrolled with a different public key; "+
				"remove it with 'smokeng agent remove %d' if you meant to re-key it, "+
				"which invalidates the old key", *name, a.ID)
		}
		fmt.Println(a.ID)
		fmt.Fprintf(os.Stderr, "agent %q is already enrolled with this key, as id %d\n", a.Name, a.ID)
		return nil
	}

	rec, err := st.AddAgent(ctx, *name, ed25519.PublicKey(raw))
	if err != nil {
		return err
	}
	// The id alone on stdout, so a caller can capture it; everything a person
	// needs to read goes to stderr, where it does not have to be parsed around.
	fmt.Println(rec.ID)
	fmt.Fprintf(os.Stderr, "enrolled agent %q with id %d\n", rec.Name, rec.ID)
	fmt.Fprintf(os.Stderr, "assign targets to it by setting `agents` to include %q, "+
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
		"allow a plain-HTTP master URL that is not on loopback; note an enrolment "+
			"token is a usable credential, so this is not merely a transport preference. "+
			"A loopback master needs no flag: that traffic never leaves the host")
	tlsCAFiles := fs.String("tls-ca-file", "",
		"comma-separated PEM files whose certificates https probes trust, in addition "+
			"to the system roots. An agent verifies certificates itself, so it needs "+
			"the same CA the master was given — the master does not supply it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := probe.TrustCAFiles(splitList(*tlsCAFiles)); err != nil {
		return fmt.Errorf("--tls-ca-file: %w", err)
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
		DBPath: *dbPath, Insecure: *insecure, Version: version,
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
