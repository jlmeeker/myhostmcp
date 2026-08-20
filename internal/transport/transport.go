// Package transport connects the local half to a remote host over SSH and
// speaks the private protocol (see package protocol) over the SSH channel.
//
// It drives the real ssh binary via os/exec, so authentication uses the user's
// normal SSH setup: ~/.ssh/config, default keys, ssh-agent, bastions, etc.
// This package stores NO credentials or hosts of its own.
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

// DialOptions describes how to reach a remote host.
type DialOptions struct {
	Host                  string        // SSH host (as in ~/.ssh/config); required
	User                  string        // remote user; "" = ssh default
	Port                  int           // 0 or 22 = default
	IdentityFiles         []string      // optional -i paths; nil = ssh defaults
	RemoteInstallDir      string        // e.g. "~/.myhostmcp"; must be safe-path
	ConnectTimeout        time.Duration // for ssh -o ConnectTimeout and overall dial
	StrictHostKeyChecking string        // ssh StrictHostKeyChecking value; default "accept-new"
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

	writeMu chan struct{}
	rpcMu   chan struct{}
	closed  chan struct{}
	cancel  context.CancelFunc
}

// Dial connects to the host, ensures the remote half is installed and
// up-to-date, starts it, and waits for its "ready" announcement.
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

	// 1. Detect the remote platform.
	sysname, machine, err := detectPlatform(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("detect platform: %w", err)
	}

	// 2. Ensure the remote binary is present and version-current.
	remotePath := opts.RemoteInstallDir + "/myhostmcp"
	if err := ensureInstalled(ctx, opts, remotePath, sysname, machine); err != nil {
		return nil, fmt.Errorf("install remote: %w", err)
	}

	// 3. Start the remote half as a persistent SSH session.
	s, err := startRemote(ctx, opts, remotePath)
	if err != nil {
		return nil, fmt.Errorf("start remote: %w", err)
	}
	s.Platform = sysname + " " + machine

	// 4. Wait for the "ready" announcement on the protocol channel.
	ready, err := s.recv()
	if err != nil {
		s.kill()
		return nil, fmt.Errorf("read ready: %w", err)
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
		return fmt.Errorf("write to ssh: %w", err)
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
	line, err := s.stdout.ReadString('\n')
	if err != nil && line == "" {
		return nil, fmt.Errorf("read from ssh: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, io.EOF
	}
	var resp protocol.Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w (line=%q)", err, truncate(line, 200))
	}
	return &resp, nil
}

// Stderr returns a reader for the remote's diagnostic output (ssh + remote
// stderr). The caller should drain it (e.g. copy to a log file) to avoid
// blocking.
func (s *Session) Stderr() io.ReadCloser { return s.stderr }

// Close sends a shutdown request, closes stdin, and waits for ssh to exit.
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

// sshArgs builds the common ssh argument list (everything before the host).
func sshArgs(opts DialOptions) []string {
	strict := opts.StrictHostKeyChecking
	if strict == "" {
		strict = "accept-new" // auto-add new hosts, reject changed
	}
	a := []string{
		"-o", "BatchMode=yes", // never prompt; fail instead (non-interactive)
		"-o", "StrictHostKeyChecking=" + strict,
		"-o", "ServerAliveInterval=30", // keep long sessions alive
		"-o", "ServerAliveCountMax=4", // ~2min of missed keepalives => drop
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

// runOnce runs a one-shot ssh command and returns its stdout. stderr is
// surfaced in the error if the command fails.
func runOnce(ctx context.Context, opts DialOptions, remoteCmd string, stdin io.Reader) ([]byte, error) {
	args := append(sshArgs(opts), opts.Host, remoteCmd)
	c := exec.CommandContext(ctx, "ssh", args...)
	if stdin != nil {
		c.Stdin = stdin
	}
	var out, errOut bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errOut
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("ssh %s: %w; stderr: %s", opts.Host, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

func detectPlatform(ctx context.Context, opts DialOptions) (sysname, machine string, err error) {
	out, err := runOnce(ctx, opts, "uname -sm", nil)
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

func ensureInstalled(ctx context.Context, opts DialOptions, remotePath, sysname, machine string) error {
	// Check the installed version (if any) with a single one-shot command.
	checkCmd := fmt.Sprintf(
		`if [ -x %s ]; then %s remote --version; else echo __MISSING__; fi`,
		remotePath, remotePath)
	out, err := runOnceFn(ctx, opts, checkCmd, nil)
	if err != nil {
		// Non-fatal: some ssh configs warn on stderr but still succeed; treat
		// a hard failure as "needs install" and try uploading.
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
	if _, err := runOnceFn(ctx, opts, uploadCmd, bytes.NewReader(bin)); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

func startRemote(_ context.Context, opts DialOptions, remotePath string) (*Session, error) {
	// The persistent ssh process must outlive the MCP request that initiated
	// it: the SDK cancels the request context once the tool handler returns,
	// which would kill a CommandContext bound to it. Use a background-derived
	// lifecycle context that is cancelled on Session.Close instead.
	lctx, cancel := context.WithCancel(context.Background())
	args := append(sshArgs(opts), opts.Host, remotePath+" remote")
	c := exec.CommandContext(lctx, "ssh", args...)
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
		return nil, fmt.Errorf("start ssh: %w", err)
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
