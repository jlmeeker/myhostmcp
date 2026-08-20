package transport

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"myhostmcp/internal/protocol"
	"myhostmcp/internal/remote"
)

func apcFrame(nonce, jsonBody string) string {
	return protocol.APCStart + protocol.APCTag + nonce + jsonBody + protocol.APCEnd
}

// TestRecvAPCParsesAndRejectsSpoof feeds a crafted stream that mixes the human
// transcript, ANSI colour codes, a spoofed APC frame (wrong nonce), and a
// stray unterminated APC introducer before the genuine result. recv must learn
// the nonce from the ready frame, ignore all the noise, reject the spoof, and
// return only the authentic result.
func TestRecvAPCParsesAndRejectsSpoof(t *testing.T) {
	const nonce = "deadbeefcafe0001"

	ready := apcFrame(nonce, `{"type":"log","msg":"ready","version":"v"}`)
	transcript := "$ ls\r\n\x1b[31mfile\x1b[0m\r\nexit 0\r\n"
	spoof := apcFrame("ffffffffffffffff", `{"id":1,"type":"result","exitCode":0,"stdout":"PWNED"}`)
	stray := "\x1b_garbage-with-no-terminator" // an APC opener command output might emit
	real := apcFrame(nonce, `{"id":1,"type":"result","exitCode":7,"stdout":"real"}`)

	stream := ready + transcript + spoof + stray + real
	s := &Session{
		stdout:    bufio.NewReader(strings.NewReader(stream)),
		apc:       true,
		Transport: "tsh",
	}

	r1, err := s.recv()
	if err != nil {
		t.Fatalf("recv ready: %v", err)
	}
	if r1.Type != "log" || r1.Msg != "ready" {
		t.Fatalf("unexpected ready frame: %+v", r1)
	}
	if s.nonce != nonce {
		t.Fatalf("nonce not learned from ready: got %q", s.nonce)
	}

	r2, err := s.recv()
	if err != nil {
		t.Fatalf("recv result: %v", err)
	}
	if r2.Stdout == "PWNED" {
		t.Fatal("spoofed frame (wrong nonce) was accepted")
	}
	if r2.ExitCode != 7 || r2.Stdout != "real" {
		t.Fatalf("unexpected result: %+v", r2)
	}
}

// TestAPCRoundTrip runs a real remote.Executor in APC mode wired to a Session
// in APC mode and confirms structured results survive the framing end-to-end.
func TestAPCRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("allowCommands:\n  - echo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	var stderr strings.Builder
	e, err := remote.New(remote.Config{ConfigPath: cfg, APC: true}, reqR, respW, &stderr)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	s := &Session{
		stdin:     reqW,
		stdout:    bufio.NewReaderSize(respR, 64*1024),
		apc:       true,
		Transport: "tsh",
		writeMu:   make(chan struct{}, 1),
		rpcMu:     make(chan struct{}, 1),
		closed:    make(chan struct{}),
	}
	t.Cleanup(func() {
		reqW.Close()
		cancel()
		<-done
	})

	ready, err := s.recv()
	if err != nil {
		t.Fatalf("recv ready: %v\nstderr: %s", err, stderr.String())
	}
	if ready.Type != "log" || ready.Msg != "ready" {
		t.Fatalf("unexpected ready: %+v", ready)
	}

	resp, err := s.RoundTrip(&protocol.Request{ID: 1, Type: "exec", Command: "echo hello"})
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.ExitCode != 0 || strings.TrimSpace(resp.Stdout) != "hello" {
		t.Fatalf("unexpected result: %+v", resp)
	}
}
