// Package local implements the local half of myhostmcp: a stdio MCP server
// that an AI agent spawns. It exposes five tools (remote_connect, remote_exec,
// remote_allowed_commands, remote_status, remote_disconnect) and manages one
// or more open sessions to remote hosts, forwarding commands to the remote
// half running there.
//
// Connections use ssh by default. When tsh (Teleport) is available it is tried
// first; if the user is not yet logged in, tsh login is run automatically
// (browser-based SSO opens on the user's desktop). If tsh fails for any
// reason the connection falls back to ssh transparently.
//
// The local half does NO work on startup: it only registers tools. Connections
// are opened lazily on the first remote_connect (or remote_exec against a
// configured default host). Diagnostics go to a log file (or stderr), never
// stdout — stdout is the MCP transport.
package local

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"myhostmcp/internal/config"
	"myhostmcp/internal/protocol"
	"myhostmcp/internal/transport"
	"myhostmcp/internal/version"
)

// Manager holds the local half's configuration, logger, and open sessions.
type Manager struct {
	cfg *config.Config
	log *log.Logger

	mu       sync.Mutex
	sessions map[string]*transport.Session
	order    []string // session names in connection order (last = default)
	default_ string   // explicit default; if "", last in order
}

// NewManager creates a manager from a loaded config.
func NewManager(cfg *config.Config) (*Manager, error) {
	logger, err := openLogger(cfg.LogFile)
	if err != nil {
		return nil, err
	}
	logger.Printf("myhostmcp local starting (version %s)", version.Version)
	return &Manager{
		cfg:      cfg,
		log:      logger,
		sessions: map[string]*transport.Session{},
	}, nil
}

// Run starts the stdio MCP server and blocks until the agent disconnects or
// ctx is cancelled. It closes all sessions on return.
func (m *Manager) Run(ctx context.Context) error {
	defer m.closeAll()
	srv := m.buildServer()
	m.log.Printf("MCP server running over stdio")
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// buildServer constructs the MCP server with the five remote_* tools. It is
// also used by tests to attach an in-memory transport instead of stdio.
func (m *Manager) buildServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "myhostmcp", Version: version.Version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_connect",
		Description: "Open (or reuse) a persistent session to a remote host and start the remote myhostmcp executor there. Uses tsh (Teleport) when available, automatically running tsh login if needed (browser-based SSO will open a browser on your desktop); falls back to ssh if tsh is unavailable or fails. The session stays open until remote_disconnect or the agent session ends. No connection activity happens until this is called (or remote_exec with a default host).",
	}, m.handleConnect)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_exec",
		Description: "Run a shell command in the persistent remote shell of an open session. cd/export/aliases persist across calls. Returns captured stdout, stderr, exit code, and the current working directory. Subject to the remote host's command allowlist (query it with remote_allowed_commands); the remote has the final say.",
	}, m.handleExec)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_allowed_commands",
		Description: "Query the remote host for the command allowlist it enforces. The allowlist is configured on the remote host (default: /etc/myhostmcp/config.yaml); the remote always has the final say over what remote_exec may run. Use this to learn which commands are available before running them.",
	}, m.handleAllowedCommands)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_status",
		Description: "Report open remote sessions and which one is the default. (Per-host command allowlists are reported by remote_allowed_commands.)",
	}, m.handleStatus)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_disconnect",
		Description: "Close one remote session (by name), the default session (if name omitted), or all sessions (name \"*\"). Releases the remote executor.",
	}, m.handleDisconnect)

	return srv
}

// ----- tool input/output types -------------------------------------------------

// ConnectInput is the argument to remote_connect.
type ConnectInput struct {
	Host            string `json:"host,omitempty" jsonschema:"SSH/Teleport host to connect to, as resolvable by your ~/.ssh/config or Teleport cluster. If omitted, uses defaultHost from the local config."`
	User            string `json:"user,omitempty" jsonschema:"remote OS login user; if omitted, ssh's/tsh's default for the host"`
	Port            int    `json:"port,omitempty" jsonschema:"SSH port; defaults to 22 or config"`
	IdentityFile    string `json:"identityFile,omitempty" jsonschema:"optional path to a private key, overriding config and ssh defaults"`
	Session         string `json:"session,omitempty" jsonschema:"optional friendly name for this session; auto-named from the host if omitted"`
	Transport       string `json:"transport,omitempty" jsonschema:"transport override: \"auto\" (default; try tsh then ssh), \"ssh\" (always use ssh), or \"tsh\" (always use tsh, no fallback)"`
	TeleportProxy   string `json:"teleportProxy,omitempty" jsonschema:"Teleport proxy address for tsh login, e.g. proxy.example.com:443; if omitted tsh uses its configured default"`
	TeleportCluster string `json:"teleportCluster,omitempty" jsonschema:"optional Teleport cluster or leaf-cluster name passed to tsh login; if omitted the proxy default is used"`
}

