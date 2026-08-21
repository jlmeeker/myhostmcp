package httpconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the conventional server config file location.
const DefaultPath = "/etc/myhostmcp/http-server.yaml"

// Duration wraps time.Duration for YAML string parsing.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Config controls the HTTP MCP server mode.
type Config struct {
	Listen           string   `yaml:"listen"`
	TLSCertFile      string   `yaml:"tlsCertFile"`
	TLSKeyFile       string   `yaml:"tlsKeyFile"`
	RemoteConfigPath string   `yaml:"remoteConfigPath"`
	AuthConfigPath   string   `yaml:"authConfigPath"`
	LogFile          string   `yaml:"logFile"`
	ExecTimeout      Duration `yaml:"execTimeout"`
	ReadTimeout      Duration `yaml:"readTimeout"`
	WriteTimeout     Duration `yaml:"writeTimeout"`
	IdleTimeout      Duration `yaml:"idleTimeout"`
}

func Default() Config {
	return Config{
		Listen:           ":8443",
		RemoteConfigPath: "/etc/myhostmcp/config.yaml",
		AuthConfigPath:   "/etc/myhostmcp/http-auth.yaml",
		ExecTimeout:      Duration(60 * time.Second),
		ReadTimeout:      Duration(30 * time.Second),
		WriteTimeout:     Duration(0),
		IdleTimeout:      Duration(2 * time.Minute),
	}
}

func Load(path string) (*Config, error) {
	c := Default()
	if path == "" {
		path = DefaultPath
	}
	path = expandTilde(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = ":8443"
	}
	if c.AuthConfigPath == "" {
		c.AuthConfigPath = "/etc/myhostmcp/http-auth.yaml"
	}
	if c.RemoteConfigPath == "" {
		c.RemoteConfigPath = "/etc/myhostmcp/config.yaml"
	}
	if c.ExecTimeout == 0 {
		c.ExecTimeout = Duration(60 * time.Second)
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = Duration(30 * time.Second)
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = Duration(2 * time.Minute)
	}
	c.TLSCertFile = expandTilde(c.TLSCertFile)
	c.TLSKeyFile = expandTilde(c.TLSKeyFile)
	c.AuthConfigPath = expandTilde(c.AuthConfigPath)
	c.RemoteConfigPath = expandTilde(c.RemoteConfigPath)
	if c.LogFile != "" {
		c.LogFile = expandTilde(c.LogFile)
	}
	if c.TLSCertFile == "" || c.TLSKeyFile == "" {
		return nil, fmt.Errorf("tlsCertFile and tlsKeyFile are required")
	}
	return &c, nil
}

func expandTilde(p string) string {
	if p == "~" {
		h, err := os.UserHomeDir()
		if err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		h, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}
