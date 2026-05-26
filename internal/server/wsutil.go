package server

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"
)

// readJSON reads one text frame and unmarshals it into v.
func readJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// writeJSON marshals v and writes it as a single text frame.
func writeJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, data)
}
