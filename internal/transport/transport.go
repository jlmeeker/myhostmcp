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
// # One session per connect
//
// A single Dial opens exactly ONE ssh/tsh connection and does all of its work
// — platform detection, version check, binary upload, and the long-lived RPC
// channel — inside that one remote shell.  This matters for two reasons:
//
//   - Teleport: `tsh ssh host "command"` is classified as a non-interactive
//     *exec* session (no PTY, usually not recorded), whereas `tsh ssh host`
//     with NO command is an interactive *shell* session that Teleport records
//     and that appears in `tsh sessions ls`.  Running each bootstrap step as a
//     separate `tsh ssh host <cmd>` would therefore litter the audit log with
//     non-interactive sessions alongside the one real interactive session.
//   - ssh: each `ssh host <cmd>` is a separate authentication + session in the
//     host's auth/sshd logs.  One shell per connect keeps those logs clean and
//     avoids repeating the connection handshake.
//
// So both transports pass NO command argument.  startShell opens the shell and
// bringUp drives the bootstrap over its stdin/stdout using sentinel markers
// (see Session.sh/sync/upload), then hands the same shell off to the persistent
// process with `exec ... remote`.
//
// # PTY handling
//
// tsh: a PTY is requested (-tt) so Teleport sees an interactive shell.  The
// PTY is put into raw mode (`stty raw -echo`) so its CRLF translation (ONLCR)
// and input echo do not corrupt the newline-delimited JSON framing.  Because a
// PTY merges stdout and stderr, the remote binary's stderr is redirected to a
// log file (`2>>~/.myhostmcp/remote.log`) so it stays out of the JSON stream
// while remaining diagnosable on the host.  During bootstrap, sentinel markers
// are built at runtime with printf (so an echoed command line can never
// contain the assembled marker), making marker matching robust even before
// stty raw settles.
//
// ssh: NO PTY is requested.  stdout and stderr stay on separate OS pipes, so
// the remote binary's log output flows to s.Stderr() and is forwarded to the
// local log by the caller, and no CRLF/echo mangling can occur.
package transport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

	// RecordingFriendly, when true and the tsh transport is used, starts the
	// remote half in APC framing mode: responses are wrapped in an invisible
	// APC envelope and a human-readable transcript is emitted alongside them so
	// Teleport session recordings play back as a readable shell session. It has
	// no effect on the ssh transport (which is not recorded). Callers (the local
	// half) enable this by default for tsh; it is disabled via the rawProtocol
	// config opt-out.
	RecordingFriendly bool

	// LoginProgressWriter, if non-nil, receives tsh login output (including
	// the browser URL line) in real time as tsh login runs.  In headless
	// environments where the browser cannot open automatically, tsh prints a
	// URL for the user to open manually; this writer surfaces that URL
	// immediately rather than waiting until after login completes.
	LoginProgressWriter io.Writer
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

	// apc is set when the remote half was started in APC framing mode; recv
	// then extracts responses from APC envelopes instead of JSON lines. nonce
	// is learned from the first (ready) frame and used to reject spoofed
	// frames injected by command output.
	apc   bool
	nonce string
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

	// 1. Open the single shell session (no command argument — see package doc).
	remotePath := opts.RemoteInstallDir + "/myhostmcp"
	s, err := startShell(opts, binary)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}
	s.Transport = binary
	s.LoginNote = loginNote

	// 2. Over that one shell, detect the platform, install/upgrade the remote
	//    binary if needed, and hand the shell off to the persistent process.
	//    Bounded by ctx: bringUp's reads block on the pipe, so on timeout we
	//    kill the session (closing the pipe) to unblock the goroutine.
	errc := make(chan error, 1)
	go func() { errc <- s.bringUp(ctx, opts, remotePath, binary) }()
	select {
	case err := <-errc:
		if err != nil {
			s.kill()
			return nil, fmt.Errorf("bootstrap remote: %w", err)
		}
	case <-ctx.Done():
		s.kill()
		return nil, fmt.Errorf("bootstrap remote: timed out (%w); check %s/remote.log on the host",
			ctx.Err(), opts.RemoteInstallDir)
	}

	// 3. Wait for the "ready" announcement on the protocol channel, bounded by
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
	// Pass --proxy when one is configured so we check status for the specific
	// proxy we intend to connect through, not just any cached Teleport cert.
	statusArgs := []string{"status"}
	if opts.TeleportProxy != "" {
		statusArgs = append(statusArgs, "--proxy="+opts.TeleportProxy)
	}
	check := exec.CommandContext(ctx, "tsh", statusArgs...)
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

	// Capture stdout+stderr for error reporting and for the success note.
	// If the caller supplied a LoginProgressWriter, tee output there in real
	// time so the browser URL is visible immediately — critical for headless
	// environments where the browser cannot open and the user must visit the
	// URL manually.  We do NOT wire up stdin: browser-based SSO works without
	// stdin; terminal-based auth would fail here by design.
	var capture bytes.Buffer
	var loginOut io.Writer = &capture
	if opts.LoginProgressWriter != nil {
		loginOut = io.MultiWriter(&capture, opts.LoginProgressWriter)
	}
	login := exec.CommandContext(ctx, "tsh", args...)
	login.Stdout = loginOut
	login.Stderr = loginOut
	if lerr := login.Run(); lerr != nil {
		out := strings.TrimSpace(capture.String())
		if out != "" {
			return "", fmt.Errorf("tsh login: %w\n%s", lerr, out)
		}
		return "", fmt.Errorf("tsh login: %w", lerr)
	}

	// Include tsh's output in the note so the agent can report the URL and
	// any status messages back to the user even after login completes.
	// Strip carriage returns before embedding: tsh uses bare \r to overwrite
	// progress lines on an interactive terminal.  Left in the note they cause
	// cursor repositioning when any terminal or TUI renders the tool result,
	// letting adjacent UI chrome bleed onto the same line.
	note = "performed tsh login (certificate was missing or expired)"
	if msg := strings.TrimSpace(capture.String()); msg != "" {
		msg = strings.ReplaceAll(msg, "\r\n", "\n") // Windows-style → Unix
		msg = strings.ReplaceAll(msg, "\r", "")     // bare \r → gone
		note += "\n" + strings.TrimSpace(msg) + "\n"
	}
	return note, nil
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
	if s.apc {
		return s.recvAPC()
	}
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

