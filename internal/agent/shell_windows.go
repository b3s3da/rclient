//go:build windows

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"rclient/internal/proto"
)

// On Windows we don't ship a real PTY backend. The agent still compiles so
// that we can develop on Windows hosts; it just refuses to open shells.

type shellSession struct{}
type shellRegistry struct{ mu sync.Mutex }

func newShellRegistry() *shellRegistry { return &shellRegistry{} }
func (r *shellRegistry) closeAll()     {}

func (a *Agent) openShell(ctx context.Context, open proto.ShellOpen) {
	raw, _ := json.Marshal(proto.ShellExit{
		ShellID:  open.ShellID,
		ExitCode: -1,
		Reason:   errors.New("shell is not supported on windows agents").Error(),
	})
	a.send(ctx, proto.Envelope{Type: proto.TypeShellExit, Data: raw})
}
func (a *Agent) shellInput(proto.ShellInput)   {}
func (a *Agent) shellResize(proto.ShellResize) {}
func (a *Agent) shellClose(proto.ShellClose)   {}
