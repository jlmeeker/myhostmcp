package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"myhostmcp/internal/protocol"
)

// harness drives a remote.Executor in-process over io.Pipes, so the executor
// can be exercised (persistent shell, sentinel framing, allowlist, timeouts)
// without SSH. The allowlist comes from a temp config file that the executor
// loads itself in New — the same code path production uses.
type harness struct {
	t    *testing.T
	e    *Executor
	in   io.WriteCloser
	out  *bufio.Scanner
	done chan error
}

func newHarness(t *testing.T, configPath string) *harness {
	t.Helper()
	pr, pw := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	e, err := New(Config{ConfigPath: configPath}, pr, outW, &stderr)
	if err != nil {
		t.Fatalf("New: %v\nstderr: %s", err, stderr.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	h := &harness{
		t:    t,
		e:    e,
		in:   pw,
		out:  bufio.NewScanner(outR),
		done: done,
	}
	h.out.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	t.Cleanup(func() { pw.Close(); <-done })
	// Consume the ready announcement.
	ready := h.recv(0)
	if ready.Type != "log" || ready.Msg != "ready" {
		t.Fatalf("ready: %+v", ready)
	}
	return h
}

func (h *harness) send(req protocol.Request) {
	b, _ := json.Marshal(req)
	b = append(b, '\n')
	if _, err := h.in.Write(b); err != nil {
		h.t.Fatalf("write: %v", err)
	}
}

// recv returns the response whose ID matches, skipping log/error lines.
func (h *harness) recv(id int64) protocol.Response {
	for h.out.Scan() {
		var r protocol.Response
		if err := json.Unmarshal(h.out.Bytes(), &r); err != nil {
			h.t.Fatalf("decode: %v line=%q", err, h.out.Text())
		}
		if r.ID == id {
			return r
		}
	}
	h.t.Fatalf("no response for id %d", id)
	return protocol.Response{}
}

// writeConfig writes a remote config YAML with the given allowlist and returns
// its path. t.Cleanup removes it. Each entry is written as a single
// whitespace-joined string (the on-disk format the remote expects); Load
// splits it back into tokens.
func writeConfig(t *testing.T, allow [][]string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	type cfg struct {
		AllowCommands []string `yaml:"allowCommands"`
	}
	raws := make([]string, 0, len(allow))
	for _, toks := range allow {
		raws = append(raws, strings.Join(toks, " "))
	}
	b, err := yaml.Marshal(cfg{AllowCommands: raws})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestExecutorEndToEnd drives the executor over pipes with a permissive temp
// config and checks persistent cwd, separated stdout/stderr, timeout, and a
// failing command not killing the session.
func TestExecutorEndToEnd(t *testing.T) {
	allow := [][]string{
		{"pwd"}, {"cd"}, {"echo"}, {"ls"},
		{"for"}, {"do"}, {"done"},
		{"sleep"}, {"nonexistent-command-xyz"},
	}
	h := newHarness(t, writeConfig(t, allow))

	// allowed_commands query returns exactly the configured list.
	h.send(protocol.Request{ID: 1, Type: "allowed_commands"})
	al := h.recv(1)
	if al.Type != "allowlist" {
		t.Fatalf("allowlist response: %+v", al)
	}
	if len(al.AllowCommands) != len(allow) {
		t.Fatalf("allowlist len = %d, want %d", len(al.AllowCommands), len(allow))
	}

	// pwd
	h.send(protocol.Request{ID: 2, Type: "exec", Command: "pwd"})
	r := h.recv(2)
	if r.Type != "result" || r.ExitCode != 0 {
		t.Fatalf("pwd: %+v", r)
	}

	// cd /tmp persists across calls.
	h.send(protocol.Request{ID: 3, Type: "exec", Command: "cd /tmp"})
	h.recv(3)
	h.send(protocol.Request{ID: 4, Type: "exec", Command: "pwd"})
	r = h.recv(4)
	if got := strings.TrimSpace(r.Stdout); got != "/tmp" {
		t.Fatalf("after cd /tmp, pwd stdout = %q, want /tmp", got)
	}
	if got := strings.TrimSpace(r.CWD); got != "/tmp" {
		t.Fatalf("cwd = %q, want /tmp", got)
	}

	// Separated stdout/stderr without using redirection (which the allowlist
	// blocks): ls on a missing path writes its error to stderr naturally.
	h.send(protocol.Request{ID: 5, Type: "exec", Command: "echo hello; ls /no/such/dir"})
	r = h.recv(5)
	if !strings.Contains(r.Stdout, "hello") {
		t.Fatalf("stdout missing \"hello\": %q", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "ls:") && !strings.Contains(r.Stderr, "No such") {
		t.Fatalf("stderr missing ls error: %q", r.Stderr)
	}

	// Timeout: sleep 10 with a 500ms deadline must time out.
	h.send(protocol.Request{ID: 6, Type: "exec", Command: "sleep 10", TimeoutMs: 500})
	r = h.recv(6)
	if !r.TimedOut {
		t.Fatalf("sleep 10 should time out: %+v", r)
	}

	// A failing command does not kill the session; the next command still runs.
	h.send(protocol.Request{ID: 7, Type: "exec", Command: "nonexistent-command-xyz; echo after-fail"})
	r = h.recv(7)
	if !strings.Contains(r.Stdout, "after-fail") {
		t.Fatalf("after-fail missing from stdout: %q", r.Stdout)
	}

	// Shutdown.
	h.send(protocol.Request{ID: 99, Type: "shutdown"})
	if got := h.recv(99).Msg; got != "bye" {
		t.Fatalf("shutdown: want \"bye\", got %q", got)
	}
}

// TestExecutorAllowlistRejection verifies the executor enforces its loaded
// allowlist: a disallowed command is rejected with an "allowlist" error, while
// an allowed one runs.
func TestExecutorAllowlistRejection(t *testing.T) {
	allow := [][]string{{"df"}, {"echo"}}
	h := newHarness(t, writeConfig(t, allow))

	// Allowed: echo.
	h.send(protocol.Request{ID: 1, Type: "exec", Command: "echo hi"})
	r := h.recv(1)
	if r.Type != "result" || r.ExitCode != 0 {
		t.Fatalf("echo should be allowed: %+v", r)
	}
	if got := strings.TrimSpace(r.Stdout); got != "hi" {
		t.Fatalf("echo stdout = %q, want \"hi\"", got)
	}

	// Disallowed: rm.
	h.send(protocol.Request{ID: 2, Type: "exec", Command: "rm -rf /"})
	r = h.recv(2)
	if r.Type != "error" || !strings.Contains(r.Error, "allowlist") {
		t.Fatalf("rm should be rejected by allowlist: %+v", r)
	}

	// Disallowed construct: redirection (even of an allowed command).
	h.send(protocol.Request{ID: 3, Type: "exec", Command: "echo hi > /tmp/x"})
	r = h.recv(3)
	if r.Type != "error" || !strings.Contains(r.Error, "allowlist") {
		t.Fatalf("redirect should be rejected by allowlist: %+v", r)
	}
}

// TestExecutorDefaultAllowlist confirms that a missing config file yields the
// built-in safe default allowlist (df, ss, top, free, ls), and that the remote
// is therefore never unrestricted.
func TestExecutorDefaultAllowlist(t *testing.T) {
	// Point at a path that does not exist.
	h := newHarness(t, "/no/such/myhostmcp-config.yaml")

	h.send(protocol.Request{ID: 1, Type: "allowed_commands"})
	al := h.recv(1)
	if al.Type != "allowlist" {
		t.Fatalf("allowlist response: %+v", al)
	}
	want := [][]string{{"df"}, {"ss"}, {"top"}, {"free"}, {"ls"}}
	if len(al.AllowCommands) != len(want) {
		t.Fatalf("default allowlist len = %d, want %d (%+v)", len(al.AllowCommands), len(want), al.AllowCommands)
	}
	// Allowed: df. Disallowed: rm (proves the default is enforced, not empty).
	h.send(protocol.Request{ID: 2, Type: "exec", Command: "df -h /"})
	r := h.recv(2)
	if r.Type != "result" {
		t.Fatalf("df should be allowed by default: %+v", r)
	}
	h.send(protocol.Request{ID: 3, Type: "exec", Command: "rm -rf /"})
	r = h.recv(3)
	if r.Type != "error" || !strings.Contains(r.Error, "allowlist") {
		t.Fatalf("rm should be rejected by default allowlist: %+v", r)
	}
}

// TestExecutorMalformedConfigErrors confirms a malformed remote config is a
// hard error (the remote refuses to start rather than silently falling back).
func TestExecutorMalformedConfigErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("allowCommands: [this is : not valid yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr, _ := io.Pipe()
	defer pr.Close()
	outR, outW := io.Pipe()
	defer outR.Close()
	defer outW.Close()
	var stderr bytes.Buffer
	if _, err := New(Config{ConfigPath: p}, pr, outW, &stderr); err == nil {
		t.Fatalf("New with malformed config should error; stderr=%s", stderr.String())
	}
}
