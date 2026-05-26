//go:build !windows

package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/creack/pty"

	"rclient/internal/proto"
)

// shellRCContent is sourced by every shell we open, so colour output and a
// readable prompt are on by default even on freshly imaged boxes that don't
// have a personal .bashrc tuned. It only sets things if they aren't set
// already, so users' own preferences win when they exist.
const shellRCContent = `# managed by rclient — re-applied each time a shell opens.
[ -t 1 ] || return 0

# Source the user's regular rc files first so personal aliases still apply.
[ -f /etc/profile ]      && . /etc/profile     2>/dev/null
[ -f "$HOME/.bashrc" ]   && . "$HOME/.bashrc"  2>/dev/null
[ -f "$HOME/.profile" ]  && . "$HOME/.profile" 2>/dev/null

export TERM=xterm-256color
export CLICOLOR=1
export LANG="${LANG:-C.UTF-8}"
export LC_ALL="${LC_ALL:-C.UTF-8}"

# GNU-style colours for ls / grep / diff.
if ls --color=auto -d / >/dev/null 2>&1; then
    alias ls='ls --color=auto'
    alias ll='ls -lah --color=auto'
    alias la='ls -A --color=auto'
fi
# BSD/busybox ls fallback.
if ls -G -d / >/dev/null 2>&1; then
    alias ls='ls -G'
fi
alias grep='grep --color=auto' 2>/dev/null
alias egrep='egrep --color=auto' 2>/dev/null
alias fgrep='fgrep --color=auto' 2>/dev/null
alias diff='diff --color=auto' 2>/dev/null
alias ip='ip -c' 2>/dev/null

# A pleasant default LS_COLORS for boxes without dircolors set up.
if [ -z "$LS_COLORS" ]; then
    export LS_COLORS='rs=0:di=01;38;5;81:ln=01;38;5;213:ex=01;38;5;120:*.tar=38;5;215:*.gz=38;5;215:*.zip=38;5;215:*.log=38;5;245:*.md=38;5;229'
fi

# Coloured man pages (works on bash and most ash/dash).
export LESS_TERMCAP_md=$'\e[01;38;5;81m'
export LESS_TERMCAP_us=$'\e[01;38;5;213m'
export LESS_TERMCAP_so=$'\e[38;5;229;48;5;236m'
export LESS_TERMCAP_me=$'\e[0m'
export LESS_TERMCAP_ue=$'\e[0m'
export LESS_TERMCAP_se=$'\e[0m'

# Two-colour prompt: cyan host, magenta path, green/red status indicator.
# Only if the user hasn't already customised PS1 to something non-default.
case "${PS1:-}" in
    ''|'\\s-\\v\\$ '|'\\u@\\h:\\w\\$ '|'\\h:\\w\\$ '|'$ '|'# ')
        if [ -n "$BASH_VERSION" ]; then
            PS1='\[\e[38;5;81m\]\u@\h\[\e[0m\] \[\e[38;5;213m\]\w\[\e[0m\] \[\e[38;5;120m\]\$\[\e[0m\] '
        else
            # POSIX-y prompt for ash / dash / busybox sh.
            PS1='\033[38;5;81m$(whoami)@$(hostname -s 2>/dev/null || hostname)\033[0m \033[38;5;213m$(pwd)\033[0m \033[38;5;120m$\033[0m '
        fi
        ;;
esac
`

// ensureShellRC writes the rc file under the agent state dir if it isn't
// already there, and returns its absolute path. Returns "" if the file
// can't be written (we'll just open a plain shell in that case).
func (a *Agent) ensureShellRC() string {
	if a.cfg.StateDir == "" {
		return ""
	}
	p := filepath.Join(a.cfg.StateDir, "shell.rc")
	if err := os.WriteFile(p, []byte(shellRCContent), 0o600); err != nil {
		return ""
	}
	return p
}
type shellSession struct {
	id     string
	cmd    *exec.Cmd
	pty    *os.File
	closed chan struct{}
	once   sync.Once
}

// shellRegistry tracks all PTYs started by this agent. Lookup is by shell id.
type shellRegistry struct {
	mu       sync.Mutex
	sessions map[string]*shellSession
}

func newShellRegistry() *shellRegistry {
	return &shellRegistry{sessions: map[string]*shellSession{}}
}

func (r *shellRegistry) put(s *shellSession) {
	r.mu.Lock()
	r.sessions[s.id] = s
	r.mu.Unlock()
}

