package httpauth

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestLoadAndAuthenticate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "http-auth.yaml")
	h, err := bcrypt.GenerateFromPassword([]byte("s3cr3t-hash"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	content := "users:\n" +
		"  - username: alice\n" +
		"    tokens:\n" +
		"      - plain-token\n" +
		"  - username: bob\n" +
		"    tokenHashes:\n" +
		"      - \"" + string(h) + "\"\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := cfg.AuthenticateBasic("alice", "plain-token"); err != nil {
		t.Fatalf("AuthenticateBasic(alice) error = %v", err)
	}
	if err := cfg.AuthenticateBasic("bob", "s3cr3t-hash"); err != nil {
		t.Fatalf("AuthenticateBasic(bob/hash) error = %v", err)
	}
	if err := cfg.AuthenticateBasic("alice", "wrong"); err == nil {
		t.Fatalf("AuthenticateBasic(alice, wrong) expected error")
	}
	u, err := cfg.AuthenticateBearer("plain-token")
	if err != nil || u != "alice" {
		t.Fatalf("AuthenticateBearer(plain) = %q, %v; want alice, nil", u, err)
	}
	u, err = cfg.AuthenticateBearer("s3cr3t-hash")
	if err != nil || u != "bob" {
		t.Fatalf("AuthenticateBearer(hash) = %q, %v; want bob, nil", u, err)
	}
}

func TestLoadRejectsWorldReadable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "http-auth.yaml")
	content := "users:\n  - username: alice\n    tokens:\n      - t\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatalf("Load() expected permissions error")
	}
}
