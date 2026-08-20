// Package transport connects the local half to a remote host over SSH (or
// Teleport tsh) and speaks the private protocol (see package protocol) over
// the SSH channel.
//
// It drives the real ssh (or tsh) binary via os/exec, so authentication uses
// the user's normal SSH setup: ~/.ssh/config, default keys, ssh-agent,
// bastions, etc.  When tsh (Teleport) is used, authentication goes through
// the Teleport proxy instead.  If the user is not yet logged in to Teleport,
// [Dial] runs `tsh login` automatically before connecting; for browser-based
// SSO the browser opens on the user's desktop and the login completes
// out-of-band.  This package stores NO credentials of its own.
//
// # Transport selection
//
// With [TransportAuto] (the default), [Dial] checks whether tsh is in PATH.
// If it is, tsh is tried first.  If tsh fails for any reason — login failed,
// host is not a Teleport node, cluster is unreachable, etc. — Dial falls back
// transparently to ssh.  The binary actually used is recorded in
// [Session.Transport]; a non-empty [Session.FallbackNote] explains why a
// fallback occurred.
//
// # PTY allocation and session recording
//
// When using tsh (Teleport), the persistent session must be an interactive
// shell session so that Teleport classifies and records it correctly.
//
// Teleport distinguishes two SSH session types:
//   - exec:  `tsh ssh -tt host "command"` — Teleport sees the literal command
//     string, marks the session as non-interactive, and may not record it.
//   - shell: `tsh ssh -tt host` (no command) — Teleport sees an interactive
//     shell session and records it as such.
//
// startRemote achieves a shell session by passing NO command argument to
// `tsh ssh` and instead writing the startup sequence to stdin immediately
// after the process starts.  The remote shell reads and executes it:
//
//	"stty raw -echo 2>/dev/null; ~/.myhostmcp/myhostmcp remote 2>>~/.myhostmcp/remote.log\n"
//
// stty raw -echo: disables PTY output CRLF translation (ONLCR) and input
// echo so the PTY does not corrupt the JSON framing.
// 2>>remote.log on the binary: keeps its stderr out of the PTY master output
// that we parse as JSON (a PTY merges remote stdout and stderr), while still
// preserving startup diagnostics on the host — if the remote exits before it
// announces "ready", this log holds the reason. recv() additionally skips any
// line that does not start with '{' as a safety net for shell prompts, MOTD,
// or command echo that arrive before stty settles.
//
// When using plain ssh, no PTY is requested.  stdout and stderr remain on
// separate OS pipes, so the remote binary's log output flows to s.Stderr()
// and is forwarded to the local log by the caller.
//
// One-shot helper commands (platform detection, binary upload) never request
// a PTY regardless of transport.
package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"myhostmcp/internal/embed"
	"myhostmcp/internal/protocol"
	"myhostmcp/internal/version"
)

// safePathRe restricts the remote install dir to characters that are safe to
// inject unquoted into a remote shell command (so that ~ can expand). It is
// config-trusted, not agent-controlled.
var safePathRe = regexp.MustCompile(`^[A-Za-z0-9_./~\-]+$`)

// TransportBinary selects which SSH-compatible binary is used to connect.
type TransportBinary string

const (
	// TransportAuto (the zero value / default) tries tsh first when tsh is
	// found in PATH, and falls back to ssh automatically if tsh fails for any
	// reason. If tsh is not found in PATH, ssh is used directly.
	TransportAuto TransportBinary = ""

	// TransportSSH always uses the system ssh binary; tsh is never tried.
	TransportSSH TransportBinary = "ssh"

	// TransportTsh always uses tsh (Teleport); no fallback to ssh on failure.
	TransportTsh TransportBinary = "tsh"
)

