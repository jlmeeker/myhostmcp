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
