// Package remoteconfig loads the remote-half YAML configuration.
//
// The remote half reads /etc/myhostmcp/config.yaml by default (override with
// `myhostmcp remote --config <path>`). It is the ONLY place the command
// allowlist is specified: the local half no longer holds an allowlist, and the
// "configure" protocol message that used to push one has been removed. This
// keeps the final say over what may run on a host with the host itself — a
// local agent cannot override or relax it.
//
// The single supported field is allowCommands. If the file is absent OR the
// field is empty, a safe built-in default allowlist is used, so the remote is
// never unrestricted.
package remoteconfig

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the conventional remote config file location.
const DefaultPath = "/etc/myhostmcp/config.yaml"

// DefaultAllowCommands is used when no allowlist is configured (file missing
// or allowCommands absent/empty). The remote is never unrestricted.
var DefaultAllowCommands = [][]string{
	{"df"},
	{"ss"},
	{"top"},
	{"free"},
	{"ls"},
}

// Config is the remote-half configuration.
type Config struct {
	// AllowCommands is the allowlist as pre-tokenized command prefixes. Each
	// entry is the leading tokens a command segment must begin with.
	AllowCommands [][]string

	// Path is the file actually loaded, set by Load for diagnostics. It is the
	// resolved path even when the file was missing (and the default was used).
	Path string
}

// yamlConfig is the on-disk shape: a list of command-prefix strings, each
// split on whitespace into tokens at load time. This keeps the YAML friendly
// ("- df", "- systemctl restart") rather than requiring nested sequences.
type yamlConfig struct {
	AllowCommands []string `yaml:"allowCommands"`
}

// Load reads a config file. If path is empty, DefaultPath is used. A missing
// file is not an error: the default allowlist is returned. A malformed file IS
// an error — fail loud rather than silently switching to a different policy
// than the admin intended. If the file parses but allowCommands is empty, the
// default allowlist is returned.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}
	c := &Config{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c.AllowCommands = clone(DefaultAllowCommands)
			return c, nil
		}
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var yc yamlConfig
	if err := yaml.Unmarshal(data, &yc); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	for _, raw := range yc.AllowCommands {
		toks := strings.Fields(raw)
		if len(toks) > 0 {
			c.AllowCommands = append(c.AllowCommands, toks)
		}
	}
	if len(c.AllowCommands) == 0 {
		c.AllowCommands = clone(DefaultAllowCommands)
	}
	return c, nil
}

func clone(in [][]string) [][]string {
	out := make([][]string, len(in))
	for i, s := range in {
		cp := make([]string, len(s))
		copy(cp, s)
		out[i] = cp
	}
	return out
}
