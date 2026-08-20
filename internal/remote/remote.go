// Package remote implements the remote half of myhostmcp: a process that owns
// one persistent shell on a remote host and runs commands in it on demand,
// returning captured stdout, stderr, exit code and current working directory.
//
// It reads protocol.Request lines from an io.Reader (the SSH channel's stdin)
// and writes protocol.Response lines to an io.Writer (the SSH channel's
// stdout). Its own diagnostics go to a separate logger (stderr), never to the
// protocol channel.
package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"myhostmcp/internal/allowlist"
	"myhostmcp/internal/protocol"
	"myhostmcp/internal/remoteconfig"
	"myhostmcp/internal/version"
)

var (
	sentStart = []byte("\x01\x02EXIT:") // sentinel begin
	sentEnd   = []byte("\x03\x04\n")    // sentinel end
)

// Config configures the remote executor. The zero value is usable.
//
// The command allowlist is NOT set here: it is loaded by New from the remote
// config file (ConfigPath, default /etc/myhostmcp/config.yaml — see package
// remoteconfig) and is immutable for the life of the executor.
type Config struct {
	ConfigPath string   // path to the remote config file; "" = /etc/myhostmcp/config.yaml
	Shell      string   // shell binary, default "bash"
	ShellArgs  []string // shell args, default ["--noprofile","--norc"]
}

// Executor is the remote half.
type Executor struct {
	cfg  Config
	in   *bufio.Reader
	out  io.Writer
	logf *log.Logger

	shell           *exec.Cmd
	shellIn         io.WriteCloser
	stdoutCh        chan []byte
	stdoutDone      chan struct{}
	stderrCh        chan []byte
	stderrDone      chan struct{}
	stderrRemainder []byte
	cwd             string
	pid             int
	hasTimeout      bool // timeout(1) is available on the host

	allow [][]string // immutable after New; loaded from the remote config file
}

// New creates an executor that reads requests from r and writes responses to w.
// Diagnostics go to errOut, which must NOT be the protocol channel (w). It loads
// the enforced allowlist from the remote config file (cfg.ConfigPath, or
// /etc/myhostmcp/config.yaml by default); a missing file is fine (the safe
// default allowlist is used), but a malformed file is a hard error.
func New(cfg Config, r io.Reader, w, errOut io.Writer) (*Executor, error) {
	if cfg.Shell == "" {
		cfg.Shell = "bash"
	}
	if cfg.ShellArgs == nil {
		cfg.ShellArgs = []string{"--noprofile", "--norc"}
	}
	rc, err := remoteconfig.Load(cfg.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load remote config: %w", err)
	}
	e := &Executor{
		cfg:        cfg,
		in:         bufio.NewReader(r),
		out:        w,
		logf:       log.New(errOut, "remote: ", log.LstdFlags|log.Lmicroseconds),
		stdoutCh:   make(chan []byte, 1),
		stdoutDone: make(chan struct{}),
		stderrCh:   make(chan []byte, 1),
		stderrDone: make(chan struct{}),
		allow:      rc.AllowCommands,
	}
	e.logf.Printf("allowlist: %d entries (from %s)", len(e.allow), rc.Path)
	return e, nil
}

// Run starts the shell and serves requests until EOF, a "shutdown" request, or
// ctx cancellation. It always cleans up the shell before returning.
func (e *Executor) Run(ctx context.Context) error {
	if err := e.startShell(ctx); err != nil {
		return err
	}
	defer e.cleanup()

	if !e.writeResp(protocol.Response{
		Type:       "log",
		Msg:        "ready",
		Version:    version.Version,
		PID:        e.pid,
		HasTimeout: e.hasTimeout,
	}) {
		return fmt.Errorf("protocol write failed")
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, rerr := e.in.ReadString('\n')
		if rerr != nil && line == "" {
			if rerr == io.EOF {
				e.logf.Printf("stdin EOF; exiting")
			} else {
				e.logf.Printf("stdin read error: %v", rerr)
			}
			return nil
		}
		line = strings.TrimSpace(line)
		if line != "" {
			var req protocol.Request
			if jerr := json.Unmarshal([]byte(line), &req); jerr != nil {
				if !e.writeResp(protocol.Response{Type: "error", Error: "invalid request: " + jerr.Error()}) {
					return fmt.Errorf("protocol write failed")
				}
			} else {
				switch req.Type {
				case "exec":
					if !e.writeResp(e.exec(req)) {
						return fmt.Errorf("protocol write failed")
					}
				case "allowed_commands":
					if !e.writeResp(protocol.Response{ID: req.ID, Type: "allowlist", AllowCommands: e.allow}) {
						return fmt.Errorf("protocol write failed")
					}
				case "shutdown":
					if !e.writeResp(protocol.Response{ID: req.ID, Type: "log", Msg: "bye"}) {
						return fmt.Errorf("protocol write failed")
					}
					e.logf.Printf("shutdown received; exiting")
					return nil
				default:
					if !e.writeResp(protocol.Response{ID: req.ID, Type: "error", Error: "unknown request type: " + req.Type}) {
						return fmt.Errorf("protocol write failed")
					}
				}
			}
		}
		if rerr != nil {
			return nil
		}
	}
}

