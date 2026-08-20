// Package remoteconfig loads the remote-half YAML configuration.
//
// The remote half reads /etc/myhostmcp/config.yaml by default (override with
// `myhostmcp remote --config <path>`). It is the ONLY place the command
// allowlist is specified: the local half no longer holds an allowlist, and the
// "configure" protocol message that used to push one has been removed. This
// keeps the final say over what may run on a host with the host itself — a
// local agent cannot override or relax it.
//
// # Allowlist resolution
//
// The config has two levels of allowlists:
//
//   - allowCommands — the base list, applied to every user regardless of group
//     membership. If absent or empty in the file, DefaultAllowCommands is used
//     so the remote is never unrestricted.
//   - groups.<name>.allowCommands — extra commands granted to users who are
//     members of that system group. Multiple group entries are all merged
//     together with the base (additive, never subtractive). A user in both
//     "operations" and "web" gets the union of both groups' entries plus the
//     base.
//
// The actual merging is done by Resolve, which the remote executor calls at
// startup after looking up the current user's system group memberships.
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
// or allowCommands absent/empty) and no group commands apply either.
// The remote is never unrestricted.
var DefaultAllowCommands = [][]string{
	{"df"},
	{"ss"},
	{"top"},
	{"free"},
	{"ls"},
}

// Config is the remote-half configuration.
type Config struct {
	// AllowCommands is the base allowlist as pre-tokenized command prefixes.
	// It applies to all users, regardless of group membership. It is never
	// empty after Load: if the file omits or empties it, Load substitutes
	// DefaultAllowCommands.
	AllowCommands [][]string

	// GroupAllowCommands maps system group name → extra allowed command
	// prefixes granted to members of that group. Resolve merges these with
	// AllowCommands based on the calling user's actual group memberships.
	// It is nil when no groups are configured.
	GroupAllowCommands map[string][][]string

	// Path is the file actually loaded, set by Load for diagnostics. It is the
	// resolved path even when the file was missing (and the default was used).
	Path string
}

// Resolve returns the effective allowlist for a user who is a member of the
// given system groups. It starts with AllowCommands (the base, always
// non-empty after Load) and appends the command prefixes of every configured
// group the user belongs to. Groups that are not present in GroupAllowCommands
// are silently ignored, so passing the full list of the user's system groups
// is safe.
//
// The merged list may contain duplicate prefixes if a command appears in both
// the base and a group list; that is harmless — prefix matching checks each
// entry independently.
//
// If the merged result would somehow be empty (e.g. Config was constructed
// outside Load), DefaultAllowCommands is returned so the remote is never
// unrestricted.
func (c *Config) Resolve(userGroups []string) [][]string {
	merged := clone(c.AllowCommands)
	for _, g := range userGroups {
		if cmds, ok := c.GroupAllowCommands[g]; ok {
			merged = append(merged, cmds...)
		}
	}
	if len(merged) == 0 {
		return clone(DefaultAllowCommands)
	}
	return merged
}

// yamlGroupConfig is the on-disk shape of a single group stanza.
type yamlGroupConfig struct {
	AllowCommands []string `yaml:"allowCommands"`
}

// yamlConfig is the on-disk shape: a list of command-prefix strings, each
// split on whitespace into tokens at load time. This keeps the YAML friendly
// ("- df", "- systemctl restart") rather than requiring nested sequences.
type yamlConfig struct {
	AllowCommands []string                   `yaml:"allowCommands"`
	Groups        map[string]yamlGroupConfig `yaml:"groups"`
}

// Load reads a config file. If path is empty, DefaultPath is used. A missing
// file is not an error: the default allowlist is returned. A malformed file IS
// an error — fail loud rather than silently switching to a different policy
// than the admin intended. If the file parses but allowCommands is empty, the
// default allowlist is returned (the base is never empty).
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
	// Parse per-group allowlists. Groups with no commands are silently dropped
	// (they would have no effect on Resolve).
	if len(yc.Groups) > 0 {
		c.GroupAllowCommands = make(map[string][][]string, len(yc.Groups))
		for name, gc := range yc.Groups {
			var cmds [][]string
			for _, raw := range gc.AllowCommands {
				toks := strings.Fields(raw)
				if len(toks) > 0 {
					cmds = append(cmds, toks)
				}
			}
			if len(cmds) > 0 {
				c.GroupAllowCommands[name] = cmds
			}
		}
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