// ConnectOutput is the structured result of remote_connect.
type ConnectOutput struct {
	Session          string     `json:"session"`
	Host             string     `json:"host"`
	User             string     `json:"user,omitempty"`
	Port             int        `json:"port,omitempty"`
	Platform         string     `json:"platform,omitempty"`
	RemotePID        int        `json:"remotePid"`
	RemoteVersion    string     `json:"remoteVersion"`
	HasTimeout       bool       `json:"hasTimeout"`
	AllowCommands    [][]string `json:"allowCommands,omitempty"` // the remote's enforced allowlist
	AlreadyConnected bool       `json:"alreadyConnected,omitempty"`
	Transport        string     `json:"transport,omitempty"`    // "ssh" or "tsh"
	LoginNote        string     `json:"loginNote,omitempty"`   // non-empty when tsh login was performed
	FallbackNote     string     `json:"fallbackNote,omitempty"` // non-empty when tsh failed and ssh was used
}

// ExecInput is the argument to remote_exec.
type ExecInput struct {
	Command string `json:"command" jsonschema:"the shell command to run in the persistent remote shell"`
	Session string `json:"session,omitempty" jsonschema:"session to run in; defaults to the most recently connected session"`
	CWD     string `json:"cwd,omitempty" jsonschema:"optional working directory to cd into before running the command"`
	Timeout string `json:"timeout,omitempty" jsonschema:"per-command timeout as a Go duration string, e.g. \"30s\" or \"2m\"; defaults to the config execTimeout"`
	PTY     bool   `json:"pty,omitempty" jsonschema:"run the command in a pseudo-terminal (for sudo/TUIs). Not yet implemented in this build."`
}

// ExecOutput is the structured result of remote_exec.
type ExecOutput struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	CWD        string `json:"cwd"`
	DurationMs int64  `json:"durationMs"`
	TimedOut   bool   `json:"timedOut"`
	Session    string `json:"session"`
}

// StatusOutput is the structured result of remote_status.
type StatusOutput struct {
	Default  string        `json:"default,omitempty"`
	Sessions []SessionInfo `json:"sessions"`
	Version  string        `json:"version"`
}

// SessionInfo describes one open session.
type SessionInfo struct {
	Name          string `json:"name"`
	Host          string `json:"host"`
	User          string `json:"user,omitempty"`
	Port          int    `json:"port,omitempty"`
	Platform      string `json:"platform,omitempty"`
	RemotePID     int    `json:"remotePid"`
	RemoteVersion string `json:"remoteVersion"`
	HasTimeout    bool   `json:"hasTimeout"`
	Transport     string `json:"transport,omitempty"` // "ssh" or "tsh"
}

// DisconnectInput is the argument to remote_disconnect.
type DisconnectInput struct {
	Session string `json:"session,omitempty" jsonschema:"session name to disconnect; if omitted, disconnects the default session; if \"*\", disconnects all sessions"`
}

// DisconnectOutput is the structured result of remote_disconnect.
type DisconnectOutput struct {
	Disconnected []string `json:"disconnected"`
}

// AllowedCommandsInput is the argument to remote_allowed_commands.
type AllowedCommandsInput struct {
	Session string `json:"session,omitempty" jsonschema:"session to query; defaults to the most recently connected session"`
}

// AllowedCommandsOutput is the structured result of remote_allowed_commands.
type AllowedCommandsOutput struct {
	Session       string     `json:"session"`
	AllowCommands [][]string `json:"allowCommands"`
}

// ----- handlers ----------------------------------------------------------------