// DialOptions describes how to reach a remote host.
type DialOptions struct {
	Host                  string          // SSH host (as in ~/.ssh/config); required
	User                  string          // remote OS user; "" = ssh/tsh default
	Port                  int             // 0 or 22 = default
	IdentityFiles         []string        // optional -i paths; nil = ssh defaults
	RemoteInstallDir      string          // e.g. "~/.myhostmcp"; must be safe-path
	ConnectTimeout        time.Duration   // applied to connection steps (not tsh login)
	StrictHostKeyChecking string          // ssh StrictHostKeyChecking value; default "accept-new"
	TransportBinary       TransportBinary // which binary to use; default TransportAuto

	// Teleport-specific options (used only when binary == "tsh").

	// TeleportProxy is the Teleport proxy address passed to `tsh login
	// --proxy=...`.  If empty, tsh uses its own configured default.
	TeleportProxy string

	// TeleportCluster is an optional Teleport cluster (leaf cluster) name
	// passed as a positional argument to `tsh login`.  If empty, the default
	// cluster for the proxy is used.
	TeleportCluster string
}

// Session is an established connection to a remote myhostmcp remote half.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr io.ReadCloser

	Host          string
	User          string
	Port          int
	RemotePID     int
	RemoteVersion string
	HasTimeout    bool
	Platform      string // "Linux x86_64", etc.
	Transport     string // "ssh" or "tsh" — whichever binary was actually used
	FallbackNote  string // non-empty when tsh was tried first but fell back to ssh
	LoginNote     string // non-empty when tsh login was performed during Dial

	writeMu chan struct{}
	rpcMu   chan struct{}
	closed  chan struct{}
	cancel  context.CancelFunc
}

// Dial connects to the host, ensures the remote half is installed and
// up-to-date, starts it, and waits for its "ready" announcement.
//
// Transport selection and Teleport login:
//   - With [TransportAuto] (default), tsh is tried first if found in PATH.
//     If tsh is unavailable or fails, Dial falls back transparently to ssh.
//   - When tsh is selected, Dial first checks `tsh status`.  If the user is
//     not logged in to Teleport, it runs `tsh login` automatically.  For
//     browser-based SSO the login opens a browser on the user's desktop and
//     waits for completion before proceeding.  The ConnectTimeout is NOT
//     applied to the login step so the user has as long as needed.
//   - [Session.Transport] records which binary was used; [Session.LoginNote]
//     is non-empty when a login was performed; [Session.FallbackNote] is
//     non-empty when tsh failed and ssh was used instead.
func Dial(ctx context.Context, opts DialOptions) (*Session, error) {
	if opts.Host == "" {
		return nil, fmt.Errorf("transport: host is required")
	}
	if opts.RemoteInstallDir == "" {
		opts.RemoteInstallDir = "~/.myhostmcp"
	}
	if !safePathRe.MatchString(opts.RemoteInstallDir) {
		return nil, fmt.Errorf("transport: remoteInstallDir %q contains disallowed characters", opts.RemoteInstallDir)
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 15 * time.Second
	}

	binary, tryFallback := resolveBinary(opts.TransportBinary)

	s, err := dialWith(ctx, opts, binary)
	if err != nil && tryFallback {
		// tsh was available but failed (could not login, host not in Teleport,
		// etc.).  Fall back to ssh.  The original ctx is unmodified (dialWith
		// operates on a local copy), so ssh gets the full remaining budget.
		tshErr := err
		s, err = dialWith(ctx, opts, "ssh")
		if err == nil {
			s.FallbackNote = fmt.Sprintf("tsh failed (%v); using ssh", tshErr)
		}
	}
	return s, err
}

// resolveBinary returns the binary to try first and whether to automatically
// fall back to "ssh" on failure.  With TransportAuto, tsh is tried first when
// it is found in PATH.
func resolveBinary(pref TransportBinary) (binary string, fallback bool) {
	switch pref {
	case TransportSSH:
		return "ssh", false
	case TransportTsh:
		return "tsh", false
	default: // TransportAuto
		if _, err := exec.LookPath("tsh"); err == nil {
			return "tsh", true // found in PATH; fall back to ssh if tsh fails
		}
		return "ssh", false
	}
}

