//go:build !remote_only

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"myhostmcp/internal/config"
	"myhostmcp/internal/local"
)

// localCmd runs the stdio MCP server that an AI agent spawns. It reads an
// optional config file and serves the five remote_* tools over stdio.
//
// This file is excluded from `remote_only` builds so the small uploaded remote
// binary does not pull in the MCP SDK, transport, or embed packages.
func localCmd(args []string) {
	fs := flag.NewFlagSet("local", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config.yaml (default: ~/.myhostmcp/config.yaml)")
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "myhostmcp local: config error:", err)
		os.Exit(1)
	}

	mgr, err := local.NewManager(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "myhostmcp local: init error:", err)
		os.Exit(1)
	}

	ctx, cancel := signalContext()
	defer cancel()
	if err := mgr.Run(ctx); err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "myhostmcp local:", err)
		os.Exit(1)
	}
}