func (m *Manager) handleConnect(ctx context.Context, _ *mcp.CallToolRequest, in ConnectInput) (*mcp.CallToolResult, ConnectOutput, error) {
	host := in.Host
	if host == "" {
		host = m.cfg.DefaultHost
	}
	if host == "" {
		return m.errResult("no host specified and no defaultHost configured", ConnectOutput{}), ConnectOutput{}, nil
	}
	user := in.User
	if user == "" {
		user = m.cfg.DefaultUser
	}
	port := in.Port
	if port == 0 {
		port = m.cfg.DefaultPort
	}
	ids := append([]string{}, m.cfg.IdentityFiles...)
	if in.IdentityFile != "" {
		ids = append(ids, in.IdentityFile)
	}

	name := in.Session
	if name == "" {
		name = hostName(host)
	}

	// Reuse an existing session with this name.
	m.mu.Lock()
	existing, ok := m.sessions[name]
	m.mu.Unlock()
	if ok {
		allow, qerr := m.queryAllowed(existing)
		if qerr != nil {
			m.log.Printf("session %q: query allowed_commands failed: %v", name, qerr)
		}
		out := ConnectOutput{
			Session: name, Host: existing.Host, User: existing.User, Port: existing.Port,
			Platform: existing.Platform, RemotePID: existing.RemotePID,
			RemoteVersion: existing.RemoteVersion, HasTimeout: existing.HasTimeout,
			AlreadyConnected: true,
			AllowCommands:    allow,
			Transport:        existing.Transport,
		}
		return m.okResult(fmt.Sprintf("Reusing existing session %q to %s via %s (remote pid %d). %d allowed command(s).",
			name, existing.Host, existing.Transport, existing.RemotePID, len(allow)), out), out, nil
	}

	// Transport preference: per-call input overrides config default.
	tBinary := transport.TransportBinary(in.Transport)
	if tBinary == "" {
		tBinary = transport.TransportBinary(m.cfg.Transport)
	}

	// Teleport login params: per-call input overrides config default.
	tProxy := in.TeleportProxy
	if tProxy == "" {
		tProxy = m.cfg.TeleportProxy
	}
	tCluster := in.TeleportCluster
	if tCluster == "" {
		tCluster = m.cfg.TeleportCluster
	}

	opts := transport.DialOptions{
		Host:                  host,
		User:                  user,
		Port:                  port,
		IdentityFiles:         ids,
		RemoteInstallDir:      m.cfg.RemoteInstallDir,
		ConnectTimeout:        time.Duration(m.cfg.ConnectTimeout),
		StrictHostKeyChecking: m.cfg.StrictHostKeyChecking,
		TransportBinary:       tBinary,
		TeleportProxy:         tProxy,
		TeleportCluster:       tCluster,
		RecordingFriendly:     !m.cfg.RawProtocol, // default on for tsh; rawProtocol opts out
		// Stream tsh login output (including the browser URL) to the log in
		// real time so headless users can see the URL without waiting for
		// login to complete.  The same output is included in LoginNote on
		// success so the agent also reports it.
		LoginProgressWriter: &tshLoginWriter{log: m.log},
	}
	m.log.Printf("connecting session=%q host=%s user=%s port=%d transport=%s", name, host, user, port, tBinary)

	s, err := transport.Dial(ctx, opts)
	if err != nil {
		m.log.Printf("connect failed: %v", err)
		return m.errResult(fmt.Sprintf("failed to connect to %s: %v", host, err), ConnectOutput{}), ConnectOutput{}, nil
	}
	if s.LoginNote != "" {
		m.log.Printf("session %q: %s", name, s.LoginNote)
	}
	if s.FallbackNote != "" {
		m.log.Printf("session %q: %s", name, s.FallbackNote)
	}

	// Drain remote stderr into our log.
	go func(name string, r io.ReadCloser) {
		sc := bufioLines(r)
		for {
			line, err := sc.ReadString('\n')
			if line != "" {
				m.log.Printf("[session %s remote] %s", name, strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}(name, s.Stderr())

	m.mu.Lock()
	m.sessions[name] = s
	m.order = append(m.order, name)
	m.mu.Unlock()

	// Best-effort: ask the remote for the allowlist it enforces, so the agent
	// learns up front what it may run. A failure here doesn't abort the
	// connection; the agent can retry via remote_allowed_commands.
	allow, qerr := m.queryAllowed(s)
	if qerr != nil {
		m.log.Printf("session %q: query allowed_commands failed: %v", name, qerr)
	}

	out := ConnectOutput{
		Session:       name,
		Host:          s.Host,
		User:          s.User,
		Port:          s.Port,
		Platform:      s.Platform,
		RemotePID:     s.RemotePID,
		RemoteVersion: s.RemoteVersion,
		HasTimeout:    s.HasTimeout,
		AllowCommands: allow,
		Transport:     s.Transport,
		LoginNote:     s.LoginNote,
		FallbackNote:  s.FallbackNote,
	}
	m.log.Printf("connected session=%q host=%s platform=%s pid=%d transport=%s allow=%d",
		name, s.Host, s.Platform, s.RemotePID, s.Transport, len(allow))
	summaryMsg := fmt.Sprintf(
		"Connected session %q to %s (%s) via %s. Remote myhostmcp v%s, pid %d. timeout(1) available: %v. %d allowed command(s) (use remote_allowed_commands to list them).",
		name, s.Host, s.Platform, s.Transport, s.RemoteVersion, s.RemotePID, s.HasTimeout, len(allow))
	if s.LoginNote != "" {
		summaryMsg += "\nNote: " + s.LoginNote
	}
	if s.FallbackNote != "" {
		summaryMsg += "\nNote: " + s.FallbackNote
	}
	return m.okResult(summaryMsg, out), out, nil
}

func (m *Manager) handleExec(ctx context.Context, _ *mcp.CallToolRequest, in ExecInput) (*mcp.CallToolResult, ExecOutput, error) {
	s, name, err := m.session(in.Session)
	if err != nil {
		return m.errResult(err.Error(), ExecOutput{}), ExecOutput{}, nil
	}
	if strings.TrimSpace(in.Command) == "" {
		return m.errResult("empty command", ExecOutput{Session: name}), ExecOutput{}, nil
	}

	// The allowlist is enforced by the remote half (which reads
	// /etc/myhostmcp/config.yaml and has the final say). A disallowed command
	// comes back as a remote error below; the local half does not gate it.

	// Parse the timeout.
	timeoutMs := 0
	if in.Timeout != "" {
		d, perr := time.ParseDuration(in.Timeout)
		if perr != nil {
			return m.errResult(fmt.Sprintf("invalid timeout %q: %v", in.Timeout, perr), ExecOutput{Session: name}), ExecOutput{}, nil
		}
		timeoutMs = int(d.Milliseconds())
	} else if d := time.Duration(m.cfg.ExecTimeout); d > 0 {
		timeoutMs = int(d.Milliseconds())
	}

	if in.PTY {
		return m.errResult("pty mode is not implemented in this build", ExecOutput{ExitCode: -1, Session: name}), ExecOutput{}, nil
	}

	req := &protocol.Request{
		Type:      "exec",
		Command:   in.Command,
		CWD:       in.CWD,
		TimeoutMs: timeoutMs,
		PTY:       in.PTY,
	}
	resp, rerr := s.RoundTrip(req)
	if rerr != nil {
		m.dropSession(name)
		return m.errResult("request failed (session dropped): "+rerr.Error(), ExecOutput{ExitCode: -1, Session: name}), ExecOutput{}, nil
	}

	switch resp.Type {
	case "result":
		out := ExecOutput{
			ExitCode:   resp.ExitCode,
			Stdout:     resp.Stdout,
			Stderr:     resp.Stderr,
			CWD:        resp.CWD,
			DurationMs: resp.DurationMs,
			TimedOut:   resp.TimedOut,
			Session:    name,
		}
		summary := fmt.Sprintf("exit=%d cwd=%q timedOut=%v dur=%dms", out.ExitCode, out.CWD, out.TimedOut, out.DurationMs)
		text := summary
		if out.Stdout != "" {
			text += "\n--- stdout ---\n" + out.Stdout
		}
		if out.Stderr != "" {
			text += "\n--- stderr ---\n" + out.Stderr
		}
		return m.okResult(text, out), out, nil
	case "error":
		return m.errResult("remote error: "+resp.Error, ExecOutput{ExitCode: -1, Session: name}), ExecOutput{}, nil
	default:
		return m.errResult(fmt.Sprintf("unexpected response type %q", resp.Type), ExecOutput{ExitCode: -1, Session: name}), ExecOutput{}, nil
	}
}

func (m *Manager) handleAllowedCommands(ctx context.Context, _ *mcp.CallToolRequest, in AllowedCommandsInput) (*mcp.CallToolResult, AllowedCommandsOutput, error) {
	s, name, err := m.session(in.Session)
	if err != nil {
		return m.errResult(err.Error(), AllowedCommandsOutput{}), AllowedCommandsOutput{}, nil
	}
	allow, qerr := m.queryAllowed(s)
	if qerr != nil {
		return m.errResult("query failed: "+qerr.Error(), AllowedCommandsOutput{}), AllowedCommandsOutput{}, nil
	}
	out := AllowedCommandsOutput{Session: name, AllowCommands: allow}
	var b strings.Builder
	fmt.Fprintf(&b, "session %q enforces %d allowed command(s):", name, len(allow))
	for _, toks := range allow {
		b.WriteString("\n  - " + strings.Join(toks, " "))
	}
	return m.okResult(b.String(), out), out, nil
}

func (m *Manager) handleStatus(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, StatusOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := StatusOutput{
		Default: m.defaultOrLastLocked(),
		Version: version.Version,
	}
	for _, name := range m.order {
		s := m.sessions[name]
		out.Sessions = append(out.Sessions, SessionInfo{
			Name: name, Host: s.Host, User: s.User, Port: s.Port,
			Platform: s.Platform, RemotePID: s.RemotePID,
			RemoteVersion: s.RemoteVersion, HasTimeout: s.HasTimeout,
			Transport: s.Transport,
		})
	}
	text := fmt.Sprintf("myhostmcp v%s. %d session(s). default=%q",
		out.Version, len(out.Sessions), out.Default)
	for _, si := range out.Sessions {
		text += fmt.Sprintf("\n  - %s: %s via %s (pid %d, %s)", si.Name, si.Host, si.Transport, si.RemotePID, si.Platform)
	}
	return m.okResult(text, out), out, nil
}

func (m *Manager) handleDisconnect(_ context.Context, _ *mcp.CallToolRequest, in DisconnectInput) (*mcp.CallToolResult, DisconnectOutput, error) {
	name := in.Session
	if name == "" {
		name = m.defaultOrLast()
	}
	var closed []string
	if name == "*" {
		m.mu.Lock()
		all := append([]string{}, m.order...)
		m.mu.Unlock()
		for _, n := range all {
			m.dropSession(n)
			closed = append(closed, n)
		}
	} else {
		m.mu.Lock()
		_, ok := m.sessions[name]
		m.mu.Unlock()
		if !ok {
			return m.errResult(fmt.Sprintf("no session named %q", name), DisconnectOutput{}), DisconnectOutput{}, nil
		}
		m.dropSession(name)
		closed = append(closed, name)
	}
	out := DisconnectOutput{Disconnected: closed}
	text := fmt.Sprintf("Disconnected %d session(s): %s", len(closed), strings.Join(closed, ", "))
	return m.okResult(text, out), out, nil
}

// ----- helpers -----------------------------------------------------------------

// session returns the named session, or the default if name is empty.
func (m *Manager) session(name string) (*transport.Session, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if name == "" {
		name = m.defaultOrLastLocked()
	}
	if name == "" {
		return nil, "", fmt.Errorf("no remote session is open; call remote_connect first")
	}
	s, ok := m.sessions[name]
	if !ok {
		return nil, "", fmt.Errorf("no session named %q; use remote_status to list sessions", name)
	}
	return s, name, nil
}

// queryAllowed sends an allowed_commands request to a session and returns the
// allowlist the remote enforces. The remote always has the final say over what
// remote_exec may run; the local half cannot change it.
func (m *Manager) queryAllowed(s *transport.Session) ([][]string, error) {
	resp, err := s.RoundTrip(&protocol.Request{Type: "allowed_commands"})
	if err != nil {
		return nil, err
	}
	switch resp.Type {
	case "allowlist":
		return resp.AllowCommands, nil
	case "error":
		return nil, fmt.Errorf("%s", resp.Error)
	default:
		return nil, fmt.Errorf("unexpected response type %q", resp.Type)
	}
}

// defaultOrLast returns the default session name, or the most recently
// connected one. Safe to call without holding the lock.
func (m *Manager) defaultOrLast() string {
	if m.default_ != "" {
		return m.default_
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defaultOrLastLocked()
}

// defaultOrLastLocked is the lock-free inner version; the caller must hold m.mu.
func (m *Manager) defaultOrLastLocked() string {
	if m.default_ != "" {
		return m.default_
	}
	if len(m.order) == 0 {
		return ""
	}
	return m.order[len(m.order)-1]
}

func (m *Manager) dropSession(name string) {
	m.mu.Lock()
	s, ok := m.sessions[name]
	if ok {
		delete(m.sessions, name)
		m.order = remove(m.order, name)
	}
	m.mu.Unlock()
	if ok {
		_ = s.Close()
		m.log.Printf("disconnected session=%q", name)
	}
}

func (m *Manager) closeAll() {
	m.mu.Lock()
	names := append([]string{}, m.order...)
	m.mu.Unlock()
	for _, n := range names {
		m.dropSession(n)
	}
}

// okResult builds a successful CallToolResult with human-readable text plus the
// structured output (the SDK also places `out` in StructuredContent).
func (m *Manager) okResult(text string, out any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// errResult builds a tool result flagged as an error (IsError=true) so the
// agent can reason about it, rather than a protocol-level error.
func (m *Manager) errResult(msg string, _ any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

func openLogger(path string) (*log.Logger, error) {
	if path == "" {
		return log.New(os.Stderr, "myhostmcp: ", log.LstdFlags|log.Lmicroseconds), nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", path, err)
	}
	return log.New(f, "myhostmcp: ", log.LstdFlags|log.Lmicroseconds), nil
}

// hostName turns a host into a safe session name (strips user@ and :port).
func hostName(h string) string {
	if i := strings.Index(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h
}

func remove(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// tshLoginWriter is an io.Writer that line-buffers tsh login output and
// forwards each non-empty line to the manager's logger.  It is used as
// DialOptions.LoginProgressWriter so the browser URL (and any other status
// lines) appear in the log immediately while tsh login blocks waiting for
// the browser callback.
type tshLoginWriter struct {
	log *log.Logger
	buf strings.Builder
}

func (w *tshLoginWriter) Write(p []byte) (int, error) {
	// Normalize line boundaries BEFORE buffering.  tsh uses a bare \r (no
	// following \n) to overwrite its "waiting for the browser" progress text
	// on an interactive terminal.  If any \r reaches the logger it repositions
	// the terminal cursor to column 0 without clearing the line, so a live TUI
	// (e.g. pi) ends up with our line drawn over the previous one and the tail
	// of the old line left as ghost text.  Treat every \r as a line boundary
	// (collapsing \r\n) so the buffer only ever holds control-char-free text
	// and every emitted line is complete and clean.
	norm := strings.ReplaceAll(string(p), "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	w.buf.WriteString(norm)

	// Flush all complete lines from the buffer.
	for {
		s := w.buf.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		line := stripControl(s[:i])
		w.buf.Reset()
		w.buf.WriteString(s[i+1:])
		if strings.TrimSpace(line) != "" {
			w.log.Printf("tsh login: %s", line)
		}
	}
	return len(p), nil
}

// stripControl removes C0 control characters (except tab) from a single line
// of text, so no stray control byte can corrupt a terminal or TUI that renders
// our log/tool output.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// bufioLines wraps an io.ReadCloser in a bufio.Reader.
func bufioLines(r io.Reader) *bufioReader {
	return &bufioReader{r}
}

type bufioReader struct {
	r io.Reader
}

func (b *bufioReader) ReadString(delim byte) (string, error) {
	return readLine(b.r, delim)
}

// readLine reads up to and including delim from r, byte by byte. Good enough
// for low-volume diagnostic stderr.
func readLine(r io.Reader, delim byte) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.WriteByte(buf[0])
			if buf[0] == delim {
				return sb.String(), nil
			}
		}
		if err != nil {
			return sb.String(), err
		}
	}
}
