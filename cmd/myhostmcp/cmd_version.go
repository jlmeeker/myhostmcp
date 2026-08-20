package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"myhostmcp/internal/version"
)

// printVersion prints "myhostmcp <version> <sha256>", where <sha256> is the
// hash of this binary's own file. The local half compares that hash against the
// embedded binary it would upload to decide whether a remote binary is stale
// and must be replaced — so re-upload is driven by actual binary content, not
// by the version tag (which does not change between dev builds). If the hash
// cannot be computed it is omitted; the local half then treats it as a mismatch
// and re-uploads, which is the safe default.
func printVersion() {
	if h := selfHash(); h != "" {
		fmt.Printf("myhostmcp %s %s\n", version.Version, h)
	} else {
		fmt.Printf("myhostmcp %s\n", version.Version)
	}
}

// selfHash returns the hex-encoded SHA-256 of the running executable's file, or
// "" if it cannot be determined.
func selfHash() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