func (r *shellRegistry) get(id string) *shellSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

func (r *shellRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
}

// closeAll terminates every running shell. Called when the WS session ends so
// no orphan PTYs remain after a disconnect.
func (r *shellRegistry) closeAll() {
	r.mu.Lock()
	for _, s := range r.sessions {
		s.terminate()
	}
	r.sessions = map[string]*shellSession{}
	r.mu.Unlock()
}

// terminate kills the underlying process and closes the PTY exactly once.
func (s *shellSession) terminate() {
	s.once.Do(func() {
		_ = s.pty.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
}

// openShell starts a new login shell under a PTY and pumps its output to the
// panel via the agent's send method.
func (a *Agent) openShell(ctx context.Context, open proto.ShellOpen) {
	cols := open.Cols
	rows := open.Rows
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 32
	}

	rc := a.ensureShellRC()

	shell := os.Getenv("SHELL")
	if shell == "" {
		// Reasonable fallbacks.
		for _, candidate := range []string{"/bin/bash", "/bin/sh"} {
			if _, err := os.Stat(candidate); err == nil {
				shell = candidate
				break
			}
		}
	}
	if shell == "" {
		shell = "/bin/sh"
	}

	// Pick how to make the shell source our rc file. bash takes --rcfile,
	// other POSIX shells respect the ENV variable for interactive shells.
	var args []string
	env := append(os.Environ(),
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
	)
	if rc != "" {
		switch filepath.Base(shell) {
		case "bash":
			args = []string{"--rcfile", rc, "-i"}
		default:
			env = append(env, "ENV="+rc)
			args = []string{"-i"}
		}
	} else {
		args = []string{"-l"}
	}

	cmd := exec.Command(shell, args...)
	cmd.Env = env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		raw, _ := json.Marshal(proto.ShellExit{
			ShellID: open.ShellID, ExitCode: -1, Reason: err.Error(),
		})
		a.send(ctx, proto.Envelope{Type: proto.TypeShellExit, Data: raw})
		return
	}
	sess := &shellSession{
		id:     open.ShellID,
		cmd:    cmd,
		pty:    ptmx,
		closed: make(chan struct{}),
	}
	a.shells.put(sess)

	// Reader goroutine: drains PTY output as fast as possible into base64
	// chunks and ships them to the panel.
	go func() {
		defer close(sess.closed)
		buf := make([]byte, 16*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				out := proto.ShellOutput{
					ShellID: sess.id,
					Data:    base64.StdEncoding.EncodeToString(buf[:n]),
				}
				raw, _ := json.Marshal(out)
				a.send(ctx, proto.Envelope{Type: proto.TypeShellOutput, Data: raw})
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for process exit, then notify the panel.
	go func() {
		err := cmd.Wait()
		<-sess.closed // ensure all output has been pushed before reporting exit
		exit := 0
		reason := ""
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exit = ee.ExitCode()
			} else if errors.Is(err, io.EOF) {
				exit = 0
			} else {
				exit = -1
				reason = err.Error()
			}
		}
		a.shells.remove(sess.id)
		raw, _ := json.Marshal(proto.ShellExit{
			ShellID: sess.id, ExitCode: exit, Reason: reason,
		})
		a.send(ctx, proto.Envelope{Type: proto.TypeShellExit, Data: raw})
	}()
}

// shellInput writes keystrokes coming from the panel into the PTY.
func (a *Agent) shellInput(in proto.ShellInput) {
	s := a.shells.get(in.ShellID)
	if s == nil {
		return
	}
	data, err := base64.StdEncoding.DecodeString(in.Data)
	if err != nil {
		return
	}
	_, _ = s.pty.Write(data)
}

// shellResize updates the PTY winsize so curses apps redraw correctly.
func (a *Agent) shellResize(rs proto.ShellResize) {
	s := a.shells.get(rs.ShellID)
	if s == nil || rs.Cols <= 0 || rs.Rows <= 0 {
		return
	}
	_ = pty.Setsize(s.pty, &pty.Winsize{Cols: uint16(rs.Cols), Rows: uint16(rs.Rows)})
}

// shellClose terminates a single shell on demand.
func (a *Agent) shellClose(c proto.ShellClose) {
	s := a.shells.get(c.ShellID)
	if s == nil {
		return
	}
	s.terminate()
}
