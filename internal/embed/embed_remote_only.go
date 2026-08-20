// This file is compiled only for `remote_only` builds (the small binaries that
// get uploaded to remote hosts). It stubs out the embed package so those
// binaries don't recursively embed copies of themselves.
//
//go:build remote_only

package embed

import "fmt"

func BinaryForUname(sysname, machine string) ([]byte, error) {
	return nil, fmt.Errorf("no embedded binaries in remote-only build")
}

func SupportedPlatforms() []string { return nil }
