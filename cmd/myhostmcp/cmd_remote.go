package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"

	"myhostmcp/internal/protocol"
	"myhostmcp/internal/remote"
)

// demoCmd starts `myhostmcp remote --config <tmp>` as a subprocess and drives
// it through a scripted sequence of requests, printing each response. This
// proves the executor (persistent shell, sentinel framing, separated
// stdout/stderr, cwd tracking, allowlist, timeout) end-to-end with no SSH
// required.
//
// The allowlist is written to a temp config file that the remote loads itself
// — the same code path production uses (just a different path). The built-in
// demo allowlist covers the scripted commands; "rm" is deliberately excluded
// so the rejection demo is guaranteed to be rejected (and never actually run).
func demoCmd(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	var allowStr string
	fs.StringVar(&allowStr, "allow", "",
		"comma-separated allowed command tokens to write to a temp remote config "+
			"(empty = a built-in demo set that covers the script). "+
			"Example: df,free,ps,ss,systemctl restart")
	_ = fs.Parse(args)

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo: cannot locate own executable:", err)
		os.Exit(1)
	}

	// Build the allowlist. The built-in demo set covers the scripted commands
	// below; "rm" is deliberately excluded so the rejection demo is guaranteed
	// to be rejected (and never actually run).
	var allow [][]string
	if allowStr != "" {
		for _, part := range strings.Split(allowStr, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			allow = append(allow, strings.Fields(part))
		}
	} else {
		allow = [][]string{
			{"pwd"}, {"cd"}, {"echo"}, {"ls"},
			{"for"}, {"do"}, {"done"},
			{"sleep"}, {"nonexistent-command-xyz"},
		}
	}

	// Write the allowlist to a temp config file and point the remote at it.
	configPath, err := writeDemoConfig(allow)
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo: write temp config:", err)
		os.Exit(1)
	}
	defer os.Remove(configPath)

	cmd := exec.Command(self, "remote", "--config", configPath)
	cmd.Stderr = os.Stderr // surface remote diagnostics

	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "demo: start remote:", err)
		os.Exit(1)
	}

	respCh := make(chan protocol.Response, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var r protocol.Response
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				fmt.Fprintln(os.Stderr, "demo: bad response line:", err)
				continue
			}
			respCh <- r
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "demo: response scanner:", err)
		}
		close(respCh)
	}()

	send := func(req protocol.Request) {
		b, _ := json.Marshal(req)
		b = append(b, '\n')
		if _, err := stdin.Write(b); err != nil {
			fmt.Fprintln(os.Stderr, "demo: write:", err)
		}
	}
	// waitResp returns the response matching id, skipping (but printing) any
	// unsolicited log/error lines.
	waitResp := func(id int64) protocol.Response {
		for r := range respCh {
			if r.ID == id {
				return r
			}
			if r.Type == "log" || r.Type == "error" {
				printResp(r)
			}
		}
		return protocol.Response{Type: "error", Error: "no response (remote exited)"}
	}

	// 1. Wait for the remote's ready announcement.
	ready := waitResp(0)
	fmt.Printf("=== remote ready: version=%s pid=%d hasTimeout=%v ===\n",
		ready.Version, ready.PID, ready.HasTimeout)

	// 2. Query the remote's enforced allowlist (the same request the local half
	// uses for the remote_allowed_commands tool).
	fmt.Printf("=== querying allowed commands: %d entries ===\n", len(allow))
	send(protocol.Request{ID: 1, Type: "allowed_commands"})
	printResp(waitResp(1))

	// 3. Scripted execs. These avoid I/O redirection and command substitution,
	// which the allowlist rejects; stderr separation is shown via a command
	// that errors to stderr naturally (ls on a missing path).
	script := []protocol.Request{
		{ID: 2, Type: "exec", Command: "pwd"},
		{ID: 3, Type: "exec", Command: "cd /tmp"},
		{ID: 4, Type: "exec", Command: "pwd"},
		{ID: 5, Type: "exec", Command: "echo hello; ls /no/such/dir"},
		{ID: 6, Type: "exec", Command: "for i in 1 2 3; do echo line$i; done"},
		{ID: 7, Type: "exec", Command: "sleep 10", TimeoutMs: 500},
		{ID: 8, Type: "exec", Command: "nonexistent-command-xyz; echo after-fail"},
		{ID: 9, Type: "exec", Command: "echo $HOME working=$PWD"},
	}
	// Rejection demo: only if "rm" is not allowed, so it is guaranteed to be
	// rejected (and never actually executed).
	if !allowsToken(allow, "rm") {
		script = append(script, protocol.Request{ID: 10, Type: "exec", Command: "rm -rf /"})
	} else {
		fmt.Println("(skipping rm rejection demo: 'rm' is in the allowlist)")
	}

	for _, req := range script {
		fmt.Printf("\n--- exec #%d: %q (timeout=%dms) ---\n", req.ID, req.Command, req.TimeoutMs)
		send(req)
		printResp(waitResp(req.ID))
	}

	// 4. Shutdown.
	fmt.Println("\n--- shutdown ---")
	send(protocol.Request{ID: 100, Type: "shutdown"})
	waitResp(100)
	_ = stdin.Close()
	_ = cmd.Wait()
	fmt.Println("=== demo complete ===")
}