// dialWith performs the full connect → install → start sequence using the
// named binary ("ssh" or "tsh").  It is called by Dial and may be called twice
// when falling back.
func dialWith(ctx context.Context, opts DialOptions, binary string) (*Session, error) {
	var loginNote string

	if binary == "tsh" {
		// Ensure the user is logged in to Teleport before we apply the
		// ConnectTimeout.  tsh login may require a browser-based SSO flow that
		// takes much longer than the connection timeout; we use the caller's ctx
		// (which for MCP requests typically has no short deadline).
		var err error
		loginNote, err = ensureTshLogin(ctx, opts)
		if err != nil {
			return nil, err // caller (Dial) may fall back to ssh
		}

		// Apply the connection timeout only for the actual dial steps below.
		if opts.ConnectTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, opts.ConnectTimeout)
			defer cancel()
		}
	}

	// 1. Detect the remote platform.
	sysname, machine, err := detectPlatform(ctx, opts, binary)
	if err != nil {
		return nil, fmt.Errorf("detect platform: %w", err)
	}

	// 2. Ensure the remote binary is present and version-current.
	remotePath := opts.RemoteInstallDir + "/myhostmcp"
	if err := ensureInstalled(ctx, opts, remotePath, sysname, machine, binary); err != nil {
		return nil, fmt.Errorf("install remote: %w", err)
	}

	// 3. Start the remote half as a persistent session.
	s, err := startRemote(ctx, opts, remotePath, binary)
	if err != nil {
		return nil, fmt.Errorf("start remote: %w", err)
	}
	s.Platform = sysname + " " + machine
	s.Transport = binary
	s.LoginNote = loginNote

	// 4. Wait for the "ready" announcement on the protocol channel, bounded by
	// the connect timeout. recv() is a blocking pipe read with no deadline, so a
	// remote that dies silently before announcing (e.g. a config parse error, a
	// missing/incompatible binary, or a login shell that never runs our startup
	// line) would otherwise hang Dial forever. On timeout we kill the session
	// and point the operator at the remote stderr log for the real cause.
	readyTimeout := opts.ConnectTimeout
	if readyTimeout <= 0 {
		readyTimeout = 15 * time.Second
	}
	ready, err := s.recvReady(readyTimeout, opts.RemoteInstallDir+"/remote.log")
	if err != nil {
		s.kill()
		return nil, fmt.Errorf("waiting for remote ready: %w", err)
	}
	if ready.Type != "log" || ready.Msg != "ready" {
		s.kill()
		return nil, fmt.Errorf("unexpected first message: %+v", ready)
	}
	s.RemoteVersion = ready.Version
	s.RemotePID = ready.PID
	s.HasTimeout = ready.HasTimeout
	if s.RemoteVersion != version.Version {
		s.kill()
		return nil, fmt.Errorf("remote version %q != local %q after install; refusing",
			s.RemoteVersion, version.Version)
	}
	return s, nil
}

// ensureTshLogin checks whether the user is currently logged in to Teleport
// (via `tsh status`).  If not, it runs `tsh login` to authenticate.
//
// For browser-based SSO — the common enterprise case — `tsh login` opens a
// browser window on the user's desktop and waits for completion.  Terminal-
// based auth (password / TOTP) requires a tty on stdin; since myhostmcp's
// stdin is the MCP protocol stream we cannot provide one.  Users who need
// terminal-based auth should run `tsh login` manually before starting the
// agent session, or configure device trust / machine certificates so that no
// interactive auth is required.
//
// The returned note is non-empty when a login was actually performed (cert
// was missing or expired); it is stored in [Session.LoginNote].
func ensureTshLogin(ctx context.Context, opts DialOptions) (note string, err error) {
	// Check current login status.  tsh status exits 0 when a valid cert exists.
	check := exec.CommandContext(ctx, "tsh", "status")
	check.Stdout = io.Discard
	check.Stderr = io.Discard
	if check.Run() == nil {
		return "", nil // already authenticated; nothing to do
	}

	// Not authenticated (cert missing or expired); attempt login.
	args := []string{"login"}
	if opts.TeleportProxy != "" {
		args = append(args, "--proxy="+opts.TeleportProxy)
	}
	if opts.TeleportCluster != "" {
		args = append(args, opts.TeleportCluster)
	}

	// Capture stdout+stderr for error reporting.  We do NOT wire up stdin:
	// browser-based SSO works without stdin (browser opens via the inherited
	// display environment); terminal-based auth would fail here by design.
	login := exec.CommandContext(ctx, "tsh", args...)
	var combined bytes.Buffer
	login.Stdout = &combined
	login.Stderr = &combined
	if lerr := login.Run(); lerr != nil {
		out := strings.TrimSpace(combined.String())
		if out != "" {
			return "", fmt.Errorf("tsh login: %w\n%s", lerr, out)
		}
		return "", fmt.Errorf("tsh login: %w", lerr)
	}

	return "performed tsh login (certificate was missing or expired)", nil
}

