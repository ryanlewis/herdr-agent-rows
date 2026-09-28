package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// herdr's socket API: newline-delimited JSON over a unix socket, one request
// per connection (the server closes after answering).

type agentInfo struct {
	PaneID                string            `json:"pane_id"`
	WorkspaceID           string            `json:"workspace_id"`
	TerminalID            string            `json:"terminal_id"`
	AgentStatus           string            `json:"agent_status"`
	Cwd                   string            `json:"cwd"`
	ForegroundCwd         string            `json:"foreground_cwd"`
	Name                  string            `json:"name"`
	TerminalTitleStripped string            `json:"terminal_title_stripped"`
	Tokens                map[string]string `json:"tokens"`
}

type paneInfo struct {
	PaneID string `json:"pane_id"`
	Label  string `json:"label"`
}

type workspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

// Pointers so a missing list reads as nil, not as an empty one.
type snapshot struct {
	Agents     *[]agentInfo     `json:"agents"`
	Workspaces *[]workspaceInfo `json:"workspaces"`
	Panes      *[]paneInfo      `json:"panes"`
}

// socketPath follows herdr's own resolution: HERDR_SOCKET_PATH (set for
// plugin commands), else the config dir's socket for HERDR_SESSION or the
// default session.
func socketPath() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	dir = filepath.Join(dir, "herdr")
	if s := os.Getenv("HERDR_SESSION"); s != "" {
		dir = filepath.Join(dir, "sessions", s)
	}
	return filepath.Join(dir, "herdr.sock")
}

func call(sock, method string, params any, out any) error {
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req, err := json.Marshal(map[string]any{"id": "agent-rows", "method": method, "params": params})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return fmt.Errorf("%s: %w", method, err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("%s: bad response: %w", method, err)
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: %s %s", method, resp.Error.Code, resp.Error.Message)
	}
	if out == nil {
		return nil
	}
	if len(resp.Result) == 0 {
		return errors.New(method + ": empty result")
	}
	return json.Unmarshal(resp.Result, out)
}

// One request returns agents (with tokens), workspaces and panes.
func sessionSnapshot(sock string) (*snapshot, error) {
	var res struct {
		Snapshot snapshot `json:"snapshot"`
	}
	if err := call(sock, "session.snapshot", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return &res.Snapshot, nil
}

// tokens: value to set, nil to clear.
func reportTokens(sock, paneID string, tokens map[string]*string) error {
	return call(sock, "pane.report_metadata", map[string]any{
		"pane_id": paneID,
		"source":  metadataSource,
		"tokens":  tokens,
	}, nil)
}