func (e *Executor) startShell(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, e.cfg.Shell, e.cfg.ShellArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start shell %q: %w", e.cfg.Shell, err)
	}
	e.shell = cmd
	e.shellIn = stdin
	e.pid = cmd.Process.Pid

	go e.stdoutPump(stdout)
	go e.stderrPump(stderr)

	// Detect GNU timeout(1) for enforced command timeouts.
	if _, err := exec.LookPath("timeout"); err == nil {
		if out, err := exec.Command("timeout", "--version").CombinedOutput(); err == nil &&
			strings.Contains(string(out), "timeout") {
			e.hasTimeout = true
		}
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		e.cwd = home
	} else {
		e.cwd = "/"
	}
	return nil
}

func (e *Executor) stdoutPump(r io.Reader) {
	defer close(e.stdoutDone)
	defer close(e.stdoutCh)
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			e.stdoutCh <- chunk
		}
		if err != nil {
			return
		}
	}
}

func (e *Executor) stderrPump(r io.Reader) {
	defer close(e.stderrDone)
	defer close(e.stderrCh)
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			e.stderrCh <- chunk
		}
		if err != nil {
			return
		}
	}
}

// exec runs one command in the persistent shell and returns its result.
func (e *Executor) exec(req protocol.Request) protocol.Response {
	start := time.Now()
	resp := protocol.Response{ID: req.ID, Type: "result"}

	cmd := strings.TrimSpace(req.Command)
	if cmd == "" {
		resp.Type = "error"
		resp.Error = "empty command"
		return resp
	}

	if err := allowlist.Validate(cmd, e.allow); err != nil {
		resp.Type = "error"
		resp.Error = "rejected by allowlist: " + err.Error()
		return resp
	}

	if req.PTY {
		// Phase 2: throwaway pty execution inheriting the tracked cwd.
		resp.Type = "error"
		resp.Error = "pty mode not implemented in Phase 0 (planned for Phase 2)"
		return resp
	}

	// Build the shell script. We wrap the command in a { ...; } group with its
	// stdin redirected from /dev/null so that commands which read stdin (cat,
	// tee, ...) cannot consume the sentinel line that follows. The group runs
	// in the current shell (not a subshell), so cd/export/alias persist.
	var b strings.Builder
	b.WriteString("{ ")
	if req.CWD != "" {
		b.WriteString("cd -- ")
		b.WriteString(shellQuote(req.CWD))
		b.WriteString(" && ")
	}
	// Wrap with `timeout` only for non-builtin commands. Builtins (cd, export,
	// alias, source, ...) must run directly in the current shell so their
	// state changes persist, and most have no external binary anyway (so
	// `timeout cd` would fail with exit 127). Builtins rely on the executor's
	// deadline backstop instead.
	useTimeout := req.TimeoutMs > 0 && e.hasTimeout && !isBuiltinCommand(cmd)
	if useTimeout {
		fmt.Fprintf(&b, "timeout -k 2 %.3f ", float64(req.TimeoutMs)/1000.0)
	}
	b.WriteString(cmd)
	b.WriteString(" ; } </dev/null\n")
	// Sentinel: written after the command, on stdout, so the executor can find
	// the exact end of the command's output and its exit code + cwd.
	b.WriteString("printf '\\x01\\x02EXIT:%d|CWD:%s\\x03\\x04\\n' \"$?\" \"$PWD\"\n")

	errToken := fmt.Sprintf("%d-%d", req.ID, time.Now().UnixNano())
	fmt.Fprintf(&b, "printf '\\x01\\x02ERRDONE:%s\\x03\\x04\\n' >&2\n", errToken)
	errMarker := []byte("\x01\x02ERRDONE:" + errToken + "\x03\x04\n")

	if _, err := e.shellIn.Write([]byte(b.String())); err != nil {
		resp.Type = "error"
		resp.Error = "write to shell failed: " + err.Error()
		e.killShell()
		return resp
	}

	// Deadline/backstop. With timeout(1) the sentinel normally arrives at
	// ~timeout (exit 124); the backstop is timeout + grace. Without timeout(1)
	// the backstop is our only enforcement and we must kill the shell on it.
	hasDeadline := false
	killOnDeadline := false
	var deadline time.Time
	if req.TimeoutMs > 0 {
		hasDeadline = true
		killOnDeadline = true
		if e.hasTimeout {
			deadline = time.Now().Add(time.Duration(req.TimeoutMs)*time.Millisecond + 5*time.Second)
		} else {
			deadline = time.Now().Add(time.Duration(req.TimeoutMs)*time.Millisecond + 2*time.Second)
		}
	}

	stdout, exitCode, cwd, deadlineExceeded, rerr := e.readUntilSentinel(hasDeadline, deadline, killOnDeadline)
	if rerr != nil {
		resp.Type = "error"
		resp.Error = "read result failed: " + rerr.Error()
		e.killShell()
		return resp
	}
	resp.Stdout = string(stdout)
	resp.ExitCode = exitCode
	if cwd != "" {
		e.cwd = cwd
		resp.CWD = cwd
	} else {
		resp.CWD = e.cwd
	}

	switch {
	case deadlineExceeded:
		resp.TimedOut = true
		if exitCode < 0 {
			resp.ExitCode = -1
		}
	case req.TimeoutMs > 0 && e.hasTimeout && (exitCode == 124 || exitCode == 137):
		resp.TimedOut = true
	}

	resp.Stderr = string(e.readStderrUntil(errMarker, 2*time.Second))

	resp.DurationMs = time.Since(start).Milliseconds()
	return resp
}