// Send writes a protocol.Request as one newline-delimited JSON line.
func (s *Session) Send(req *protocol.Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.writeMu <- struct{}{}
	defer func() { <-s.writeMu }()
	if _, err := s.stdin.Write(b); err != nil {
		return fmt.Errorf("write to %s: %w", s.Transport, err)
	}
	return nil
}

// Recv reads one protocol.Response line.
func (s *Session) Recv() (*protocol.Response, error) {
	return s.recv()
}

// RoundTrip sends one request and reads exactly one response while holding the
// per-session RPC lock, so concurrent callers cannot consume each other's
// replies.
func (s *Session) RoundTrip(req *protocol.Request) (*protocol.Response, error) {
	s.rpcMu <- struct{}{}
	defer func() { <-s.rpcMu }()
	if err := s.Send(req); err != nil {
		return nil, err
	}
	return s.recv()
}

func (s *Session) recv() (*protocol.Response, error) {
	for {
		line, err := s.stdout.ReadString('\n')

		// Strip CR and LF. With a PTY, the line discipline may produce \r\n
		// (ONLCR) before stty raw takes effect; also handles bare \r.
		line = strings.TrimRight(line, "\r\n")

		if resp, ok := parseResponseLine(line); ok {
			return resp, nil
		}

		// Non-JSON line: a log/diagnostic line from ssh/tsh, or (with a PTY,
		// which merges stdout and stderr) a host login/monitoring banner, MOTD,
		// shell prompt, or the echoed startup command that arrives before stty
		// raw settles. Skip it. If we also got a read error, surface that now.
		if err != nil {
			return nil, fmt.Errorf("read from %s: %w", s.Transport, err)
		}
		// err == nil: empty line or non-JSON line — loop and read the next.
	}
}

// parseResponseLine extracts a single protocol.Response from one line of the
// channel. Protocol responses are always a complete JSON object per line
// (writeResp), but over a tsh PTY the host may merge non-protocol bytes — a
// login/monitoring banner, MOTD, a shell prompt, or the echoed startup
// command — onto the same physical line as the JSON. So rather than requiring
// the line to *begin* with '{', we scan for the first '{' that decodes as a
// valid Response, tolerating both leading and trailing noise. A json.Decoder
// is used so trailing bytes after the object are ignored.
func parseResponseLine(line string) (*protocol.Response, bool) {
	for i := 0; i < len(line); i++ {
		if line[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line[i:]))
		var resp protocol.Response
		if err := dec.Decode(&resp); err == nil && resp.Type != "" {
			return &resp, true
		}
		// This '{' did not start a valid Response (e.g. a brace inside banner
		// text); try the next one.
	}
	return nil, false
}

// recvReady waits for the remote's first protocol message (its "ready"
// announcement), but no longer than timeout. Because recv() is a blocking pipe
// read with no deadline, a remote that exits before announcing would hang the
// caller forever; this bounds that wait so Dial can fail with a useful message
// instead. logHint names the remote-side stderr log where the real cause is
// recorded. On timeout the caller should kill the session.
//
// The reader goroutine is not leaked: the channel is buffered, and once the
// caller kills the session the underlying pipe closes and recv() returns.
func (s *Session) recvReady(timeout time.Duration, logHint string) (*protocol.Response, error) {
	type result struct {
		resp *protocol.Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := s.recv()
		ch <- result{resp, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-timer.C:
		return nil, fmt.Errorf("timed out after %s; the remote half started but never announced itself "+
			"(likely it exited on startup). Check its stderr log at %s on the host for the cause "+
			"(e.g. a config parse error or an incompatible binary)", timeout, logHint)
	}
}

// Stderr returns a reader for the remote's diagnostic output (ssh/tsh + remote
// stderr). The caller should drain it (e.g. copy to a log file) to avoid
// blocking.
func (s *Session) Stderr() io.ReadCloser { return s.stderr }

// Close sends a shutdown request, closes stdin, and waits for the process to exit.
func (s *Session) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
	}
	close(s.closed)
	// Best-effort shutdown.
	_ = s.Send(&protocol.Request{Type: "shutdown"})
	_ = s.stdin.Close()
	if s.cancel != nil {
		s.cancel()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	if s.cmd != nil {
		_ = s.cmd.Wait()
	}
	return nil
}

