package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"myhostmcp/internal/config"
)

// canSSHSystem returns true if `ssh 127.0.0.1` (system sshd on :22) works with
// the user's default key. The integration tests rely on this.
func canSSHSystem() bool {
	c := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=5", "127.0.0.1", "true")
	return c.Run() == nil
}

func skipIfNoSSH(t *testing.T) {
	t.Helper()
	if !canSSHSystem() {
		t.Skip("system sshd on 127.0.0.1:22 not reachable with default key; skipping integration test")
	}
}

// connectMCP wires a Manager's server to an in-memory MCP client and returns
// the client session.
func connectMCP(t *testing.T, mgr *Manager) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := mgr.buildServer()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	return tc.Text
}

func structField(t *testing.T, res *mcp.CallToolResult, key string) any {
	t.Helper()
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent not a map: %T", res.StructuredContent)
	}
	return m[key]
}

// TestIntegrationEndToEnd connects the local half to the system sshd on
// 127.0.0.1:22 and exercises connect → (query + exec) → status → disconnect
// through the MCP tools. The remote enforces its own allowlist
// (/etc/myhostmcp/config.yaml, defaulting to df/ss/top/free/ls), so this test
// is robust to the host's configuration: it queries the allowlist, confirms a
// disallowed command is rejected by the remote, and verifies the session/state
// lifecycle.
func TestIntegrationEndToEnd(t *testing.T) {
	skipIfNoSSH(t)

	cfg := &config.Config{
		DefaultPort:      22,
		RemoteInstallDir: "~/.myhostmcp-itest",
		ConnectTimeout:   config.Duration(15 * time.Second),
		ExecTimeout:      config.Duration(30 * time.Second),
	}
	mgr, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cs := connectMCP(t, mgr)

	// 1. remote_connect (the result includes the remote's allowlist).
	res := callTool(t, cs, "remote_connect", map[string]any{
		"host": "127.0.0.1",
	})
	if res.IsError {
		t.Fatalf("remote_connect failed: %s", textOf(t, res))
	}
	sessName, _ := structField(t, res, "session").(string)
	if sessName == "" {
		t.Fatal("no session name in connect result")
	}
	allow, _ := structField(t, res, "allowCommands").([]any)
	if len(allow) == 0 {
		t.Fatalf("connect result should include a non-empty allowlist")
	}
	t.Logf("connected session=%q platform=%v pid=%v allow=%d",
		sessName, structField(t, res, "platform"), structField(t, res, "remotePid"), len(allow))

	// 2. remote_allowed_commands: query the remote's enforced allowlist.
	res = callTool(t, cs, "remote_allowed_commands", map[string]any{})
	if res.IsError {
		t.Fatalf("remote_allowed_commands failed: %s", textOf(t, res))
	}
	allow2, _ := structField(t, res, "allowCommands").([]any)
	if len(allow2) != len(allow) {
		t.Fatalf("allowed_commands count %d != connect-embedded %d", len(allow2), len(allow))
	}

	// 3. remote_exec: a command that is certainly not allowed is rejected by
	//    the remote (the remote has the final say). The error must come from
	//    the remote and mention the allowlist.
	res = callTool(t, cs, "remote_exec", map[string]any{"command": "rm -rf /"})
	if !res.IsError {
		t.Fatalf("rm -rf / should be rejected by the remote allowlist")
	}
	if txt := textOf(t, res); !strings.Contains(txt, "allowlist") {
		t.Fatalf("rejection should mention allowlist: %s", txt)
	}

	// 4. remote_status
	res = callTool(t, cs, "remote_status", map[string]any{})
	if res.IsError {
		t.Fatalf("status failed: %s", textOf(t, res))
	}
	sessions, _ := structField(t, res, "sessions").([]any)
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	// 5. remote_disconnect
	res = callTool(t, cs, "remote_disconnect", map[string]any{"session": sessName})
	if res.IsError {
		t.Fatalf("disconnect failed: %s", textOf(t, res))
	}
	disc, _ := structField(t, res, "disconnected").([]any)
	if len(disc) != 1 {
		t.Fatalf("expected 1 disconnected, got %d", len(disc))
	}

	// 6. remote_exec after disconnect -> error
	res = callTool(t, cs, "remote_exec", map[string]any{"command": "pwd"})
	if !res.IsError {
		t.Fatalf("exec after disconnect should be an error, got: %s", textOf(t, res))
	}
}

// TestIntegrationRemoteAllowlist verifies that the allowlist is enforced on the
// REMOTE host (not the local half): the local half forwards exec, and the
// remote rejects disallowed commands while allowing ones in its list. There is
// no longer a local allowlist to configure.
func TestIntegrationRemoteAllowlist(t *testing.T) {
	skipIfNoSSH(t)

	cfg := &config.Config{
		DefaultPort:      22,
		RemoteInstallDir: "~/.myhostmcp-itest",
		ConnectTimeout:   config.Duration(15 * time.Second),
		ExecTimeout:      config.Duration(30 * time.Second),
	}
	mgr, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cs := connectMCP(t, mgr)

	res := callTool(t, cs, "remote_connect", map[string]any{"host": "127.0.0.1"})
	if res.IsError {
		t.Fatalf("connect: %s", textOf(t, res))
	}

	// Query the remote's allowlist and pick a safe allowed command.
	res = callTool(t, cs, "remote_allowed_commands", map[string]any{})
	if res.IsError {
		t.Fatalf("allowed_commands: %s", textOf(t, res))
	}
	allowAny, _ := structField(t, res, "allowCommands").([]any)
	if len(allowAny) == 0 {
		t.Fatalf("remote allowlist should be non-empty")
	}
	cmd := safeAllowedCommandFromStruct(allowAny)
	if cmd == "" {
		t.Skipf("no safe allowed command in the remote allowlist %v; skipping positive check", allowAny)
	}

	// An allowed command runs (no IsError from the allowlist).
	res = callTool(t, cs, "remote_exec", map[string]any{"command": cmd, "timeout": "5s"})
	if res.IsError {
		t.Fatalf("allowed command %q should run, got: %s", cmd, textOf(t, res))
	}

	// A disallowed command is rejected by the remote.
	res = callTool(t, cs, "remote_exec", map[string]any{"command": "rm -rf /"})
	if !res.IsError {
		t.Fatalf("rm -rf / should be rejected by the remote allowlist")
	}
	if txt := textOf(t, res); !strings.Contains(txt, "allowlist") {
		t.Fatalf("rejection should mention allowlist: %s", txt)
	}
}

// safeAllowedCommandFromStruct picks a harmless invocation from an allowlist
// represented as unstructured JSON ([][]string came back as [][]any).
func safeAllowedCommandFromStruct(allow []any) string {
	safe := map[string]string{
		"df":    "df -h /",
		"free":  "free",
		"ls":    "ls /",
		"ss":    "ss",
		"ps":    "ps -e",
		"echo":  "echo ok",
		"pwd":   "pwd",
		"uname": "uname -s",
	}
	for _, entry := range allow {
		toks, ok := entry.([]any)
		if !ok || len(toks) == 0 {
			continue
		}
		first, _ := toks[0].(string)
		if cmd, ok := safe[first]; ok {
			return cmd
		}
	}
	return ""
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// (removed: self-contained sshd helpers — these tests now use the system sshd
// on 127.0.0.1:22 with the user's default SSH key, per the project setup.)

// keep fmt import used in earlier helpers guarded.
var _ = fmt.Sprintf
