// Command myhostmcp is a split MCP: a local half (stdio MCP server the agent
// spawns) and a remote half (an executor uploaded to and run on a remote host
// over SSH). Both are the same binary, dispatched by subcommand.
//
// Subcommands:
//
//	myhostmcp local    run the stdio MCP server the agent spawns
//	myhostmcp remote   run the remote executor on stdin/stdout (uploaded to hosts)
//	myhostmcp demo     drive `myhostmcp remote` over pipes to prove the
//	                   executor end-to-end without SSH
//	myhostmcp version  print the build version
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "local":
		localCmd(os.Args[2:])
	case "remote":
		remoteCmd(os.Args[2:])
	case "demo":
		demoCmd(os.Args[2:])
	case "version", "--version", "-v":
		printVersion()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: myhostmcp <local|remote|demo|version> [flags]")
}