// recvAPC reads one protocol.Response from an APC-framed stream (recording-
// friendly mode). It scans the channel for APC envelopes, ignoring the human-
// readable transcript (and any stray APC sequences in command output), and
// returns the first envelope whose payload carries the session nonce. The
// nonce is established by the first frame (the remote's "ready"), which cannot
// be preceded by command output, and thereafter authenticates every frame so
// output cannot spoof a response.
func (s *Session) recvAPC() (*protocol.Response, error) {
	for {
		payload, err := s.readAPCFrame()
		if err != nil {
			return nil, fmt.Errorf("read from %s: %w", s.Transport, err)
		}
		if !strings.HasPrefix(payload, protocol.APCTag) {
			continue // not one of ours (e.g. an APC emitted by a command)
		}
		rest := payload[len(protocol.APCTag):]
		brace := strings.IndexByte(rest, '{')
		if brace < 0 {
			continue
		}
		nonce := rest[:brace]
		if s.nonce == "" {
			s.nonce = nonce // first frame (ready) establishes the session nonce
		} else if nonce != s.nonce {
			continue // foreign/spoofed frame; ignore
		}
		var resp protocol.Response
		if err := json.Unmarshal([]byte(rest[brace:]), &resp); err == nil && resp.Type != "" {
			return &resp, nil
		}
		// Malformed JSON in a nonce-tagged frame: keep scanning.
	}
}

