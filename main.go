// popfleet: personal, self-owned, account-free fleet terminal.
//
//	popfleet serve   run the broker (panel + API + WS relay)
//	popfleet agent   run the outbound-only agent
//
// All secrets come from the environment, never flags (flags show in `ps`).
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/ejoliet/popfleet/internal/agent"
	"github.com/ejoliet/popfleet/internal/broker"
	"github.com/ejoliet/popfleet/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  popfleet serve [--addr 127.0.0.1:7333] [--state popfleet.json] [--insecure]
      env: POPFLEET_ADMIN_TOKEN (required)
  popfleet agent [--name <display name>]
      env: POPFLEET_URL, POPFLEET_TOKEN (required), POPFLEET_NAME`)
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7333", "listen address")
	state := fs.String("state", "popfleet.json", "path to the JSON state file")
	insecure := fs.Bool("insecure", false, "allow binding a non-loopback address without TLS")
	fs.Parse(args)

	admin := os.Getenv("POPFLEET_ADMIN_TOKEN")
	if admin == "" {
		log.Fatal("refusing to start: POPFLEET_ADMIN_TOKEN is not set (env only — never a flag, flags show in `ps`)")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("bad --addr %q: %v", *addr, err)
	}
	if !isLoopback(host) && !*insecure {
		log.Fatalf("refusing to bind non-loopback address %q without TLS.\n"+
			"Front the broker with TLS you own instead, e.g. Caddyfile:\n"+
			"    fleet.example.com {\n        reverse_proxy 127.0.0.1:7333\n    }\n"+
			"or pass --insecure if you really know what you are doing.", *addr)
	}

	st, err := store.Open(*state)
	if err != nil {
		log.Fatalf("state file %s: %v", *state, err)
	}
	// Bind before announcing: printing "listening" and then failing to bind is
	// the single most confusing thing a server can do on first run.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v\n"+
			"Something else is already there — try --addr 127.0.0.1:7334", *addr, err)
	}
	log.Printf("popfleet broker listening on http://%s (state: %s)", ln.Addr(), *state)
	log.Printf("open the panel and paste your POPFLEET_ADMIN_TOKEN when it asks")
	log.Fatal(http.Serve(ln, broker.New(st, admin).Handler()))
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	// A display name is not a secret, so a flag is fine; the token is not.
	name := fs.String("name", "", "display name (overrides POPFLEET_NAME)")
	fs.Parse(args)

	url := os.Getenv("POPFLEET_URL")
	token := os.Getenv("POPFLEET_TOKEN")
	if url == "" {
		log.Fatal("refusing to start: POPFLEET_URL is not set")
	}
	if token == "" {
		log.Fatal("refusing to start: POPFLEET_TOKEN is not set (env only — never a flag, flags show in `ps`)")
	}
	n := *name
	if n == "" {
		n = os.Getenv("POPFLEET_NAME")
	}
	if n == "" {
		n, _ = os.Hostname()
	}
	log.Fatal(agent.Run(url, token, n))
}
