// rclient-agent CLI.
//
// Subcommands:
//   (no args)        run as the daemon (this is what systemd/OpenRC invoke)
//   install          copy myself to /usr/local/bin and create the unit
//   uninstall        stop and remove unit + binary
//   start / stop     control the running service
//   restart          stop + start
//   status           service state, configured URL, agent id, recent logs
//   logs             follow the service log (`-n N` for backlog size)
//   reconfigure      change URL/token in /etc/rclient/agent.env and restart
//   version          print version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rclient/internal/agent"
)

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "install":
			runInstall(os.Args[2:])
			return
		case "uninstall":
			runUninstall(os.Args[2:])
			return
		case "start", "stop", "restart", "status", "logs", "reconfigure":
			runService(os.Args[1], os.Args[2:])
			return
		case "version", "-v", "--version":
			fmt.Println("rclient-agent", agent.Version)
			return
		case "help", "-h", "--help":
			printUsage()
			return
		}
	}
	runDaemon()
}

func printUsage() {
	fmt.Print(`Usage:
  rclient-agent                          run as a daemon (reads env)

Service management (need root):
  rclient-agent install --connect BLOB             one-line install
  rclient-agent install --url U --token T          explicit
  rclient-agent install                            interactive
  rclient-agent reconfigure --connect BLOB         change URL/token, restart
  rclient-agent start | stop | restart
  rclient-agent uninstall

Inspection:
  rclient-agent status                   service state + recent logs
  rclient-agent logs [-n NUM]            follow the log
  rclient-agent version

Daemon flags (used by the systemd unit, normally you don't run these by hand):
  --url       wss://... (env: RCLIENT_URL)
  --token     shared secret (env: RCLIENT_TOKEN)
  --state     state directory (env: RCLIENT_STATE, default /var/lib/rclient)
  --interval  metrics interval (default 5s)
  --insecure  skip TLS verify (testing only)
`)
}

func runDaemon() {
	url := flag.String("url", envOr("RCLIENT_URL", ""), "server URL")
	token := flag.String("token", envOr("RCLIENT_TOKEN", ""), "shared bearer token")
	state := flag.String("state", envOr("RCLIENT_STATE", "/var/lib/rclient"), "state directory")
	interval := flag.Duration("interval", 5*time.Second, "metrics interval")
	insecure := flag.Bool("insecure", false, "skip TLS verification (testing only)")
	flag.Parse()

	if *url == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "missing --url or --token. Try: rclient-agent install")
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	a, err := agent.New(agent.Config{
		ServerURL:    *url,
		Token:        *token,
		StateDir:     *state,
		MetricsEvery: *interval,
		Insecure:     *insecure,
	}, log)
	if err != nil {
		log.Error("init failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a.Run(ctx)
}

func envOr(k, d string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return d
}
