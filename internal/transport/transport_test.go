package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"myhostmcp/internal/protocol"
	"myhostmcp/internal/version"
)

// canSSHSystem returns true if `ssh 127.0.0.1` works with the default key.
func canSSHSystem() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s, err := Dial(ctx, DialOptions{
		Host:             "127.0.0.1",
		RemoteInstallDir: "~/.myhostmcp-transport-test",
		ConnectTimeout:   8 * time.Second,
	})
	if err != nil {
		return false
	}
	_ = s.Close()
	return true
}

// TestTransportPersistent verifies that an SSH session stays alive after Dial
// returns (the lifecycle-context fix) and that request/response framing works
// over the real transport. The remote enforces its own allowlist
// (/etc/myhostmcp/config.yaml, defaulting to df/ss/top/free/ls), so this test
// is robust to whatever the host is configured to allow: it queries the
// allowlist first, runs an allowed command, and confirms a disallowed one is
// rejected by the remote.
func TestTransportPersistent(t *testing.T) {
	if !canSSHSystem() {
		t.Skip("system sshd on 127.0.0.1:22 not reachable with default key; skipping")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := Dial(ctx, DialOptions{
		Host:             "127.0.0.1",
		RemoteInstallDir: "~/.myhostmcp-transport-test",
		ConnectTimeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Logf("connected: pid=%d platform=%q version=%q", s.RemotePID, s.Platform, s.RemoteVersion)
	defer s.Close()

	// Drain stderr in the background (as the local half does) so it can't
	// block. We don't assert on it.
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := s.Stderr().Read(buf); err != nil {
				return
			}
		}
	}()

	// The crucial assertion: after Dial returns, the request context above is
	// still alive, but even if it were cancelled the session must persist
	// (lifecycle context). Wait a moment, then confirm the session still
	// answers.
	time.Sleep(500 * time.Millisecond)

	// 1. Query the remote's enforced allowlist (must be non-empty; the remote
	//    is never unrestricted).
	if err := s.Send(&protocol.Request{ID: 1, Type: "allowed_commands"}); err != nil {
		t.Fatalf("Send allowed_commands: %v", err)
	}
	r, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv allowed_commands: %v", err)
	}
	if r.Type != "allowlist" {
		t.Fatalf("allowed_commands: expected allowlist, got %+v", r)
	}
	if len(r.AllowCommands) == 0 {
		t.Fatalf("allowlist should be non-empty; the remote is never unrestricted")
	}
	t.Logf("remote enforces %d command(s): %v", len(r.AllowCommands), r.AllowCommands)

	// 2. Run an allowed command (pick a safe one from the list, with a short
	//    timeout in case it is something interactive like `top`).
	if cmd := safeAllowedCommand(r.AllowCommands); cmd != "" {
		if err := s.Send(&protocol.Request{ID: 2, Type: "exec", Command: cmd, TimeoutMs: 5000}); err != nil {
			t.Fatalf("Send exec: %v", err)
		}
		r, err = s.Recv()
		if err != nil {
			t.Fatalf("Recv exec: %v", err)
		}
		if r.Type != "result" {
			t.Fatalf("allowed exec %q: expected result, got %+v", cmd, r)
		}
		t.Logf("allowed exec %q -> exit=%d", cmd, r.ExitCode)
	} else {
		t.Logf("no safe allowed command in the list; skipping positive-exec check")
	}

	// 3. A command that is certainly not allowed is rejected by the remote
	//    (the remote has the final say). `rm -rf /` is disallowed by every
	//    built-in default and is never in a sane allowlist.
	if err := s.Send(&protocol.Request{ID: 3, Type: "exec", Command: "rm -rf /"}); err != nil {
		t.Fatalf("Send rm: %v", err)
	}
	r, err = s.Recv()
	if err != nil {
		t.Fatalf("Recv rm: %v", err)
	}
	if r.Type != "error" || !strings.Contains(r.Error, "allowlist") {
		t.Fatalf("rm -rf / should be rejected by the remote allowlist, got %+v", r)
	}

	// 4. The session is still alive after the rejection: another query works.
	if err := s.Send(&protocol.Request{ID: 4, Type: "allowed_commands"}); err != nil {
		t.Fatalf("Send after rejection: %v", err)
	}
	if _, err := s.Recv(); err != nil {
		t.Fatalf("Recv after rejection: %v", err)
	}
}

// safeAllowedCommand returns a harmless invocation of an allowed entry, or ""
// if none of the allowed entries are known-safe to run (e.g. only `top`).
// fakeShell emulates just enough of a remote shell to exercise the marker
// protocol used by bringUp: it reads the sentinel-carrying command lines the
// Session writes, extracts the per-call nonce, and replies with canned output
// followed by the matching `__MHM_<nonce>__<code>__END__` (or the bare sync
// sentinel). It records which logical commands it saw and whether a base64
// upload heredoc was streamed.
type fakeShell struct {
	in  *bufio.Reader // commands written by the Session
	out io.Writer     // shell stdout the Session reads

	sawUpload bool
	commands  []string
}

