// Package protocol defines the newline-delimited JSON messages spoken between
// the local half and the remote half over the SSH channel (the remote's
// stdin/stdout).
//
// This is NOT the MCP protocol. The agent speaks MCP to the local half; the
// local half speaks this private protocol to the remote half.
package protocol

// APC framing (recording-friendly mode). When enabled, each Response is wrapped
// in a terminal Application Program Command string: ESC '_' <payload> ESC '\'.
// Terminal emulators — including xterm.js, which Teleport's session player uses
// — parse and silently discard APC strings, so the wrapped protocol is
// invisible on playback while a human-readable transcript emitted alongside it
// is shown. The payload is APCTag + <nonce> + <json>, where <nonce> is a
// per-session secret (hex) that lets the local half reject spoofed APC strings
// injected by command output. Because encoding/json escapes all control bytes
// (including ESC as \u001b), a marshaled Response never contains a raw ESC and
// so can never prematurely terminate the APC envelope.
const (
	APCStart = "\x1b_"  // Application Program Command - introducer
	APCEnd   = "\x1b\\" // String Terminator
	APCTag   = "MH"     // payload prefix, followed by <nonce><json>
)

// Request is sent from the local half to the remote.
//
//   type = "exec"             : run a command in the persistent shell
//   type = "allowed_commands" : ask the remote for the allowlist it enforces
//   type = "shutdown"          : close the remote cleanly
//
// There is no "configure" request: the allowlist is read by the remote from
// its own config file (see package remoteconfig) and is immutable for the life
// of the executor. The local half cannot set or change it.
type Request struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Command   string `json:"command,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	TimeoutMs int    `json:"timeoutMs,omitempty"`
	PTY       bool   `json:"pty,omitempty"`
}

// Response is sent from the remote to the local half.
//
//   type = "result"    : a command finished (see ExitCode/Stdout/Stderr/CWD)
//   type = "allowlist" : reply to "allowed_commands" (see AllowCommands)
//   type = "log"       : informational (ready/bye)
//   type = "error"     : the request could not be honoured (see Error)
type Response struct {
	ID   int64  `json:"id,omitempty"`
	Type string `json:"type"`

	// result fields
	ExitCode   int    `json:"exitCode,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	CWD        string `json:"cwd,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	TimedOut   bool   `json:"timedOut,omitempty"`

	// allowlist fields (type = "allowlist")
	AllowCommands [][]string `json:"allowCommands,omitempty"`

	// log fields
	Msg        string `json:"msg,omitempty"`
	Version    string `json:"version,omitempty"` // reported in "ready"
	PID        int    `json:"pid,omitempty"`     // reported in "ready"
	HasTimeout bool   `json:"hasTimeout,omitempty"` // reported in "ready": timeout(1) available?

	// error fields
	Error string `json:"error,omitempty"`
}