// writeDemoConfig writes a remote config YAML with the given allowlist to a
// temp file and returns its path. The caller should remove it when done. Each
// entry is written as a single whitespace-joined string — the on-disk format
// the remote's config loader expects (it splits on whitespace into tokens).
func writeDemoConfig(allow [][]string) (string, error) {
	f, err := os.CreateTemp("", "myhostmcp-demo-*.yaml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	type cfg struct {
		AllowCommands []string `yaml:"allowCommands"`
	}
	raws := make([]string, 0, len(allow))
	for _, toks := range allow {
		raws = append(raws, strings.Join(toks, " "))
	}
	b, err := yaml.Marshal(cfg{AllowCommands: raws})
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// allowsToken reports whether any allowlist entry begins with the given token.
func allowsToken(allow [][]string, tok string) bool {
	for _, toks := range allow {
		if len(toks) > 0 && toks[0] == tok {
			return true
		}
	}
	return false
}

func printResp(r protocol.Response) {
	switch r.Type {
	case "result":
		fmt.Printf("exit=%d cwd=%q timedOut=%v dur=%dms\n",
			r.ExitCode, r.CWD, r.TimedOut, r.DurationMs)
		if r.Stdout != "" {
			fmt.Printf("stdout:\n%s", r.Stdout)
		}
		if r.Stderr != "" {
			fmt.Printf("stderr:\n%s", r.Stderr)
		}
		if r.Stdout == "" && r.Stderr == "" {
			fmt.Println("(no output)")
		}
	case "allowlist":
		fmt.Printf("allowlist: %d entries\n", len(r.AllowCommands))
		for _, toks := range r.AllowCommands {
			fmt.Printf("  - %s\n", strings.Join(toks, " "))
		}
	case "error":
		fmt.Printf("ERROR: %s\n", r.Error)
	case "log":
		fmt.Printf("log: %s\n", r.Msg)
	default:
		fmt.Printf("?(%s): %+v\n", r.Type, r)
	}
}

// remoteCmd runs the remote executor, speaking the private protocol on
// stdin/stdout and logging diagnostics to stderr. This is the mode that runs
// on the remote host. It loads its enforced allowlist from
// /etc/myhostmcp/config.yaml by default (override with --config); that file is
// the only place the allowlist is set, so the remote always has the final say.
func remoteCmd(args []string) {
	fs := flag.NewFlagSet("remote", flag.ExitOnError)
	showVersion := fs.Bool("version", false, "print build version and exit")
	configPath := fs.String("config", "", "path to the remote config file (default: /etc/myhostmcp/config.yaml)")
	apc := fs.Bool("apc", false, "recording-friendly framing: wrap responses in an APC envelope and emit a "+
		"human-readable transcript so Teleport session recordings play back as a readable shell session")
	_ = fs.Parse(args)

	if *showVersion {
		printVersion()
		return
	}

	cfg := remote.Config{ConfigPath: *configPath, APC: *apc}
	e, err := remote.New(cfg, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "myhostmcp remote:", err)
		os.Exit(1)
	}

	ctx, cancel := signalContext()
	defer cancel()
	if err := e.Run(ctx); err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "myhostmcp remote:", err)
		os.Exit(1)
	}
}