// readAPCFrame reads bytes from the channel and returns the payload of the next
// APC string (the bytes between the ESC '_' introducer and the ESC '\'
// terminator). It is robust to arbitrary transcript bytes and to stray/partial
// APC-like sequences in command output: a genuine payload is pure JSON with no
// raw ESC byte, so any ESC that is not the terminator or a fresh introducer
// means the current candidate is not ours and the scan restarts.
func (s *Session) readAPCFrame() (string, error) {
	const esc = 0x1b
	for {
		// Phase A: scan for the APC introducer ESC '_'.
		for {
			b, err := s.stdout.ReadByte()
			if err != nil {
				return "", err
			}
			if b != esc {
				continue
			}
			b2, err := s.stdout.ReadByte()
			if err != nil {
				return "", err
			}
			if b2 == '_' {
				break // introducer found
			}
			// Some other ESC sequence (e.g. a color code in the transcript);
			// keep scanning.
		}
		// Phase B: collect the payload until the String Terminator ESC '\'.
		var sb strings.Builder
		aborted := false
		for {
			b, err := s.stdout.ReadByte()
			if err != nil {
				return "", err
			}
			if b != esc {
				sb.WriteByte(b)
				continue
			}
			b2, err := s.stdout.ReadByte()
			if err != nil {
				return "", err
			}
			switch b2 {
			case '\\':
				return sb.String(), nil // complete frame
			case '_':
				sb.Reset() // a fresh introducer; restart collection
			default:
				aborted = true // stray ESC seq; abandon this candidate
			}
			if aborted {
				break
			}
		}
		// aborted: fall through to Phase A and resume scanning.
	}
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

// closeGrace is how long Close waits for the remote half to exit on its own
// after a "shutdown" request before it force-kills the transport.
const closeGrace = 5 * time.Second

// Close shuts the session down cleanly: it asks the remote half to exit and
// waits for the ssh/tsh client to disconnect normally, so the host-side
// session ends immediately (and, for Teleport, the session recording is
// finalized) instead of lingering as "active".
//
// The order matters. We do NOT kill the local client first: a SIGKILL to
// ssh/tsh drops the connection abruptly, which leaves the remote process to be
// reaped by a terminal hangup and the host/Teleport session to linger until a
// keepalive timeout. Instead we send "shutdown", let the remote `myhostmcp
// remote` process return and exit (0), which completes the remote shell/command
// and makes ssh/tsh disconnect cleanly on its own. Only if the remote fails to
// exit within closeGrace do we escalate to closing stdin and killing.
func (s *Session) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
	}
	close(s.closed)

	// Ask the remote half to exit. Ignore errors: if the write fails the
	// connection is already gone and the wait below will return promptly.
	_ = s.Send(&protocol.Request{Type: "shutdown"})

	exited := make(chan struct{})
	go func() {
		if s.cmd != nil {
			_ = s.cmd.Wait() // also closes the stdin pipe once the client exits
		}
		close(exited)
	}()

	select {
	case <-exited:
		// Clean shutdown: the client disconnected normally.
	case <-time.After(closeGrace):
		// The remote did not exit in time. Force it: EOF on stdin may nudge the
		// client out, then cancel the lifecycle context (SIGKILL via
		// CommandContext) and kill the process outright.
		_ = s.stdin.Close()
		if s.cancel != nil {
			s.cancel()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-exited
	}

	if s.cancel != nil {
		s.cancel() // release lifecycle context resources
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
		"-o", "ServerAliveCountMax=4", // ~2 min of missed keepalives → drop
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

var binaryForUnameFn = embed.BinaryForUname

// marker returns a random, per-call sentinel token. It is assembled at runtime
// on the remote via printf, so an echoed command line (which contains the
// printf *format*, not the substituted value) can never contain the full
// token — making marker matching robust even over a PTY before echo is off.
func marker() (token, nonce string) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	nonce = hex.EncodeToString(b[:])
	return "__MHM_" + nonce + "__", nonce
}

// sync drains shell start-up noise (MOTD, banner, the echoed `stty` line for
// tsh) up to a known point, so the first real sh() call sees only its own
// output. It emits a sentinel via printf and reads until that sentinel.
func (s *Session) sync() error {
	tok, nonce := marker()
	if _, err := io.WriteString(s.stdin, "printf '\\n__MHM_%s__\\n' '"+nonce+"'\n"); err != nil {
		return err
	}
	for {
		line, err := s.stdout.ReadString('\n')
		if strings.Contains(line, tok) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sync shell: %w", err)
		}
	}
}

