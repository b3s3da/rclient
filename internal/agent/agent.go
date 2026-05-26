// Package agent implements the long-running client that runs on each
// remote box. It connects to the central server over WebSocket, reports
// metrics on a schedule, and executes commands sent by the server.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"rclient/internal/proto"
)

// Version of the agent. Overridable at build time via:
//   go build -ldflags="-X rclient/internal/agent.Version=v1.2.3" ...
// (variable, not const, so -X can mutate it).
var Version = "0.1.0"

// Config holds runtime configuration for the agent.
type Config struct {
	ServerURL    string        // wss://r.example.com:13337/ws/agent
	Token        string        // shared bearer secret
	StateDir     string        // where to persist the agent uuid
	MetricsEvery time.Duration // metrics interval
	Insecure     bool          // skip TLS verify (for local testing only)
}

type Agent struct {
	cfg Config
	id  string
	log *slog.Logger

	mu     sync.Mutex
	ws     *websocket.Conn
	shells *shellRegistry

	secretMu sync.Mutex
	secret   string // per-agent enrollment secret, persisted on disk
}

func New(cfg Config, log *slog.Logger) (*Agent, error) {
	if cfg.MetricsEvery <= 0 {
		cfg.MetricsEvery = 5 * time.Second
	}
	id, err := loadOrCreateID(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		cfg:    cfg,
		id:     id,
		log:    log.With("agent", id),
		shells: newShellRegistry(),
	}
	a.secret = loadSecret(cfg.StateDir)
	return a, nil
}

// Run keeps trying to maintain a session until ctx is canceled.
func (a *Agent) Run(ctx context.Context) {
	delay := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		err := a.session(ctx)
		if ctx.Err() != nil {
			return
		}
		a.log.Warn("session ended", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
}

// session establishes one WebSocket connection and serves it until something
// goes wrong, at which point it returns and Run will reconnect.
func (a *Agent) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	httpClient := &http.Client{}
	if a.cfg.Insecure {
		httpClient = newInsecureClient()
	}

	ws, _, err := websocket.Dial(dialCtx, a.cfg.ServerURL, &websocket.DialOptions{
		HTTPClient: httpClient,
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + a.cfg.Token},
			"User-Agent":    []string{"rclient-agent/" + Version},
		},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(1 << 20)

	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.ws = nil
		a.mu.Unlock()
		// Disconnect always means: kill all running shells. We don't try to
		// keep them alive across reconnects to avoid orphaned root processes
		// that nobody is watching.
		a.shells.closeAll()
		_ = ws.Close(websocket.StatusNormalClosure, "")
	}()

	if err := a.sendHello(ctx); err != nil {
		return fmt.Errorf("hello: %w", err)
	}

	// Wait for Welcome before doing anything else. If the server issued a
	// fresh secret (first-contact enrollment), persist it before we accept
	// any commands — otherwise a crash here would leave us with a server
	// that knows our secret and a client that doesn't.
	if err := a.readWelcome(ctx, ws); err != nil {
		return fmt.Errorf("welcome: %w", err)
	}

	a.log.Info("connected")

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	go a.metricsLoop(sessCtx)

	// Reader loop, the main thread of the session.
	for {
		var env proto.Envelope
		_, data, err := ws.Read(sessCtx)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &env); err != nil {
			a.log.Warn("bad envelope", "err", err)
			continue
		}
		a.dispatch(sessCtx, env)
	}
}

func (a *Agent) dispatch(ctx context.Context, env proto.Envelope) {
	switch env.Type {
	case proto.TypePing:
		a.send(ctx, proto.Envelope{Type: proto.TypePong})
	case proto.TypeShellOpen:
		var o proto.ShellOpen
		if err := json.Unmarshal(env.Data, &o); err != nil {
			return
		}
		a.openShell(ctx, o)
	case proto.TypeShellInput:
		var in proto.ShellInput
		if err := json.Unmarshal(env.Data, &in); err != nil {
			return
		}
		a.shellInput(in)
	case proto.TypeShellResize:
		var rs proto.ShellResize
		if err := json.Unmarshal(env.Data, &rs); err != nil {
			return
		}
		a.shellResize(rs)
	case proto.TypeShellClose:
		var c proto.ShellClose
		if err := json.Unmarshal(env.Data, &c); err != nil {
			return
		}
		a.shellClose(c)
	}
}

// send marshals an envelope and writes it. Safe to call concurrently;
// the underlying *websocket.Conn serializes writes internally.
func (a *Agent) send(ctx context.Context, env proto.Envelope) {
	a.mu.Lock()
	ws := a.ws
	a.mu.Unlock()
	if ws == nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	if err := ws.Write(wctx, websocket.MessageText, data); err != nil {
		a.log.Debug("write failed", "err", err)
	}
}

func (a *Agent) sendHello(ctx context.Context) error {
	host, _ := os.Hostname()
	a.secretMu.Lock()
	secret := a.secret
	a.secretMu.Unlock()
	hello := proto.Hello{
		AgentID:  a.id,
		Secret:   secret,
		Hostname: host,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Version:  Version,
		Kernel:   kernelVersion(),
	}
	raw, _ := json.Marshal(hello)
	a.send(ctx, proto.Envelope{Type: proto.TypeHello, Data: raw})
	return nil
}

// readWelcome consumes the Welcome message the server sends right after a
// successful Hello. If it carries a fresh secret, persist it.
func (a *Agent) readWelcome(ctx context.Context, ws *websocket.Conn) error {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, data, err := ws.Read(rctx)
	if err != nil {
		return err
	}
	var env proto.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("bad welcome envelope: %w", err)
	}
	if env.Type != proto.TypeWelcome {
		return fmt.Errorf("expected welcome, got %s", env.Type)
	}
	var w proto.Welcome
	if err := json.Unmarshal(env.Data, &w); err != nil {
		return err
	}
	if w.IssuedSecret != "" {
		if err := a.saveSecret(w.IssuedSecret); err != nil {
			return fmt.Errorf("persist secret: %w", err)
		}
		a.log.Info("enrolled, secret saved")
	}
	return nil
}

// loadSecret reads the persisted enrollment secret from disk. Returns empty
// string if none exists yet, which is expected on first contact.
func loadSecret(dir string) string {
	if dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "agent.secret"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	return s
}

// saveSecret persists a freshly issued secret. The agent must successfully
// store it before announcing enrollment success — otherwise the next
// reconnect would fail and we'd be stuck in a loop.
func (a *Agent) saveSecret(s string) error {
	a.secretMu.Lock()
	a.secret = s
	a.secretMu.Unlock()
	if a.cfg.StateDir == "" {
		return nil
	}
	p := filepath.Join(a.cfg.StateDir, "agent.secret")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(s+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// loadOrCreateID returns a stable uuid for this agent, persisting it under
// stateDir/agent.id on first run.
func loadOrCreateID(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "agent.id")
	if b, err := os.ReadFile(p); err == nil {
		s := string(b)
		// trim trailing whitespace
		for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\r') {
			s = s[:len(s)-1]
		}
		if s != "" {
			return s, nil
		}
	}
	id := uuid.NewString()
	if err := os.WriteFile(p, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}