var (
	syncRe = regexp.MustCompile(`^printf '\\n__MHM_%s__\\n' '([0-9a-f]+)'`)
	shRe   = regexp.MustCompile(`^printf '\\n__MHMBEG_%s__\\n' '([0-9a-f]+)'; \{ (.*) ; \}; printf '\\n__MHMEND_%s__%s__\\n'`)
)

// run drives the fake shell until it sees the exec handoff line (or EOF).
func (f *fakeShell) run(handler func(cmd string) (string, int)) {
	for {
		line, err := f.in.ReadString('\n')
		if line != "" {
			f.dispatch(strings.TrimRight(line, "\n"), handler)
			if strings.HasPrefix(strings.TrimSpace(line), "exec ") {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *fakeShell) dispatch(line string, handler func(cmd string) (string, int)) {
	if m := syncRe.FindStringSubmatch(line); m != nil {
		fmt.Fprintf(f.out, "\n__MHM_%s__\n", m[1])
		return
	}
	if m := shRe.FindStringSubmatch(line); m != nil {
		nonce, cmd := m[1], m[2]
		f.commands = append(f.commands, cmd)
		out, code := handler(cmd)
		fmt.Fprintf(f.out, "\n__MHMBEG_%s__\n", nonce)
		if out != "" {
			fmt.Fprint(f.out, out)
		}
		fmt.Fprintf(f.out, "\n__MHMEND_%s__%s__\n", nonce, strconv.Itoa(code))
		return
	}
	if strings.Contains(line, "base64") && strings.Contains(line, "<<'__MHM_DATA_") {
		f.sawUpload = true
	}
}

// newFakeSession wires a Session's stdin/stdout to an in-memory fake shell and
// returns the session plus a channel that closes when the fake shell's run loop
// exits (i.e. after it sees the exec handoff).
func newFakeSession(t *testing.T, handler func(cmd string) (string, int)) (*Session, *fakeShell, <-chan struct{}) {
	t.Helper()
	cmdR, cmdW := io.Pipe() // Session writes commands → fake shell reads
	outR, outW := io.Pipe() // fake shell writes output → Session reads
	fsh := &fakeShell{in: bufio.NewReader(cmdR), out: outW}
	s := &Session{
		stdin:   cmdW,
		stdout:  bufio.NewReaderSize(outR, 64*1024),
		writeMu: make(chan struct{}, 1),
		rpcMu:   make(chan struct{}, 1),
		closed:  make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		fsh.run(handler)
		close(done)
	}()
	return s, fsh, done
}

func TestBringUpSkipsUploadWhenHashMatches(t *testing.T) {
	origBinary := binaryForUnameFn
	t.Cleanup(func() { binaryForUnameFn = origBinary })
	fakeBin := []byte("fake-remote-binary-bytes-v1")
	binaryForUnameFn = func(_, _ string) ([]byte, error) { return fakeBin, nil }
	wantHash := sha256Hex(fakeBin)

	origSCP := scpRunner
	t.Cleanup(func() { scpRunner = origSCP })
	scpRunner = func(context.Context, string, []string) error {
		t.Fatalf("scp must not run when the remote hash matches")
		return nil
	}

	s, fsh, done := newFakeSession(t, func(cmd string) (string, int) {
		switch {
		case cmd == "uname -sm":
			return "Linux x86_64\n", 0
		case strings.Contains(cmd, "remote --version"):
			// Remote reports the SAME hash -> no upload expected.
			return "myhostmcp " + version.Version + " " + wantHash + "\n", 0
		default:
			return "", 0
		}
	})

	if err := s.bringUp(context.Background(), DialOptions{RemoteInstallDir: "~/.myhostmcp"}, "~/.myhostmcp/myhostmcp", "ssh"); err != nil {
		t.Fatalf("bringUp: %v", err)
	}
	<-done

	if s.Platform != "Linux x86_64" {
		t.Fatalf("platform = %q, want %q", s.Platform, "Linux x86_64")
	}
	if fsh.sawUpload {
		t.Fatalf("unexpected upload when hashes match")
	}
}

func TestBringUpUploadsViaSCPWhenHashDiffers(t *testing.T) {
	origBinary := binaryForUnameFn
	t.Cleanup(func() { binaryForUnameFn = origBinary })
	binaryForUnameFn = func(_, _ string) ([]byte, error) { return []byte("new-bytes"), nil }

	origSCP := scpRunner
	t.Cleanup(func() { scpRunner = origSCP })
	var scpProg string
	var scpArgs []string
	scpRunner = func(_ context.Context, prog string, args []string) error {
		scpProg, scpArgs = prog, args
		return nil // simulate a successful transfer
	}

	s, fsh, done := newFakeSession(t, func(cmd string) (string, int) {
		switch {
		case cmd == "uname -sm":
			return "Linux x86_64\n", 0
		case strings.Contains(cmd, "remote --version"):
			// Same version tag, but a DIFFERENT hash -> must re-upload.
			return "myhostmcp " + version.Version + " " + sha256Hex([]byte("old-bytes")) + "\n", 0
		case strings.Contains(cmd, "&& pwd"):
			return "/home/tester/.myhostmcp\n", 0
		default: // chmod after upload
			return "", 0
		}
	})

	if err := s.bringUp(context.Background(), DialOptions{RemoteInstallDir: "~/.myhostmcp", Host: "h", User: "u"}, "~/.myhostmcp/myhostmcp", "tsh"); err != nil {
		t.Fatalf("bringUp: %v", err)
	}
	<-done

	if scpProg != "tsh" {
		t.Fatalf("expected transfer via tsh scp, got prog=%q", scpProg)
	}
	if len(scpArgs) < 2 || scpArgs[len(scpArgs)-1] != "u@h:/home/tester/.myhostmcp/myhostmcp" {
		t.Fatalf("unexpected scp args (destination): %v", scpArgs)
	}
	if fsh.sawUpload {
		t.Fatalf("heredoc fallback should not run when scp succeeds")
	}
}

func TestBringUpFallsBackToHeredocWhenSCPFails(t *testing.T) {
	origBinary := binaryForUnameFn
	t.Cleanup(func() { binaryForUnameFn = origBinary })
	binaryForUnameFn = func(_, _ string) ([]byte, error) { return []byte("fake-binary-bytes"), nil }

	origSCP := scpRunner
	t.Cleanup(func() { scpRunner = origSCP })
	scpRunner = func(context.Context, string, []string) error {
		return fmt.Errorf("file transfer disabled by policy") // force fallback
	}

	s, fsh, done := newFakeSession(t, func(cmd string) (string, int) {
		switch {
		case cmd == "uname -sm":
			return "Linux x86_64\n", 0
		case strings.Contains(cmd, "remote --version"):
			return "__MISSING__\n", 0
		case strings.Contains(cmd, "&& pwd"):
			return "/home/tester/.myhostmcp\n", 0
		default: // chmod after upload
			return "", 0
		}
	})

	if err := s.bringUp(context.Background(), DialOptions{RemoteInstallDir: "~/.myhostmcp", Host: "h"}, "~/.myhostmcp/myhostmcp", "tsh"); err != nil {
		t.Fatalf("bringUp: %v", err)
	}
	<-done

	if !fsh.sawUpload {
		t.Fatalf("expected base64-over-PTY heredoc fallback when scp fails")
	}
}

func TestParseReportedVersion(t *testing.T) {
	cases := map[string]string{
		"myhostmcp 1.2.3\n": "1.2.3",
		"1.2.3":             "1.2.3",
		"":                  "",
	}
	for in, want := range cases {
		if got := parseReportedVersion(in); got != want {
			t.Fatalf("parseReportedVersion(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseReportedHash(t *testing.T) {
	cases := map[string]string{
		"myhostmcp 0.2.0-dev abc123\n":            "abc123",
		"banner line\nmyhostmcp 1.0.0 deadbeef\n": "deadbeef", // hash on a later line
		"myhostmcp 0.2.0-dev\n":                   "",         // old binary: no hash
		"__MISSING__\n":                           "",
		"":                                        "",
	}
	for in, want := range cases {
		if got := parseReportedHash(in); got != want {
			t.Fatalf("parseReportedHash(%q)=%q want %q", in, got, want)
		}
	}
}

func safeAllowedCommand(allow [][]string) string {
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
	for _, toks := range allow {
		if len(toks) == 0 {
			continue
		}
		if cmd, ok := safe[toks[0]]; ok {
			return cmd
		}
	}
	return ""
}

func TestParseResponseLine(t *testing.T) {
	ready := `{"type":"log","msg":"ready","version":"0.2.0-dev","pid":123,"hasTimeout":true}`

	cases := []struct {
		name    string
		line    string
		wantOK  bool
		wantMsg string
	}{
		{"clean", ready, true, "ready"},
		{"leading banner", "All actions are being monitored" + ready, true, "ready"},
		{"leading banner with brace", "note {monitored} here " + ready, true, "ready"},
		{"trailing noise", ready + "  # prompt$", true, "ready"},
		{"pure banner", "-------------------------------- All actions are being monitored", false, ""},
		{"empty", "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, ok := parseResponseLine(c.line)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (line=%q)", ok, c.wantOK, c.line)
			}
			if ok && resp.Msg != c.wantMsg {
				t.Fatalf("msg = %q, want %q", resp.Msg, c.wantMsg)
			}
		})
	}
}
