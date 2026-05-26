//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runService dispatches start/stop/restart/status/logs/reconfigure to the
// host init system. It hides the systemctl-vs-rc-service difference behind
// a uniform CLI.
func runService(action string, args []string) {
	switch action {
	case "start":
		ensureRoot()
		serviceCmd("start")
	case "stop":
		ensureRoot()
		serviceCmd("stop")
	case "restart":
		ensureRoot()
		serviceCmd("restart")
	case "status":
		runStatus()
	case "logs":
		runLogs(args)
	case "reconfigure":
		runReconfigure(args)
	}
}

// serviceCmd runs the per-init equivalent of a simple action.
func serviceCmd(action string) {
	initSys, err := detectInit()
	if err != nil {
		die("%v", err)
	}
	switch initSys {
	case "systemd":
		run("systemctl", action, "rclient-agent")
	case "openrc":
		run("rc-service", "rclient-agent", action)
	}
	fmt.Printf("rclient-agent %sed.\n", strings.TrimSuffix(action, "e"))
}

// runStatus prints a short, human-friendly status block: service state,
// effective config (URL only, never the token), agent uuid, last 20 log
// lines.
func runStatus() {
	initSys, err := detectInit()
	if err != nil {
		die("%v", err)
	}

	fmt.Println("init:        ", initSys)

	// Service state.
	switch initSys {
	case "systemd":
		out, _ := exec.Command("systemctl", "is-active", "rclient-agent").Output()
		state := strings.TrimSpace(string(out))
		fmt.Println("service:     ", state)
	case "openrc":
		out, _ := exec.Command("rc-service", "rclient-agent", "status").CombinedOutput()
		fmt.Println("service:     ", strings.TrimSpace(string(out)))
	}

	// Configured URL.
	if data, err := os.ReadFile(confFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "RCLIENT_URL=") {
				fmt.Println("url:         ", strings.TrimPrefix(line, "RCLIENT_URL="))
			}
		}
	} else {
		fmt.Println("url:          (no config at " + confFile + ")")
	}

	// Persistent agent uuid.
	if id, err := os.ReadFile(stateDir + "/agent.id"); err == nil {
		fmt.Println("agent_id:    ", strings.TrimSpace(string(id)))
	}

	// Recent logs.
	fmt.Println("\n--- last 20 log lines ---")
	switch initSys {
	case "systemd":
		c := exec.Command("journalctl", "-u", "rclient-agent", "-n", "20", "--no-pager")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		_ = c.Run()
	case "openrc":
		c := exec.Command("tail", "-n", "20", "/var/log/rclient-agent.log")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		_ = c.Run()
	}
}

// runLogs streams logs in -f mode. With "-n N" you get a bounded backlog.
func runLogs(args []string) {
	initSys, err := detectInit()
	if err != nil {
		die("%v", err)
	}
	// Forward -n NUM if provided; ignore anything else.
	n := "200"
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-n" {
			n = args[i+1]
		}
	}
	switch initSys {
	case "systemd":
		c := exec.Command("journalctl", "-u", "rclient-agent", "-f", "-n", n)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		_ = c.Run()
	case "openrc":
		c := exec.Command("tail", "-n", n, "-F", "/var/log/rclient-agent.log")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		_ = c.Run()
	}
}

// runReconfigure rewrites just the URL/token in /etc/rclient/agent.env and
// restarts the service. Useful when the server endpoint or shared secret
// rotates and you don't want to touch anything else.
func runReconfigure(args []string) {
	ensureRoot()

	urlFlag := ""
	tokFlag := ""
	connect := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--url":
			if i+1 < len(args) {
				urlFlag = args[i+1]
				i++
			}
		case "--token":
			if i+1 < len(args) {
				tokFlag = args[i+1]
				i++
			}
		case "--connect":
			if i+1 < len(args) {
				connect = args[i+1]
				i++
			}
		}
	}

	if connect == "" && urlFlag == "" && tokFlag == "" {
		// Interactive: ask for the connect blob, just like install.
		connect = promptSecret("Paste the connect token from your server")
	}
	if connect != "" {
		u, t, err := decodeConnect(connect)
		if err != nil {
			die("bad connect token: %v", err)
		}
		if urlFlag == "" {
			urlFlag = u
		}
		if tokFlag == "" {
			tokFlag = t
		}
	}
	if urlFlag == "" || tokFlag == "" {
		die("could not derive URL+token; pass --connect or both --url and --token")
	}

	if err := os.MkdirAll(confDir, 0o700); err != nil {
		die("mkdir %s: %v", confDir, err)
	}
	body := fmt.Sprintf("RCLIENT_URL=%s\nRCLIENT_TOKEN=%s\nRCLIENT_STATE=%s\n", urlFlag, tokFlag, stateDir)
	if err := writeFileAtomic(confFile, []byte(body), 0o600); err != nil {
		die("write %s: %v", confFile, err)
	}
	fmt.Println("updated", confFile)

	serviceCmd("restart")
}
