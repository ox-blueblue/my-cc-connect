//go:build linux

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const daemonPreemptTimeout = 5 * time.Second

type daemonRPCMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *daemonRPCError `json:"error,omitempty"`
	Method string          `json:"method,omitempty"`
}

type daemonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func preemptCodexThreadViaDaemon(codexHome, threadID string) error {
	socketPath := filepath.Join(resolveCodexHomeDir(codexHome), "app-server-control", "app-server-control.sock")
	info, err := os.Stat(socketPath)
	if err != nil {
		return fmt.Errorf("codex writer preemption: stat daemon control socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("codex writer preemption: daemon control path is not a socket")
	}

	dialer := websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	conn, _, err := dialer.DialContext(context.Background(), "ws://localhost/", nil)
	if err != nil {
		return fmt.Errorf("codex writer preemption: connect daemon control socket: %w", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(daemonPreemptTimeout))
	_ = conn.SetWriteDeadline(time.Now().Add(daemonPreemptTimeout))

	if err := daemonRPC(conn, 1, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "cc-connect-codex-preempt",
			"title":   "CC Connect Codex Writer Preempt",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return fmt.Errorf("codex writer preemption: daemon initialize: %w", err)
	}
	if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		return fmt.Errorf("codex writer preemption: daemon initialized: %w", err)
	}
	if err := daemonRPC(conn, 2, "thread/archive", map[string]string{"threadId": threadID}); err != nil {
		return fmt.Errorf("codex writer preemption: daemon archive thread: %w", err)
	}
	if err := daemonRPC(conn, 3, "thread/unarchive", map[string]string{"threadId": threadID}); err != nil {
		return fmt.Errorf("codex writer preemption: daemon unarchive thread: %w", err)
	}

	if current, err := inspectWriterLock(filepath.Join(resolveCodexHomeDir(codexHome), "thread-writer-locks", threadID+".lock")); err == nil {
		return fmt.Errorf("codex writer preemption: daemon still owns thread lock with pid %d", current.pid)
	} else if !strings.Contains(err.Error(), "no active owner") && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codex writer preemption: verify daemon thread lock release: %w", err)
	}
	return nil
}

func daemonRPC(conn *websocket.Conn, id int, method string, params any) error {
	if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		var msg daemonRPCMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.Method != "" || string(msg.ID) != fmt.Sprintf("%d", id) {
			continue
		}
		if msg.Error != nil {
			return fmt.Errorf("RPC %d: %s", msg.Error.Code, msg.Error.Message)
		}
		return nil
	}
}
