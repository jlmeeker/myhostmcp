//go:build remote_only

package main

import (
	"fmt"
	"os"
)

// localCmd is a stub for remote-only builds, which exclude the MCP SDK to keep
// the uploaded remote binary small. The local mode is never invoked in a
// remote-only build.
func localCmd(args []string) {
	fmt.Fprintln(os.Stderr, "myhostmcp: 'local' subcommand is not available in this (remote-only) build")
	os.Exit(2)
}