// readUntilSentinel accumulates shell stdout until it finds the sentinel. If
// hasDeadline is true and the deadline passes first, it returns the partial
// stdout with deadlineExceeded=true. If killOnDeadline is true, it also kills
// the shell (used when the remote lacks timeout(1), or as a safety backstop).
func (e *Executor) readUntilSentinel(hasDeadline bool, deadline time.Time, killOnDeadline bool) (stdout []byte, exitCode int, cwd string, deadlineExceeded bool, err error) {
	buf := &bytes.Buffer{}
	var timer *time.Timer
	var timerC <-chan time.Time
	if hasDeadline {
		timer = time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timerC = timer.C
	}
	for {
		if i := bytes.Index(buf.Bytes(), sentStart); i >= 0 {
			if j := bytes.Index(buf.Bytes()[i:], sentEnd); j >= 0 {
				end := i + j + len(sentEnd)
				exitCode, cwd = parseSentinel(buf.Bytes()[i:end])
				stdout = append([]byte{}, buf.Bytes()[:i]...)
				return stdout, exitCode, cwd, false, nil
			}
		}
		select {
		case chunk, ok := <-e.stdoutCh:
			if !ok {
				return buf.Bytes(), -1, e.cwd, false, io.EOF
			}
			buf.Write(chunk)
		case <-timerC:
			if killOnDeadline {
				e.killShell()
			}
			return buf.Bytes(), -1, e.cwd, true, nil
		}
	}
}

// parseSentinel decodes "\x01\x02EXIT:<code>|CWD:<path>\x03\x04\n".
func parseSentinel(s []byte) (exitCode int, cwd string) {
	body := s[len(sentStart) : len(s)-len(sentEnd)] // "<code>|CWD:<path>"
	idx := bytes.IndexByte(body, '|')
	if idx < 0 {
		return -1, ""
	}
	codeStr := body[:idx]
	cwdStr := body[idx+1:]
	const cwdPrefix = "CWD:"
	if bytes.HasPrefix(cwdStr, []byte(cwdPrefix)) {
		cwdStr = cwdStr[len(cwdPrefix):]
	}
	code, err := strconv.Atoi(string(codeStr))
	if err != nil {
		code = -1
	}
	return code, string(cwdStr)
}

func (e *Executor) writeResp(r protocol.Response) bool {
	b, err := json.Marshal(r)
	if err != nil {
		e.logf.Printf("marshal response failed: %v", err)
		return false
	}
	b = append(b, '\n')
	if _, err := e.out.Write(b); err != nil {
		e.logf.Printf("protocol write failed: %v", err)
		return false
	}
	return true
}

func (e *Executor) killShell() {
	if e.shell != nil && e.shell.Process != nil {
		_ = e.shell.Process.Kill()
	}
}

func (e *Executor) readStderrUntil(marker []byte, maxWait time.Duration) []byte {
	buf := &bytes.Buffer{}
	if len(e.stderrRemainder) > 0 {
		buf.Write(e.stderrRemainder)
		e.stderrRemainder = nil
	}
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	for {
		all := buf.Bytes()
		if i := bytes.Index(all, marker); i >= 0 {
			out := append([]byte{}, all[:i]...)
			tail := all[i+len(marker):]
			if len(tail) > 0 {
				e.stderrRemainder = append([]byte{}, tail...)
			}
			return out
		}
		select {
		case chunk, ok := <-e.stderrCh:
			if !ok {
				return append([]byte{}, buf.Bytes()...)
			}
			buf.Write(chunk)
		case <-timer.C:
			return append([]byte{}, buf.Bytes()...)
		}
	}
}

func (e *Executor) cleanup() {
	if e.shellIn != nil {
		_ = e.shellIn.Close()
	}
	e.killShell()
	<-e.stdoutDone
	<-e.stderrDone
	if e.shell != nil {
		_ = e.shell.Wait()
	}
}

// shellQuote single-quotes s for safe inclusion in a shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
