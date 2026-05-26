// Package proto describes the JSON message format used between agent, server,
// and the panel. Both directions on both sockets share the same envelope.
//
// Shell sessions are the only command-execution mechanism. A panel opens one
// or more shell sessions per agent, sends raw input bytes, receives raw
// output bytes, and resizes the PTY when the terminal in the browser resizes.
package proto

import "encoding/json"

// Envelope wraps every message exchanged over the WebSocket connections.
// `Type` selects which payload struct to unmarshal `Data` into.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// --- Agent -> Server ---

const (
	TypeHello       = "hello"        // agent announces itself
	TypeMetrics     = "metrics"      // periodic system metrics
	TypePong        = "pong"         // reply to ping
	TypeShellOutput = "shell_output" // raw PTY output (base64)
	TypeShellExit   = "shell_exit"   // shell process exited
)

// Hello is the first message an agent sends after connecting. The Secret
// field is empty on first contact (the server will then issue one) and
// must match the previously issued value on every subsequent connect.
type Hello struct {
	AgentID  string `json:"agent_id"`
	Secret   string `json:"secret,omitempty"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	Kernel   string `json:"kernel"`
}

// Metrics is sent by the agent on a fixed schedule.
type Metrics struct {
	TS         int64   `json:"ts"`           // unix seconds
	CPUPercent float64 `json:"cpu_percent"`  // 0..100
	MemPercent float64 `json:"mem_percent"`  // 0..100
	MemUsedMB  uint64  `json:"mem_used_mb"`
	MemTotalMB uint64  `json:"mem_total_mb"`
	DiskPct    float64 `json:"disk_percent"` // 0..100, root mount
	UptimeSec  uint64  `json:"uptime_sec"`
	Load1      float64 `json:"load1"`
	Load5      float64 `json:"load5"`
	Load15     float64 `json:"load15"`
}

// ShellOutput streams raw bytes coming out of a PTY back to the panel.
// `Data` is base64 to keep the JSON envelope clean.
type ShellOutput struct {
	ShellID string `json:"shell_id"`
	Data    string `json:"data"`
}

// ShellExit is emitted when the underlying shell process exits.
type ShellExit struct {
	ShellID  string `json:"shell_id"`
	ExitCode int    `json:"exit_code"`
	Reason   string `json:"reason,omitempty"`
}

// --- Server -> Agent ---

const (
	TypePing        = "ping"
	TypeWelcome     = "welcome"     // server confirms enrollment, may issue secret
	TypeShellOpen   = "shell_open"
	TypeShellInput  = "shell_input"  // raw input to PTY (base64)
	TypeShellResize = "shell_resize"
	TypeShellClose  = "shell_close"
)

// Welcome is the first message the server sends an agent after a successful
// hello. If `IssuedSecret` is non-empty, the agent must persist it and use
// it on every subsequent connection.
type Welcome struct {
	IssuedSecret string `json:"issued_secret,omitempty"`
}

// ShellOpen asks the agent to spawn a new PTY.
type ShellOpen struct {
	ShellID string `json:"shell_id"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// ShellInput delivers user keystrokes to the PTY.
type ShellInput struct {
	ShellID string `json:"shell_id"`
	Data    string `json:"data"`
}

// ShellResize updates the PTY winsize when the browser terminal resizes.
type ShellResize struct {
	ShellID string `json:"shell_id"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// ShellClose tells the agent to terminate the shell. Sent by the panel on
// tab close, or generated locally on agent disconnect.
type ShellClose struct {
	ShellID string `json:"shell_id"`
}

// --- Panel <-> Server ---

const (
	TypeAgentList         = "agent_list"
	TypeAgentConnected    = "agent_connected"
	TypeAgentDisconnected = "agent_disconnected"
	TypeAgentMetrics      = "agent_metrics"
	TypeAgentShellOutput  = "agent_shell_output"
	TypeAgentShellExit    = "agent_shell_exit"

	TypePanelShellOpen   = "panel_shell_open"
	TypePanelShellInput  = "panel_shell_input"
	TypePanelShellResize = "panel_shell_resize"
	TypePanelShellClose  = "panel_shell_close"
)

// AgentInfo describes an agent in messages going to the panel.
type AgentInfo struct {
	AgentID     string   `json:"agent_id"`
	Hostname    string   `json:"hostname"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Kernel      string   `json:"kernel"`
	Version     string   `json:"version"`
	Connected   bool     `json:"connected"`
	ConnectedAt int64    `json:"connected_at"`
	LastMetrics *Metrics `json:"last_metrics,omitempty"`
}

// AgentList is the initial snapshot pushed to a panel client.
type AgentList struct {
	Agents []AgentInfo `json:"agents"`
}

// AgentMetrics wraps a metrics payload with an agent id for the panel.
type AgentMetrics struct {
	AgentID string  `json:"agent_id"`
	Metrics Metrics `json:"metrics"`
}

// AgentShellOutput is shell output forwarded from agent to panel.
type AgentShellOutput struct {
	AgentID string      `json:"agent_id"`
	Output  ShellOutput `json:"output"`
}

// AgentShellExit notifies the panel that a shell ended.
type AgentShellExit struct {
	AgentID string    `json:"agent_id"`
	Exit    ShellExit `json:"exit"`
}

// PanelShellOpen / Input / Resize / Close are panel commands carrying an
// agent id so the server can route them to the right agent.
type PanelShellOpen struct {
	AgentID string `json:"agent_id"`
	ShellID string `json:"shell_id"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

type PanelShellInput struct {
	AgentID string `json:"agent_id"`
	ShellID string `json:"shell_id"`
	Data    string `json:"data"`
}

type PanelShellResize struct {
	AgentID string `json:"agent_id"`
	ShellID string `json:"shell_id"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

type PanelShellClose struct {
	AgentID string `json:"agent_id"`
	ShellID string `json:"shell_id"`
}
