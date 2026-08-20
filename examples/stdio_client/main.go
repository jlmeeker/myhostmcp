// Command stdio_client is an example MCP client that spawns `myhostmcp local`
// as a subprocess (exactly how an AI agent would) and exercises the five
// remote_* tools over stdio. It demonstrates the real deployment path.
//
// Run from the repo root after building:
//
//	go run ./examples/stdio_client -binary ./build/myhostmcp -host 127.0.0.1
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	binary := flag.String("binary", "./build/myhostmcp", "path to the myhostmcp local binary")
	host := flag.String("host", "127.0.0.1", "remote host to connect to (per your ~/.ssh/config)")
	user := flag.String("user", "", "remote user (optional)")
	port := flag.Int("port", 22, "remote port")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Spawn `myhostmcp local` as a subprocess, talking over stdio. This is the
	// same mechanism an AI agent uses to launch an MCP server.
	t := &mcp.CommandTransport{Command: exec.Command(*binary, "local")}
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-client-example", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(ctx, t, nil)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	// List tools to confirm the server advertised them.
	fmt.Println("=== tools advertised by myhostmcp local ===")
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			log.Fatalf("list tools: %v", err)
		}
		fmt.Printf("  - %s: %s\n", tool.Name, tool.Description)
	}

	// 1. remote_connect
	fmt.Printf("\n=== remote_connect host=%s port=%d ===\n", *host, *port)
	args := map[string]any{"host": *host, "port": *port}
	if *user != "" {
		args["user"] = *user
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remote_connect", Arguments: args})
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	printResult(res)

	// 2. remote_allowed_commands: query the remote's enforced allowlist
	fmt.Println("\n=== remote_allowed_commands ===")
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "remote_allowed_commands", Arguments: map[string]any{}})
	if err != nil {
		log.Fatalf("allowed_commands: %v", err)
	}
	printResult(res)

	// 3. remote_exec: an allowed command (df is in the built-in default).
	fmt.Println("\n=== remote_exec: df -h / ===")
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "remote_exec", Arguments: map[string]any{"command": "df -h /", "timeout": "10s"}})
	if err != nil {
		log.Fatalf("exec df: %v", err)
	}
	printResult(res)

	// 4. remote_exec: a disallowed command is rejected by the remote (the
	//    remote has the final say over its allowlist).
	fmt.Println("\n=== remote_exec: rm -rf / (expect remote rejection) ===")
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "remote_exec", Arguments: map[string]any{"command": "rm -rf /"}})
	if err != nil {
		log.Fatalf("exec rm: %v", err)
	}
	printResult(res)

	// 6. remote_status
	fmt.Println("\n=== remote_status ===")
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "remote_status", Arguments: map[string]any{}})
	if err != nil {
		log.Fatalf("status: %v", err)
	}
	printResult(res)

	// 7. remote_disconnect
	fmt.Println("\n=== remote_disconnect ===")
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "remote_disconnect", Arguments: map[string]any{}})
	if err != nil {
		log.Fatalf("disconnect: %v", err)
	}
	printResult(res)

	fmt.Println("\n=== stdio client example complete ===")
}

func printResult(res *mcp.CallToolResult) {
	if res.IsError {
		fmt.Println("RESULT (isError=true):")
	} else {
		fmt.Println("RESULT:")
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			fmt.Println(tc.Text)
		}
	}
	if res.StructuredContent != nil {
		b, _ := json.MarshalIndent(res.StructuredContent, "  ", "  ")
		fmt.Printf("  structured: %s\n", b)
	}
}
