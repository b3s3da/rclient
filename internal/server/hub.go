// Package server implements the central server that agents connect to and
// the panel UI talks to.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"rclient/internal/proto"
)

// agentConn represents a single connected agent.
type agentConn struct {
	id          string
	info        proto.AgentInfo
	ws          *websocket.Conn
	sendCh      chan proto.Envelope
	lastMetrics *proto.Metrics
}

// panelConn represents a connected panel UI client.
type panelConn struct {
	id     string
	ws     *websocket.Conn
	sendCh chan proto.Envelope
}

// Hub is the central registry. All access is serialized through its mutex.
type Hub struct {
	mu     sync.RWMutex
	agents map[string]*agentConn // by agent id
	panels map[string]*panelConn // by panel session id
	log    *slog.Logger
}

func NewHub(log *slog.Logger) *Hub {
	return &Hub{
		agents: map[string]*agentConn{},
		panels: map[string]*panelConn{},
		log:    log,
	}
}

// snapshot returns a copy of all agents currently known.
func (h *Hub) snapshot() []proto.AgentInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]proto.AgentInfo, 0, len(h.agents))
	for _, a := range h.agents {
		info := a.info
		info.LastMetrics = a.lastMetrics
		out = append(out, info)
	}
	return out
}

// addAgent registers an agent. If one with the same id is already connected,
// the previous connection is closed.
func (h *Hub) addAgent(a *agentConn) {
	h.mu.Lock()
	if old, ok := h.agents[a.id]; ok {
		_ = old.ws.Close(websocket.StatusPolicyViolation, "replaced")
	}
	h.agents[a.id] = a
	h.mu.Unlock()
	h.broadcastPanels(proto.TypeAgentConnected, a.info)
}

func (h *Hub) removeAgent(id string) {
	h.mu.Lock()
	a, ok := h.agents[id]
	if ok {
		delete(h.agents, id)
	}
	h.mu.Unlock()
	if ok {
		h.broadcastPanels(proto.TypeAgentDisconnected, a.info)
	}
}

// sendToAgent enqueues a message for a specific agent. Drops on full buffer.
func (h *Hub) sendToAgent(id string, env proto.Envelope) bool {
	h.mu.RLock()
	a, ok := h.agents[id]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case a.sendCh <- env:
		return true
	default:
		h.log.Warn("agent send buffer full", "agent", id)
		return false
	}
}

func (h *Hub) addPanel(p *panelConn) {
	h.mu.Lock()
	h.panels[p.id] = p
	h.mu.Unlock()
}

func (h *Hub) removePanel(id string) {
	h.mu.Lock()
	delete(h.panels, id)
	h.mu.Unlock()
}

// broadcastPanels sends the same payload to every connected panel client.
func (h *Hub) broadcastPanels(typ string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		h.log.Error("marshal panel payload", "err", err)
		return
	}
	env := proto.Envelope{Type: typ, Data: raw}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, p := range h.panels {
		select {
		case p.sendCh <- env:
		default:
			// Panel can't keep up, drop the message rather than block the hub.
			h.log.Warn("panel send buffer full", "panel", p.id)
		}
	}
}

// onMetrics is called when an agent reports new metrics.
func (h *Hub) onMetrics(agentID string, m proto.Metrics) {
	h.mu.Lock()
	a, ok := h.agents[agentID]
	if ok {
		mc := m
		a.lastMetrics = &mc
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	h.broadcastPanels(proto.TypeAgentMetrics, proto.AgentMetrics{
		AgentID: agentID,
		Metrics: m,
	})
}

// pingLoop periodically sends pings to all agents to keep NAT mappings warm
// and detect dead peers.
func (h *Hub) pingLoop(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.mu.RLock()
			ids := make([]string, 0, len(h.agents))
			for id := range h.agents {
				ids = append(ids, id)
			}
			h.mu.RUnlock()
			for _, id := range ids {
				h.sendToAgent(id, proto.Envelope{Type: proto.TypePing})
			}
		}
	}
}
