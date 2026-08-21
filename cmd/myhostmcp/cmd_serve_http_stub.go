//go:build remote_only

package main

import (
	"fmt"
	"os"
)

func serveHTTPCmd(args []string) {
	fmt.Fprintln(os.Stderr, "myhostmcp: 'serve-http' subcommand is not available in this (remote-only) build")
	os.Exit(2)
}
