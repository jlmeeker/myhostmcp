// Package config loads the local-half YAML configuration.
//
// The local half reads ~/.myhostmcp/config.yaml by default (override with
// --config). All fields are optional. Crucially, this package stores NO SSH
// credentials or hosts: authentication is delegated entirely to the real ssh
// binary (default keys, ssh-agent, ~/.ssh/config). The config only holds
// optional convenience defaults. The command allowlist is NOT configured here
// — it lives on the remote host (see package remoteconfig), so the remote
// always has the final say over what an agent may run.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it can be parsed from YAML strings such as
// "15s" or "2m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the local-half configuration.
type Config struct {
	DefaultHost           string   `yaml:"defaultHost"`
	DefaultUser           string   `yaml:"defaultUser"`
	DefaultPort           int      `yaml:"defaultPort"`
	IdentityFiles         []string `yaml:"identityFiles"`
	RemoteInstallDir      string   `yaml:"remoteInstallDir"`
	StrictHostKeyChecking string   `yaml:"strictHostKeyChecking"`
	ConnectTimeout        Duration `yaml:"connectTimeout"`
	ExecTimeout           Duration `yaml:"execTimeout"`
	LogFile               string   `yaml:"logFile"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		DefaultPort:           22,
		RemoteInstallDir:      "~/.myhostmcp",
		StrictHostKeyChecking: "accept-new",
		ConnectTimeout:        Duration(15 * time.Second),
		ExecTimeout:           Duration(60 * time.Second),
	}
}

// DefaultPath returns the conventional config file location.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".myhostmcp", "config.yaml"), nil
}

// Load reads a config file, applying defaults for any unset fields. If path is
// empty, the default location is used. A missing file is not an error: the
// defaults are returned.
func Load(path string) (*Config, error) {
	c := Default()
	if path == "" {
		p, err := DefaultPath()
		if err == nil {
			path = p
		}
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config %q: %w", path, err)
			}
		} else if err := yaml.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
	}
	// Re-apply defaults for zero fields (in case the file set them to zero).
	if c.DefaultPort == 0 {
		c.DefaultPort = 22
	}
	if c.RemoteInstallDir == "" {
		c.RemoteInstallDir = "~/.myhostmcp"
	}
	if c.StrictHostKeyChecking == "" {
		c.StrictHostKeyChecking = "accept-new"
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = Duration(15 * time.Second)
	}
	if c.ExecTimeout == 0 {
		c.ExecTimeout = Duration(60 * time.Second)
	}
	// Expand ~ for LOCAL paths only. remoteInstallDir is a remote path; its ~
	// is expanded by the remote shell, so it is left untouched.
	for i, f := range c.IdentityFiles {
		c.IdentityFiles[i] = ExpandTilde(f)
	}
	if c.LogFile != "" {
		c.LogFile = ExpandTilde(c.LogFile)
	}
	return &c, nil
}

// ExpandTilde replaces a leading ~ with the local home directory.
func ExpandTilde(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
