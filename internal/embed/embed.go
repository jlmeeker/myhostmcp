// Package embed provides the cross-compiled remote binaries embedded in the
// local binary, so the local half can upload the right one to a remote host
// without any external toolchain or manual setup.
//
// This file is compiled for the LOCAL build (no `remote_only` build tag) and
// contains the real go:embed directives.
//
//go:build !remote_only

package embed

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
)

//go:embed bin/*.gz
var remoteBinaries embed.FS

// unameMap translates the output of `uname -sm` (e.g. "Linux x86_64") into the
// GOOS-GOARCH label used for the embedded file names.
var unameMap = map[string]string{
	"Linux x86_64":   "linux-amd64",
	"Linux aarch64":  "linux-arm64",
	"Linux armv7l":   "linux-arm",
	"Darwin arm64":   "darwin-arm64",
	"Darwin x86_64":  "darwin-amd64",
	"FreeBSD amd64":  "freebsd-amd64",
	"FreeBSD arm64":  "freebsd-arm64",
}

// BinaryForUname returns the decompressed remote binary for the given
// `uname -sm` output, or an error if no prebuilt binary is available.
func BinaryForUname(sysname, machine string) ([]byte, error) {
	key := sysname + " " + machine
	arch, ok := unameMap[key]
	if !ok {
		return nil, fmt.Errorf("unsupported remote platform %q: no prebuilt binary", key)
	}
	data, err := remoteBinaries.ReadFile("bin/myhostmcp-" + arch + ".gz")
	if err != nil {
		return nil, fmt.Errorf("embedded binary for %s: %w", arch, err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gzip for %s: %w", arch, err)
	}
	defer gz.Close()
	bin, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("gunzip for %s: %w", arch, err)
	}
	return bin, nil
}

// SupportedPlatforms returns the platforms for which embedded binaries exist.
func SupportedPlatforms() []string {
	seen := map[string]bool{}
	for _, v := range unameMap {
		seen[v] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}