// sh runs one command in the remote shell and returns its stdout and exit
// code. The command's output is bracketed between a begin sentinel and an end
// sentinel (the latter carrying $?): sh discards everything up to the begin
// sentinel — which absorbs an interactive shell's prompt and any bracketed-
// paste escapes emitted before the command — then captures up to the end
// sentinel. This works over both a raw PTY (tsh) and a plain pipe (ssh). The
// sentinels are assembled at runtime by printf, so an echoed command line can
// never contain the full sentinel and cause a false match.
func (s *Session) sh(cmd string) (out string, code int, err error) {
	_, nonce := marker()
	beg := "__MHMBEG_" + nonce + "__"
	end := "__MHMEND_" + nonce + "__"
	full := "printf '\\n__MHMBEG_%s__\\n' '" + nonce + "'; { " + cmd +
		" ; }; printf '\\n__MHMEND_%s__%s__\\n' '" + nonce + "' \"$?\"\n"
	if _, err := io.WriteString(s.stdin, full); err != nil {
		return "", 0, err
	}
	// Phase 1: discard everything up to and including the begin sentinel.
	for {
		line, rerr := s.stdout.ReadString('\n')
		if strings.Contains(line, beg) {
			break
		}
		if rerr != nil {
			return "", 0, fmt.Errorf("shell command %q: %w", cmd, rerr)
		}
	}
	// Phase 2: capture output up to the end sentinel and parse the exit code.
	var b strings.Builder
	for {
		line, rerr := s.stdout.ReadString('\n')
		if idx := strings.Index(line, end); idx >= 0 {
			b.WriteString(line[:idx])
			rest := line[idx+len(end):]
			if e := strings.Index(rest, "__"); e >= 0 {
				code, _ = strconv.Atoi(strings.TrimSpace(rest[:e]))
			}
			return strings.TrimRight(b.String(), "\r\n"), code, nil
		}
		b.WriteString(line)
		if rerr != nil {
			return "", 0, fmt.Errorf("shell command %q: %w", cmd, rerr)
		}
	}
}

// upload writes bin to remotePath on the remote via a base64 heredoc. base64
// keeps the payload to a safe text alphabet (no NULs, no long lines, cannot
// clash with the heredoc delimiter), so it streams cleanly over a raw PTY or a
// pipe. The decoder flag differs by OS: GNU/Linux uses `-d`, BSD/macOS `-D`.
func (s *Session) upload(remotePath, installDir, sysname string, bin []byte) error {
	decode := "-d"
	if sysname == "Darwin" {
		decode = "-D"
	}
	_, nonce := marker()
	delim := "__MHM_DATA_" + nonce + "__"
	enc := base64.StdEncoding.EncodeToString(bin)

	var b strings.Builder
	b.WriteString("mkdir -p " + installDir + " && base64 " + decode + " > " + remotePath + " <<'" + delim + "'\n")
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteByte('\n')
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteByte('\n')
	b.WriteString(delim + "\n")
	if _, err := io.WriteString(s.stdin, b.String()); err != nil {
		return err
	}
	// Finalize + confirm success through the marker protocol.
	if _, code, err := s.sh("chmod +x " + remotePath); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("chmod remote binary: exit %d", code)
	}
	return nil
}

// scpRunner runs the external file-transfer program (scp / tsh scp). It is a
// package var so tests can stub the transfer without touching the network.
var scpRunner = func(ctx context.Context, prog string, args []string) error {
	cmd := exec.CommandContext(ctx, prog, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", prog, err, msg)
		}
		return fmt.Errorf("%s: %w", prog, err)
	}
	return nil
}

// installBinary places the remote binary on the host. It prefers a native file
// transfer (scp / tsh scp), which is fast and robust, and falls back to the
// base64-over-PTY heredoc (see upload) if the transfer fails — e.g. because
// file transfer is disabled by Teleport role policy.
func (s *Session) installBinary(ctx context.Context, opts DialOptions, binary, remotePath, sysname string, bin []byte) error {
	if err := s.uploadViaSCP(ctx, opts, binary, opts.RemoteInstallDir, bin); err == nil {
		return nil
	} else if ferr := s.upload(remotePath, opts.RemoteInstallDir, sysname, bin); ferr != nil {
		return fmt.Errorf("scp transfer failed (%v); base64-over-PTY fallback also failed: %w", err, ferr)
	}
	return nil
}

