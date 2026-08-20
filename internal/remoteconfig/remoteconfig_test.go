package remoteconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingFileUsesDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(c.AllowCommands) != len(DefaultAllowCommands) {
		t.Fatalf("missing file: got %d entries, want default %d", len(c.AllowCommands), len(DefaultAllowCommands))
	}
	// Must equal the documented safe default, in order.
	want := [][]string{{"df"}, {"ss"}, {"top"}, {"free"}, {"ls"}}
	for i, w := range want {
		if len(c.AllowCommands[i]) != 1 || c.AllowCommands[i][0] != w[0] {
			t.Fatalf("default entry %d = %v, want %v", i, c.AllowCommands[i], w)
		}
	}
}

func TestLoadPresentFile(t *testing.T) {
	p := writeFile(t, t.TempDir(), "config.yaml",
		"allowCommands:\n  - df\n  - \"systemctl restart\"\n  - free\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AllowCommands) != 3 {
		t.Fatalf("got %d entries, want 3", len(c.AllowCommands))
	}
	// "systemctl restart" is a single YAML string, split on whitespace into
	// two tokens at load time.
	if got := c.AllowCommands[1]; len(got) != 2 || got[0] != "systemctl" || got[1] != "restart" {
		t.Fatalf("entry 1 = %v, want [systemctl restart]", got)
	}
	if got := c.AllowCommands[0]; len(got) != 1 || got[0] != "df" {
		t.Fatalf("entry 0 = %v, want [df]", got)
	}
}

func TestLoadEmptyAllowCommandsUsesDefault(t *testing.T) {
	p := writeFile(t, t.TempDir(), "config.yaml", "allowCommands: []\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AllowCommands) != len(DefaultAllowCommands) {
		t.Fatalf("empty allowCommands: got %d, want default %d (never unrestricted)",
			len(c.AllowCommands), len(DefaultAllowCommands))
	}
}

func TestLoadAbsentFieldUsesDefault(t *testing.T) {
	p := writeFile(t, t.TempDir(), "config.yaml", "# nothing here\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AllowCommands) != len(DefaultAllowCommands) {
		t.Fatalf("absent field: got %d, want default %d", len(c.AllowCommands), len(DefaultAllowCommands))
	}
}

func TestLoadMalformedIsHardError(t *testing.T) {
	p := writeFile(t, t.TempDir(), "config.yaml",
		"allowCommands: [this is : not valid yaml\n")
	if _, err := Load(p); err == nil {
		t.Fatal("malformed config should be a hard error, not a silent default")
	}
}

func TestLoadDefaultPathWhenEmpty(t *testing.T) {
	// Load("") should target DefaultPath. We can't rely on /etc existing, but
	// the resolved Path must be DefaultPath (used for diagnostics), and it must
	// not error whether or not the file exists.
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if c.Path != DefaultPath {
		t.Fatalf("Path = %q, want %q", c.Path, DefaultPath)
	}
}

// ---------------------------------------------------------------------------
// Group allowlist tests
// ---------------------------------------------------------------------------

