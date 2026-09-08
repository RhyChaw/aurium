// Command aurium-mcp is the in-container stdio MCP shim (§4.1).
//
// It exists because MCP clients vary in their support for HTTP transports,
// but every one of them can spawn a stdio server. This binary is ~3 MB of
// static Go copied into each derived image; it speaks stdio MCP to the agent
// and forwards JSON-RPC to auriumd over HTTP with the container's token.
//
// Phase C fills in the protocol. This scaffolding establishes the transport
// and, importantly, the failure behaviour: an agent that cannot reach the
// daemon must be told so in a JSON-RPC error it can act on, not left waiting.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	cfg := loadConfig()
	if err := serve(os.Stdin, os.Stdout, cfg); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "aurium-mcp:", err)
		os.Exit(1)
	}
}

type config struct {
	URL   string
	Token string
}

// loadConfig reads the daemon URL and the container's bearer token.
//
// The token comes from a file by preference: a file can be replaced when the
// token is rotated, whereas an environment variable is fixed for the life of
// the process and is visible to anything else running in the container.
func loadConfig() config {
	c := config{URL: os.Getenv("AURIUM_URL")}
	if c.URL == "" {
		c.URL = "http://host.docker.internal:7770"
	}
	if body, err := os.ReadFile("/run/aurium/token"); err == nil {
		c.Token = strings.TrimSpace(string(body))
	}
	if c.Token == "" {
		c.Token = os.Getenv("AURIUM_TOKEN")
	}
	return c
}

// serve reads newline-delimited JSON-RPC from r and writes responses to w.
func serve(r io.Reader, w io.Writer, cfg config) error {
	scanner := bufio.NewScanner(r)
	// MCP tool results can be large; the default 64 KB token limit would
	// truncate them into malformed JSON.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	client := &http.Client{Timeout: 120 * time.Second}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		resp := forward(client, cfg, line)
		if resp == nil {
			continue // a notification has no reply
		}
		if _, err := w.Write(append(resp, '\n')); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// forward relays one JSON-RPC message to the daemon.
func forward(client *http.Client, cfg config, msg []byte) []byte {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(msg, &probe); err != nil {
		return rpcError(nil, -32700, "parse error: "+err.Error())
	}
	// A notification (no id) expects no response.
	isNotification := len(probe.ID) == 0 || string(probe.ID) == "null"

	req, err := http.NewRequest("POST", strings.TrimRight(cfg.URL, "/")+"/mcp", bytes.NewReader(msg))
	if err != nil {
		return rpcError(probe.ID, -32603, err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	res, err := client.Do(req)
	if err != nil {
		if isNotification {
			return nil
		}
		// Tell the agent plainly. Silence here would leave it waiting on a
		// reply that is never coming.
		return rpcError(probe.ID, -32603,
			"cannot reach the aurium daemon at "+cfg.URL+": "+err.Error())
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return rpcError(probe.ID, -32603, err.Error())
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return rpcError(probe.ID, -32603,
			"the aurium daemon refused this container's token (status "+res.Status+")")
	}
	if isNotification {
		return nil
	}
	return bytes.TrimSpace(body)
}

func rpcError(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	return body
}