// uploadViaSCP copies bin to the remote install dir using scp (or tsh scp).
// The install dir is created and resolved to an absolute path over the already-
// open shell first (where ~ expands), because scp — which now uses SFTP under
// the hood — does not reliably expand ~ in the destination path.
func (s *Session) uploadViaSCP(ctx context.Context, opts DialOptions, binary, installDir string, bin []byte) error {
	out, code, err := s.sh("mkdir -p " + installDir + " && cd " + installDir + " && pwd")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("prepare remote install dir: exit %d", code)
	}
	absDir := lastNonEmptyLine(out)
	if !strings.HasPrefix(absDir, "/") {
		return fmt.Errorf("could not resolve remote install dir (got %q)", absDir)
	}
	absPath := absDir + "/myhostmcp"

	tmp, err := os.CreateTemp("", "myhostmcp-remote-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpName, 0o755)

	target := opts.Host + ":" + absPath
	if opts.User != "" {
		target = opts.User + "@" + target
	}

	var prog string
	var args []string
	if binary == "tsh" {
		prog = "tsh"
		args = []string{"scp", "-q"}
		if opts.Port != 0 && opts.Port != 22 {
			args = append(args, "-P", strconv.Itoa(opts.Port))
		}
		args = append(args, tmpName, target)
	} else {
		prog = "scp"
		strict := opts.StrictHostKeyChecking
		if strict == "" {
			strict = "accept-new"
		}
		args = []string{"-B", "-q", "-o", "StrictHostKeyChecking=" + strict}
		if opts.Port != 0 && opts.Port != 22 {
			args = append(args, "-P", strconv.Itoa(opts.Port))
		}
		for _, f := range opts.IdentityFiles {
			if f != "" {
				args = append(args, "-i", f)
			}
		}
		args = append(args, tmpName, target)
	}

	if err := scpRunner(ctx, prog, args); err != nil {
		return err
	}

	if _, code, err := s.sh("chmod +x " + absPath); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("chmod remote binary: exit %d", code)
	}
	return nil
}

