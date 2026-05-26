package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"rclient/internal/proto"
)

// handleAgentWS handles an incoming WebSocket connection from an agent.
// Authentication is a shared bearer token; on mismatch we 404 to keep the
// endpoint invisible to scanners.
func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.cfg.AgentToken {
		http.NotFound(w, r)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // origin check is irrelevant for native clients
	})
	if err != nil {
		s.log.Warn("agent ws accept failed", "err", err)
		return
	}
	// Allow large command output frames.
	ws.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Expect Hello as the very first message.
	hello, err := readHello(ctx, ws)
	if err != nil {
		s.log.Warn("agent hello failed", "err", err)
		_ = ws.Close(websocket.StatusPolicyViolation, "hello required")
		return
	}

	// Enrollment check. New agent_id => server mints a per-agent secret and
	// hands it back. Known agent_id => the presented secret must match. Bad
	// secret = drop the connection. This stops a holder of the shared bearer
	// token from impersonating an existing box.
	issued, err := s.enroll.authenticate(hello.AgentID, hello.Secret)
	if err != nil {
		s.log.Warn("agent enrollment failed",
			"agent", hello.AgentID, "host", hello.Hostname, "err", err)
		_ = ws.Close(websocket.StatusPolicyViolation, "enrollment failed")
		return
	}

	ac := &agentConn{
		id: hello.AgentID,
		info: proto.AgentInfo{
			AgentID:     hello.AgentID,
			Hostname:    hello.Hostname,
			OS:          hello.OS,
			Arch:        hello.Arch,
			Kernel:      hello.Kernel,
			Version:     hello.Version,
			Connected:   true,
			ConnectedAt: time.Now().Unix(),
		},
		ws:     ws,
		sendCh: make(chan proto.Envelope, 64),
	}
	s.hub.addAgent(ac)
	defer s.hub.removeAgent(ac.id)

	// Send the welcome before anything else; if it carries an issued secret
	// the agent persists it before we ever do anything else with this conn.
	welcome, _ := json.Marshal(proto.Welcome{IssuedSecret: issued})
	if err := writeJSON(ctx, ws, proto.Envelope{Type: proto.TypeWelcome, Data: welcome}); err != nil {
		s.log.Warn("welcome write failed", "agent", ac.id, "err", err)
		return
	}

	if issued != "" {
		s.log.Info("agent enrolled (first contact)",
			"agent", ac.id, "host", hello.Hostname)
	} else {
		s.log.Info("agent connected", "agent", ac.id, "host", hello.Hostname)
	}

	// Writer goroutine. Returns when ctx is done or write fails.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-ac.sendCh:
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := writeJSON(wctx, ws, env)
				wcancel()
				if err != nil {
					_ = ws.Close(websocket.StatusInternalError, "write failed")
					return
				}
			}
		}
	}()

	// Reader loop.
	for {
		var env proto.Envelope
		if err := readJSON(ctx, ws, &env); err != nil {
			if !errors.Is(err, context.Canceled) {
				s.log.Info("agent disconnected", "agent", ac.id, "err", err)
			}
			return
		}
		s.dispatchAgentMessage(ac, env)
	}
}

// dispatchAgentMessage routes a single message from an agent.
func (s *Server) dispatchAgentMessage(ac *agentConn, env proto.Envelope) {
	switch env.Type {
	case proto.TypeMetrics:
		var m proto.Metrics
		if err := json.Unmarshal(env.Data, &m); err == nil {
			s.hub.onMetrics(ac.id, m)
		}
	case proto.TypeShellOutput:
		var o proto.ShellOutput
		if err := json.Unmarshal(env.Data, &o); err == nil {
			s.hub.broadcastPanels(proto.TypeAgentShellOutput, proto.AgentShellOutput{
				AgentID: ac.id, Output: o,
			})
		}
	case proto.TypeShellExit:
		var x proto.ShellExit
		if err := json.Unmarshal(env.Data, &x); err == nil {
			s.log.Info("shell exited",
				"agent", ac.id, "shell", x.ShellID,
				"exit", x.ExitCode, "reason", x.Reason)
			s.hub.broadcastPanels(proto.TypeAgentShellExit, proto.AgentShellExit{
				AgentID: ac.id, Exit: x,
			})
		}
	case proto.TypePong:
		// nothing to do for now
	default:
		s.log.Debug("unknown agent msg", "type", env.Type)
	}
}

// readHello reads exactly one envelope and decodes it as a Hello payload.
func readHello(ctx context.Context, ws *websocket.Conn) (*proto.Hello, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var env proto.Envelope
	if err := readJSON(rctx, ws, &env); err != nil {
		return nil, err
	}
	if env.Type != proto.TypeHello {
		return nil, errors.New("first message must be hello")
	}
	var h proto.Hello
	if err := json.Unmarshal(env.Data, &h); err != nil {
		return nil, err
	}
	if h.AgentID == "" {
		return nil, errors.New("agent_id is empty")
	}
	return &h, nil
}