func TestLoadGroupCommands(t *testing.T) {
	p := writeFile(t, t.TempDir(), "config.yaml", `
allowCommands:
  - df
  - ls
groups:
  operations:
    allowCommands:
      - systemctl
      - "journalctl -n"
  web:
    allowCommands:
      - "nginx -t"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AllowCommands) != 2 {
		t.Fatalf("base AllowCommands: got %d entries, want 2", len(c.AllowCommands))
	}
	if len(c.GroupAllowCommands) != 2 {
		t.Fatalf("GroupAllowCommands: got %d groups, want 2", len(c.GroupAllowCommands))
	}
	ops, ok := c.GroupAllowCommands["operations"]
	if !ok {
		t.Fatal("operations group missing from GroupAllowCommands")
	}
	if len(ops) != 2 {
		t.Fatalf("operations: got %d entries, want 2", len(ops))
	}
	if got := ops[0]; len(got) != 1 || got[0] != "systemctl" {
		t.Fatalf("operations[0] = %v, want [systemctl]", got)
	}
	if got := ops[1]; len(got) != 2 || got[0] != "journalctl" || got[1] != "-n" {
		t.Fatalf("operations[1] = %v, want [journalctl -n]", got)
	}
	web, ok := c.GroupAllowCommands["web"]
	if !ok {
		t.Fatal("web group missing from GroupAllowCommands")
	}
	if len(web) != 1 || len(web[0]) != 2 || web[0][0] != "nginx" || web[0][1] != "-t" {
		t.Fatalf("web[0] = %v, want [nginx -t]", web[0])
	}
}

func TestLoadGroupNoCommandsDropped(t *testing.T) {
	// A group whose allowCommands list is empty should be silently omitted
	// (it would have no effect on Resolve).
	p := writeFile(t, t.TempDir(), "config.yaml", `
allowCommands:
  - df
groups:
  empty:
    allowCommands: []
  real:
    allowCommands:
      - ls
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := c.GroupAllowCommands["empty"]; ok {
		t.Fatal("empty group should be dropped from GroupAllowCommands")
	}
	if _, ok := c.GroupAllowCommands["real"]; !ok {
		t.Fatal("real group should be present in GroupAllowCommands")
	}
}

func TestLoadNoGroupsNilMap(t *testing.T) {
	// When no groups stanza is present, GroupAllowCommands should be nil
	// (Resolve must handle a nil map gracefully).
	p := writeFile(t, t.TempDir(), "config.yaml", "allowCommands:\n  - df\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GroupAllowCommands != nil {
		t.Fatalf("expected nil GroupAllowCommands when no groups configured, got %v", c.GroupAllowCommands)
	}
}

// ---------------------------------------------------------------------------
// Resolve tests
// ---------------------------------------------------------------------------

func TestResolveNoGroups(t *testing.T) {
	// User in no configured group: gets only the base AllowCommands.
	p := writeFile(t, t.TempDir(), "config.yaml", `
allowCommands:
  - df
groups:
  operations:
    allowCommands:
      - systemctl
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.Resolve(nil)
	if len(got) != 1 || got[0][0] != "df" {
		t.Fatalf("Resolve(nil) = %v, want [[df]]", got)
	}
}

func TestResolveUnknownGroupsIgnored(t *testing.T) {
	// Groups the user is in that are not configured are silently ignored.
	p := writeFile(t, t.TempDir(), "config.yaml", `
allowCommands:
  - df
groups:
  operations:
    allowCommands:
      - systemctl
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// "staff" and "sudo" are not configured.
	got := c.Resolve([]string{"staff", "sudo"})
	if len(got) != 1 || got[0][0] != "df" {
		t.Fatalf("Resolve(unknown groups) = %v, want [[df]]", got)
	}
}

func TestResolveSingleMatchedGroup(t *testing.T) {
	// User in "operations" gets base + operations commands.
	p := writeFile(t, t.TempDir(), "config.yaml", `
allowCommands:
  - df
groups:
  operations:
    allowCommands:
      - systemctl
      - journalctl
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.Resolve([]string{"staff", "operations"})
	// df + systemctl + journalctl
	if len(got) != 3 {
		t.Fatalf("Resolve([operations]) = %d entries, want 3: %v", len(got), got)
	}
	if got[0][0] != "df" {
		t.Fatalf("entry 0 = %v, want [df]", got[0])
	}
	if got[1][0] != "systemctl" {
		t.Fatalf("entry 1 = %v, want [systemctl]", got[1])
	}
	if got[2][0] != "journalctl" {
		t.Fatalf("entry 2 = %v, want [journalctl]", got[2])
	}
}

func TestResolveMultipleMatchedGroups(t *testing.T) {
	// User in both "web" and "ops" gets base + both groups' commands.
	p := writeFile(t, t.TempDir(), "config.yaml", `
allowCommands:
  - ls
groups:
  web:
    allowCommands:
      - "nginx -t"
  ops:
    allowCommands:
      - systemctl
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.Resolve([]string{"web", "ops"})
	// ls + nginx -t + systemctl = 3 entries
	if len(got) != 3 {
		t.Fatalf("Resolve([web ops]) = %d entries, want 3: %v", len(got), got)
	}
}

func TestResolveNoConfiguredGroupsBaseOnly(t *testing.T) {
	// No groups stanza at all: Resolve behaves like AllowCommands regardless
	// of what user groups are passed.
	p := writeFile(t, t.TempDir(), "config.yaml", `
allowCommands:
  - df
  - free
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.Resolve([]string{"root", "wheel", "staff"})
	if len(got) != 2 {
		t.Fatalf("Resolve with no groups config = %d entries, want 2: %v", len(got), got)
	}
}

func TestResolveDefaultFallbackWhenEmpty(t *testing.T) {
	// A zero-value Config (no AllowCommands, no GroupAllowCommands) should
	// fall back to DefaultAllowCommands via Resolve, never returning empty.
	c := &Config{} // deliberately zero; bypasses Load guarantees
	got := c.Resolve(nil)
	if len(got) != len(DefaultAllowCommands) {
		t.Fatalf("Resolve on zero Config = %d entries, want default %d", len(got), len(DefaultAllowCommands))
	}
}

func TestResolveMissingFileWithGroups(t *testing.T) {
	// Missing config file → default base. Resolve on that with any groups
	// (none configured) should return the default only.
	c, err := Load("/no/such/file/config.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.Resolve([]string{"operations", "web"})
	if len(got) != len(DefaultAllowCommands) {
		t.Fatalf("missing file + Resolve = %d entries, want default %d: %v",
			len(got), len(DefaultAllowCommands), got)
	}
}
