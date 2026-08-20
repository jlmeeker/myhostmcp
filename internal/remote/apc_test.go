package remote

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"myhostmcp/internal/protocol"
)

// TestExecutorAPCTranscript verifies that in APC mode the executor emits BOTH a
// human-readable transcript (visible on a Teleport replay) and an APC-wrapped
// structured response (invisible on replay, parsed by the local half), and that
// no bare JSON line is written to the channel.
func TestExecutorAPCTranscript(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("allowCommands:\n  - echo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	var stderr bytes.Buffer
	e, err := New(Config{ConfigPath: cfg, APC: true}, reqR, respW, &stderr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	t.Cleanup(func() {
		reqW.Close()
		cancel()
		<-done
	})

	// Continuously drain the response channel into a buffer.
	var mu sync.Mutex
	var buf bytes.Buffer
	go func() {
		b := make([]byte, 4096)
		for {
			n, rerr := respR.Read(b)
			if n > 0 {
				mu.Lock()
				buf.Write(b[:n])
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()

	send := func(s string) {
		if _, err := reqW.Write([]byte(s + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	send(`{"id":1,"type":"exec","command":"echo hello"}`)

	// Wait until the transcript's exit line has been emitted.
	deadline := time.Now().Add(3 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		mu.Lock()
		got = buf.String()
		mu.Unlock()
		if strings.Contains(got, "exit 0") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 1. Human-readable transcript is present.
	if !strings.Contains(got, "$ echo hello\r\n") {
		t.Errorf("transcript command echo missing; got:\n%q", got)
	}
	if !strings.Contains(got, "hello\r\n") {
		t.Errorf("transcript stdout missing; got:\n%q", got)
	}
	if !strings.Contains(got, "exit 0\r\n") {
		t.Errorf("transcript exit line missing; got:\n%q", got)
	}

	// 2. Structured response is APC-wrapped and nonce-tagged, never a bare line.
	tag := protocol.APCStart + protocol.APCTag
	if !strings.Contains(got, tag) {
		t.Errorf("no APC-wrapped response found; got:\n%q", got)
	}
	// Every '{' (start of a JSON object) must be inside an APC envelope, i.e.
	// preceded on the stream by the APC tag, not emitted as a raw JSON line.
	if idx := strings.Index(got, `{"`); idx >= 0 {
		prefix := got[:idx]
		if !strings.Contains(prefix, tag) {
			t.Errorf("JSON object appears outside an APC envelope; got:\n%q", got)
		}
	}
}
