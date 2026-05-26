//go:build !windows

package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"
)

const (
	binDest     = "/usr/local/bin/rclient-agent"
	confDir     = "/etc/rclient"
	confFile    = "/etc/rclient/agent.env"
	stateDir    = "/var/lib/rclient"
	systemdUnit = "/etc/systemd/system/rclient-agent.service"
	openrcInit  = "/etc/init.d/rclient-agent"
)

const systemdUnitContent = `[Unit]
Description=rclient agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/rclient-agent
EnvironmentFile=/etc/rclient/agent.env
Restart=always
RestartSec=5s
KillMode=control-group
TimeoutStopSec=10
StateDirectory=rclient
StateDirectoryMode=0700
# Note: we deliberately do NOT enable ProtectHome / ProtectSystem here.
# The agent gives an interactive root shell by design — sandboxing the
# parent doesn't add real security, but it would break any tool the
# operator runs that touches /root, /home, /usr/local, etc.
NoNewPrivileges=false

[Install]
WantedBy=multi-user.target
`

const openrcInitContent = `#!/sbin/openrc-run

name="rclient-agent"
description="rclient agent"
command="/usr/local/bin/rclient-agent"
command_user="root"
command_background="yes"
pidfile="/run/rclient-agent.pid"
output_log="/var/log/rclient-agent.log"
error_log="/var/log/rclient-agent.log"
respawn_delay=5
respawn_max=0

depend() {
	need net
	after firewall
}

start_pre() {
	if [ -f /etc/rclient/agent.env ]; then
		set -a
		. /etc/rclient/agent.env
		set +a
	fi
	checkpath -d -m 0700 /var/lib/rclient
}

supervisor=supervise-daemon
`

// runInstall implements the `rclient-agent install` subcommand. It copies
// the running binary to /usr/local/bin, writes the env file, drops the
// init unit appropriate for the host, and starts the service.
//
// Interactive flow expects a single "connect blob" produced by
// deploy/setup.sh on the server. Power users can still pass --url + --token
// or env vars, but those are no longer prompted for.
func runInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	urlFlag := fs.String("url", os.Getenv("RCLIENT_URL"), "server URL incl. agent path (advanced)")
	tokFlag := fs.String("token", os.Getenv("RCLIENT_TOKEN"), "shared bearer token (advanced)")
	connectFlag := fs.String("connect", os.Getenv("RCLIENT_CONNECT"), "single-line connect blob from `setup.sh`")
	_ = fs.Parse(args)

	ensureRoot()

	url, tok := *urlFlag, *tokFlag

	// Prefer a connect blob — it's what the server hands to you. Decode it
	// even if --url/--token are also given so explicit flags can override.
	connect := *connectFlag
	if connect == "" && url == "" && tok == "" {
		// Truly nothing supplied — ask for the blob (and only the blob).
		connect = promptSecret("Paste the connect token from your server")
	}
	if connect != "" {
		u, t, err := decodeConnect(connect)
		if err != nil {
			die("bad connect token: %v", err)
		}
		if url == "" {
			url = u
		}
		if tok == "" {
			tok = t
		}
	}

	if url == "" || tok == "" {
		die("could not derive URL+token; pass --connect or both --url and --token")
	}

	initSys, err := detectInit()
	if err != nil {
		die("%v", err)
	}
	fmt.Println("init system:", initSys)

	// Copy the running binary to the canonical location, atomically.
	self, err := os.Executable()
	if err != nil {
		die("can't find own path: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if absSelf, _ := filepath.Abs(self); absSelf != binDest {
		if err := copyFileAtomic(self, binDest, 0o755); err != nil {
			die("copy binary: %v", err)
		}
		fmt.Println("installed", binDest)
	} else {
		fmt.Println("binary already at", binDest)
	}

	// Write the env file.
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		die("mkdir %s: %v", confDir, err)
	}
	body := fmt.Sprintf("RCLIENT_URL=%s\nRCLIENT_TOKEN=%s\nRCLIENT_STATE=%s\n", url, tok, stateDir)
	if err := writeFileAtomic(confFile, []byte(body), 0o600); err != nil {
		die("write %s: %v", confFile, err)
	}
	fmt.Println("wrote", confFile)

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		die("mkdir %s: %v", stateDir, err)
	}

	// Drop the init file and (re)start the service.
	switch initSys {
	case "systemd":
		if err := os.WriteFile(systemdUnit, []byte(systemdUnitContent), 0o644); err != nil {
			die("write unit: %v", err)
		}
		run("systemctl", "daemon-reload")
		run("systemctl", "enable", "rclient-agent")
		run("systemctl", "restart", "rclient-agent")
		fmt.Println("service started. logs: journalctl -u rclient-agent -f")
	case "openrc":
		if err := os.WriteFile(openrcInit, []byte(openrcInitContent), 0o755); err != nil {
			die("write init: %v", err)
		}
		run("rc-update", "add", "rclient-agent", "default")
		run("rc-service", "rclient-agent", "restart")
		fmt.Println("service started. logs: tail -f /var/log/rclient-agent.log")
	}
}

