package httpauth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// DefaultPath is the conventional auth config location for HTTP mode.
const DefaultPath = "/etc/myhostmcp/http-auth.yaml"

// Config is the on-disk auth configuration.
type Config struct {
	Users []User `yaml:"users"`

	// byUsername is built at load time for fast username lookups.
	byUsername map[string]User
}

// User maps one username to one or more valid secrets.
//
// You may provide plaintext tokens (tokens) and/or bcrypt hashes (tokenHashes).
// Hashes should be preferred in production.
type User struct {
	Username    string   `yaml:"username"`
	Tokens      []string `yaml:"tokens,omitempty"`
	TokenHashes []string `yaml:"tokenHashes,omitempty"`
}

// Load parses the auth config and validates basic file safety constraints.
//
// Security checks:
//   - path must exist
//   - path must be a regular file (not a symlink)
//   - file must not be accessible by "other" (mode ...---)
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}
	path = expandTilde(path)

	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("read auth config %q: file does not exist", path)
		}
		return nil, fmt.Errorf("read auth config %q: %w", path, err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("auth config %q must not be a symlink", path)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("auth config %q must be a regular file", path)
	}
	if st.Mode().Perm()&0o007 != 0 {
		return nil, fmt.Errorf("auth config %q is too permissive (mode %04o): file must not grant any permissions to 'other'", path, st.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read auth config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse auth config %q: %w", path, err)
	}
	if len(c.Users) == 0 {
		return nil, fmt.Errorf("auth config %q: users is empty", path)
	}
	c.byUsername = make(map[string]User, len(c.Users))
	for _, u := range c.Users {
		u.Username = strings.TrimSpace(u.Username)
		if u.Username == "" {
			return nil, fmt.Errorf("auth config %q: username must not be empty", path)
		}
		if _, dup := c.byUsername[u.Username]; dup {
			return nil, fmt.Errorf("auth config %q: duplicate username %q", path, u.Username)
		}
		for i := range u.Tokens {
			u.Tokens[i] = strings.TrimSpace(u.Tokens[i])
		}
		u.Tokens = nonEmpty(u.Tokens)
		for i := range u.TokenHashes {
			u.TokenHashes[i] = strings.TrimSpace(u.TokenHashes[i])
		}
		u.TokenHashes = nonEmpty(u.TokenHashes)
		if len(u.Tokens) == 0 && len(u.TokenHashes) == 0 {
			return nil, fmt.Errorf("auth config %q: user %q has neither tokens nor tokenHashes", path, u.Username)
		}
		for _, h := range u.TokenHashes {
			if _, err := bcrypt.Cost([]byte(h)); err != nil {
				return nil, fmt.Errorf("auth config %q: user %q has invalid bcrypt hash: %w", path, u.Username, err)
			}
		}
		c.byUsername[u.Username] = u
	}
	return &c, nil
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

var ErrUnauthorized = errors.New("unauthorized")

// AuthenticateBasic validates username+token from HTTP Basic auth.
func (c *Config) AuthenticateBasic(username, token string) error {
	u, ok := c.byUsername[username]
	if !ok {
		return ErrUnauthorized
	}
	if matchUserToken(u, token) {
		return nil
	}
	return ErrUnauthorized
}

// AuthenticateBearer validates a bearer token and returns the associated
// username when successful.
func (c *Config) AuthenticateBearer(token string) (string, error) {
	for _, u := range c.byUsername {
		if matchUserToken(u, token) {
			return u.Username, nil
		}
	}
	return "", ErrUnauthorized
}

func matchUserToken(u User, token string) bool {
	for _, want := range u.Tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1 {
			return true
		}
	}
	for _, hash := range u.TokenHashes {
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(token)) == nil {
			return true
		}
	}
	return false
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