// lastNonEmptyLine returns the last non-blank, whitespace-trimmed line of s.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// bringUp performs the whole bootstrap over the single shell opened by
// startShell: detect platform, install/upgrade the binary if needed, then
// hand the shell off to the persistent `myhostmcp remote` process.
func (s *Session) bringUp(ctx context.Context, opts DialOptions, remotePath, binary string) error {
	// Drain start-up noise so the first sh() sees only its own output.
	if err := s.sync(); err != nil {
		return err
	}

	// 1. Detect the remote platform.
	out, code, err := s.sh("uname -sm")
	if err != nil {
		return fmt.Errorf("detect platform: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("detect platform: uname exited %d", code)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return fmt.Errorf("detect platform: unexpected uname output: %q", out)
	}
	sysname, machine := fields[0], fields[1]
	s.Platform = sysname + " " + machine

	// 2. Ensure the remote binary is present and byte-identical to the one we
	//    embed. The decision is driven by the binary's content hash, not the
	//    version tag: dev builds keep the same version string, so a tag compare
	//    would leave a stale remote in place. We fetch the embedded binary up
	//    front so we can both hash it and upload it if needed; the remote reports
	//    the SHA-256 of its own file via `remote --version`. An old remote that
	//    predates hash reporting yields an empty hash, which counts as a mismatch
	//    and triggers a refresh.
	bin, err := binaryForUnameFn(sysname, machine)
	if err != nil {
		return fmt.Errorf("no prebuilt remote binary for %s %s: %w", sysname, machine, err)
	}
	wantHash := sha256Hex(bin)
	check := fmt.Sprintf("if [ -x %s ]; then %s remote --version; else echo __MISSING__; fi", remotePath, remotePath)
	out, _, err = s.sh(check)
	if err != nil {
		return fmt.Errorf("version check: %w", err)
	}
	if parseReportedHash(out) != wantHash {
		if err := s.installBinary(ctx, opts, binary, remotePath, sysname, bin); err != nil {
			return fmt.Errorf("upload: %w", err)
		}
	}

	// 3. Hand the shell off to the persistent process with exec, so it takes
	//    over this same session instead of opening a new one. For tsh the PTY
	//    merges stdout+stderr, so the binary's stderr is redirected to a log
	//    file to keep it out of the JSON stream; for ssh, stderr stays on its
	//    own pipe and flows to s.Stderr().
	handoff := "exec " + remotePath + " remote\n"
	if binary == "tsh" {
		remoteArgs := "remote"
		if opts.RecordingFriendly {
			// Recording-friendly framing is only meaningful on tsh, whose PTY
			// session Teleport records. Enable it and switch recv() to APC mode.
			remoteArgs = "remote --apc"
			s.apc = true
		}
		handoff = "exec " + remotePath + " " + remoteArgs + " 2>>" + opts.RemoteInstallDir + "/remote.log\n"
	}
	if _, err := io.WriteString(s.stdin, handoff); err != nil {
		return fmt.Errorf("handoff: %w", err)
	}
	return nil
}

// startShell opens the single persistent shell used for a whole connection.
// It passes NO command argument to ssh/tsh so exactly one SSH session (and one
// authentication) is created per Dial. bringUp then drives platform detection,
// installation, and the handoff over this shell's stdin/stdout.
//
// tsh: a PTY is requested (-tt) so Teleport records one interactive shell
// session, and the PTY is switched to raw mode so its CRLF/echo processing
// cannot corrupt the JSON framing. ssh: no PTY, so stdout and stderr stay on
// separate pipes and the remote binary's logs flow to s.Stderr().
func startShell(opts DialOptions, binary string) (*Session, error) {
	// The persistent process must outlive the MCP request that initiated it:
	// the SDK cancels the request context once the tool handler returns, which
	// would kill a CommandContext bound to it.  Use a background-derived
	// lifecycle context cancelled on Session.Close instead.
	lctx, cancel := context.WithCancel(context.Background())

	var c *exec.Cmd
	if binary == "tsh" {
		// -tt: force PTY allocation even though our local stdin is a pipe, so
		// Teleport classifies this as an interactive shell session.
		args := append(tshArgs(opts), "-tt", opts.Host)
		c = exec.CommandContext(lctx, "tsh", append([]string{"ssh"}, args...)...)
	} else {
		// No command and no PTY: ssh still requests a shell session, reading
		// our commands from the stdin pipe, with stderr on its own pipe.
		args := append(sshArgs(opts), opts.Host)
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

	if binary == "tsh" {
		// Put the PTY into raw mode before any bootstrap so ONLCR (CRLF) output
		// translation and input echo cannot corrupt the JSON framing, and quiet
		// the interactive shell's prompt so it does not interleave with output.
		// Any remaining banner/echo is drained by sync() and by sh()'s begin
		// sentinel. The escapes are best-effort (2>/dev/null / unset).
		init := "stty raw -echo 2>/dev/null; PS1=''; PROMPT_COMMAND=''; unset PROMPT_COMMAND 2>/dev/null\n"
		if _, err := io.WriteString(stdin, init); err != nil {
			_ = c.Process.Kill()
			cancel()
			return nil, fmt.Errorf("init tsh shell: %w", err)
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

// parseReportedHash extracts the self-hash from `remote --version` output of
// the form "myhostmcp <version> <sha256>". It returns "" if no hash is present
// (an older remote binary, a missing binary, or unexpected output) — which the
// caller treats as a mismatch and refreshes the remote binary.
func parseReportedHash(raw string) string {
	// Scan lines so a leading banner/echo over a PTY doesn't hide the report.
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 3 && fields[0] == "myhostmcp" {
			return fields[2]
		}
	}
	return ""
}

// sha256Hex returns the hex-encoded SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
