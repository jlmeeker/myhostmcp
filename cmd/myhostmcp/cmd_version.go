package main

import (
	"fmt"

	"myhostmcp/internal/version"
)

func printVersion() {
	fmt.Printf("myhostmcp %s\n", version.Version)
}
