package transport

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"myhostmcp/internal/protocol"
)

// Run with: MHM_LIVE_HOST=se-ov38.vivintsky.com go test ./internal/transport -run TestLiveTsh -v
func TestLiveTsh(t *testing.T) {
	host := os.Getenv("MHM_LIVE_HOST")
	if host == "" {
		t.Skip("set MHM_LIVE_HOST to run the live tsh test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := Dial(ctx, DialOptions{
		Host:             host,
		User:             os.Getenv("MHM_LIVE_USER"),
		RemoteInstallDir: "~/.myhostmcp",
		ConnectTimeout:   30 * time.Second,
		TransportBinary:  TransportTsh,
		TeleportProxy:    os.Getenv("MHM_LIVE_PROXY"),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Logf("connected transport=%s platform=%q version=%q pid=%d", s.Transport, s.Platform, s.RemoteVersion, s.RemotePID)
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, e := s.Stderr().Read(buf); e != nil {
				return
			}
		}
	}()
	r, err := s.RoundTrip(&protocol.Request{ID: 1, Type: "allowed_commands"})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	t.Logf("allowlist: %v", r.AllowCommands)

	host0 := strings.SplitN(host, ".", 2)[0]
	before := countActiveSessions(t, host0)
	t.Logf("active sessions on %s BEFORE close: %d", host0, before)

	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Logf("Close() returned in %s", time.Since(start))

	// Give Teleport a moment to finalize the session.end event.
	time.Sleep(3 * time.Second)
	after := countActiveSessions(t, host0)
	t.Logf("active sessions on %s AFTER close: %d", host0, after)
	if before > 0 && after >= before {
		t.Errorf("expected active sessions on %s to drop after Close (before=%d after=%d): session lingered", host0, before, after)
	}
}

// countActiveSessions returns how many active Teleport sessions target host.
func countActiveSessions(t *testing.T, host string) int {
	t.Helper()
	out, err := exec.Command("tsh", "sessions", "ls").CombinedOutput()
	if err != nil {
		t.Logf("tsh sessions ls: %v\n%s", err, out)
		return -1
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, host) {
			n++
		}
	}
	return n
}