func (s *Session) kill() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// sshArgs builds the ssh argument list (everything before the host).
// Does NOT include -tt; that is added only in startRemote for the persistent
// session, not for one-shot helper commands.
func sshArgs(opts DialOptions) []string {
	strict := opts.StrictHostKeyChecking
	if strict == "" {
		strict = "accept-new" // auto-add new hosts, reject changed
	}
	a := []string{
		"-o", "BatchMode=yes", // never prompt; fail instead (non-interactive)
		"-o", "StrictHostKeyChecking=" + strict,
		"-o", "ServerAliveInterval=30", // keep long sessions alive
		"-o", "ServerAliveCountMax=4",  // ~2 min of missed keepalives → drop
	}
	if opts.Port != 0 && opts.Port != 22 {
		a = append(a, "-p", strconv.Itoa(opts.Port))
	}
	for _, f := range opts.IdentityFiles {
		if f != "" {
			a = append(a, "-i", f)
		}
	}
	ct := int(opts.ConnectTimeout.Seconds())
	if ct < 1 {
		ct = 1
	}
	a = append(a, "-o", "ConnectTimeout="+strconv.Itoa(ct))
	if opts.User != "" {
		a = append(a, "-l", opts.User)
	}
	return a
}

// tshArgs builds the minimal argument list for `tsh ssh`.  Teleport manages
// authentication and host-key verification through its proxy and certificate
// authorities, so we use only the flags that are universally safe to pass to
// tsh.  ConnectTimeout is enforced via a context deadline in dialWith rather
// than via an -o option that tsh may not support.
// Does NOT include -tt; that is added only in startRemote.
func tshArgs(opts DialOptions) []string {
	var a []string
	if opts.Port != 0 && opts.Port != 22 {
		a = append(a, "-p", strconv.Itoa(opts.Port))
	}
	for _, f := range opts.IdentityFiles {
		if f != "" {
			a = append(a, "-i", f)
		}
	}
	if opts.User != "" {
		a = append(a, "-l", opts.User)
	}
	return a
}

// runOnce runs a one-shot command on the remote host and returns its stdout.
// stderr is surfaced in the error if the command fails.  No PTY is requested.
func runOnce(ctx context.Context, opts DialOptions, binary, remoteCmd string, stdin io.Reader) ([]byte, error) {
	var c *exec.Cmd
	if binary == "tsh" {
		args := append(tshArgs(opts), opts.Host, remoteCmd)
		c = exec.CommandContext(ctx, "tsh", append([]string{"ssh"}, args...)...)
	} else {
		args := append(sshArgs(opts), opts.Host, remoteCmd)
		c = exec.CommandContext(ctx, "ssh", args...)
	}
	if stdin != nil {
		c.Stdin = stdin
	}
	var out, errOut bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errOut
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w; stderr: %s", binary, opts.Host, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

func detectPlatform(ctx context.Context, opts DialOptions, binary string) (sysname, machine string, err error) {
	out, err := runOnceFn(ctx, opts, binary, "uname -sm", nil)
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected uname output: %q", strings.TrimSpace(string(out)))
	}
	return fields[0], fields[1], nil
}

var runOnceFn = runOnce
var binaryForUnameFn = embed.BinaryForUname