// runUninstall stops the service, removes the unit and the binary. The
// agent.env and the persistent state (agent uuid) are left in place so a
// later reinstall picks up the same identity.
func runUninstall(args []string) {
	_ = args
	ensureRoot()
	initSys, err := detectInit()
	if err != nil {
		die("%v", err)
	}
	switch initSys {
	case "systemd":
		runQuiet("systemctl", "disable", "--now", "rclient-agent")
		_ = os.Remove(systemdUnit)
		runQuiet("systemctl", "daemon-reload")
	case "openrc":
		runQuiet("rc-service", "rclient-agent", "stop")
		runQuiet("rc-update", "del", "rclient-agent", "default")
		_ = os.Remove(openrcInit)
	}
	_ = os.Remove(binDest)
	fmt.Printf("removed binary and unit. %s and %s left untouched — delete manually to fully wipe.\n",
		confDir, stateDir)
}

// decodeConnect parses a base64-url blob produced by deploy/setup.sh that
// bundles the server URL and shared token into a single copy-pasteable
// string, so installing an agent is one command.
func decodeConnect(s string) (url, token string, err error) {
	s = strings.TrimSpace(s)
	// Be permissive about padding and the std-vs-url alphabet; setup.sh
	// strips padding and uses the URL-safe alphabet.
	dec := base64.RawURLEncoding
	raw, derr := dec.DecodeString(s)
	if derr != nil {
		// Try standard base64 with padding as a fallback.
		raw, derr = base64.StdEncoding.DecodeString(s)
	}
	if derr != nil {
		return "", "", derr
	}
	var v struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", "", err
	}
	if v.URL == "" || v.Token == "" {
		return "", "", errors.New("connect blob missing url or token")
	}
	return v.URL, v.Token, nil
}

// --- helpers ---

func ensureRoot() {
	if syscall.Geteuid() != 0 {
		die("must run as root: sudo %s %s", filepath.Base(os.Args[0]), os.Args[1])
	}
}

func detectInit() (string, error) {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return "systemd", nil
	}
	if _, err := exec.LookPath("rc-service"); err == nil {
		return "openrc", nil
	}
	return "", errors.New("no supported init system detected (need systemd or openrc)")
}

func readExistingConfig() (url, tok string) {
	data, err := os.ReadFile(confFile)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "RCLIENT_URL="):
			url = strings.TrimPrefix(line, "RCLIENT_URL=")
		case strings.HasPrefix(line, "RCLIENT_TOKEN="):
			tok = strings.TrimPrefix(line, "RCLIENT_TOKEN=")
		}
	}
	return
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func run(cmd string, args ...string) {
	c := exec.Command(cmd, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		die("%s %s failed: %v", cmd, strings.Join(args, " "), err)
	}
}

func runQuiet(cmd string, args ...string) {
	c := exec.Command(cmd, args...)
	_ = c.Run()
}

func promptLine(prompt, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", prompt, defaultVal)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	line = strings.TrimRight(line, "\r\n \t")
	if line == "" {
		return defaultVal
	}
	return line
}

func promptSecret(prompt string) string {
	fmt.Print(prompt + ": ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		buf, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			die("read failed: %v", err)
		}
		return strings.TrimSpace(string(buf))
	}
	// Not a TTY (e.g. piped) — fall back to a normal line read.
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	return strings.TrimRight(line, "\r\n \t")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