func ensureInstalled(ctx context.Context, opts DialOptions, remotePath, sysname, machine, binary string) error {
	// Check the installed version (if any) with a single one-shot command.
	checkCmd := fmt.Sprintf(
		`if [ -x %s ]; then %s remote --version; else echo __MISSING__; fi`,
		remotePath, remotePath)
	out, err := runOnceFn(ctx, opts, binary, checkCmd, nil)
	if err != nil {
		// Non-fatal: treat a hard failure as "needs install" and try uploading.
		out = []byte("__MISSING__")
	}
	installed := parseReportedVersion(string(out))
	if installed == version.Version {
		return nil // up to date
	}

	// Fetch the matching embedded binary.
	bin, err := binaryForUnameFn(sysname, machine)
	if err != nil {
		return fmt.Errorf("no prebuilt remote binary for %s %s: %w", sysname, machine, err)
	}

	// Upload: stream the binary to `cat > remotePath` on the remote shell.
	uploadCmd := fmt.Sprintf(
		`mkdir -p %s && cat > %s && chmod +x %s`,
		opts.RemoteInstallDir, remotePath, remotePath)
	if _, err := runOnceFn(ctx, opts, binary, uploadCmd, bytes.NewReader(bin)); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

// startRemote launches the persistent remote myhostmcp process.
//
// See the package-level doc for the PTY / session-recording strategy.
func startRemote(_ context.Context, opts DialOptions, remotePath, binary string) (*Session, error) {
	// The persistent process must outlive the MCP request that initiated it:
	// the SDK cancels the request context once the tool handler returns, which
	// would kill a CommandContext bound to it.  Use a background-derived
	// lifecycle context cancelled on Session.Close instead.
	lctx, cancel := context.WithCancel(context.Background())

	var (
		c       *exec.Cmd
		initCmd string // non-empty → written to stdin after Start()
	)

	if binary == "tsh" {
		// NO command argument: tsh sends an SSH "shell" request so Teleport
		// treats the session as interactive and records it.
		// -tt: force PTY allocation even though our local stdin is a pipe.
		args := append(tshArgs(opts), "-tt", opts.Host)
		c = exec.CommandContext(lctx, "tsh", append([]string{"ssh"}, args...)...)
		// The startup sequence is sent to the shell via stdin immediately after
		// Start().  The shell executes it and then myhostmcp remote takes over.
		// stty raw -echo: suppress PTY CRLF translation (ONLCR) and input echo.
		// The binary's stderr must be kept out of the PTY data stream (the PTY
		// merges stdout and stderr, which would corrupt our JSON framing), but
		// discarding it hides startup failures. Redirect it to a log file in the
		// install dir instead, so `waiting for remote ready` timeouts remain
		// diagnosable on the host. RemoteInstallDir is config-trusted and
		// safe-path validated, so it is safe to inject unquoted.
		logPath := opts.RemoteInstallDir + "/remote.log"
		initCmd = "stty raw -echo 2>/dev/null; " + remotePath + " remote 2>>" + logPath + "\n"
	} else {
		// Plain ssh: run the binary directly; no PTY, separate stderr pipe.
		args := append(sshArgs(opts), opts.Host, remotePath+" remote")
		c = exec.CommandContext(lctx, "ssh", args...)
	}

	stdin, err := c.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := c.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}

	if initCmd != "" {
		// Write the startup command to the remote shell's stdin. The pipe
		// buffer is large enough that this never blocks at start-up.
		if _, err := io.WriteString(stdin, initCmd); err != nil {
			_ = c.Process.Kill()
			cancel()
			return nil, fmt.Errorf("write tsh startup command: %w", err)
		}
	}

	return &Session{
		cmd:     c,
		stdin:   stdin,
		stdout:  bufio.NewReaderSize(stdout, 64*1024),
		stderr:  stderr,
		Host:    opts.Host,
		User:    opts.User,
		Port:    opts.Port,
		writeMu: make(chan struct{}, 1),
		rpcMu:   make(chan struct{}, 1),
		closed:  make(chan struct{}),
		cancel:  cancel,
	}, nil
}

func parseReportedVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	fields := strings.Fields(raw)
	if len(fields) >= 2 && fields[0] == "myhostmcp" {
		return fields[1]
	}
	if len(fields) == 1 {
		return fields[0]
	}
	return raw
}
