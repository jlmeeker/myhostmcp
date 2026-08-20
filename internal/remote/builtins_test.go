package remote

import "testing"

func TestFirstWord(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"pwd", "pwd"},
		{"cd /tmp", "cd"},
		{"   ls -la", "ls"},
		{"FOO=bar df -h", "df"},        // assignment prefix skipped
		{"A=1 B=2 ps aux", "ps"},       // multiple assignments skipped
		{"'echo' hi", "echo"},          // single-quoted command
		{`"grep" foo`, "grep"},         // double-quoted command
		{`/bin/ls -l`, "/bin/ls"},      // absolute path
		{`e\c\ho hi`, "echo"},          // backslash escapes
		{"", ""},
		{"   ", ""},
		{"FOO=bar", ""}, // only an assignment, no command word
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			got := firstWord(c.cmd)
			if got != c.want {
				t.Fatalf("firstWord(%q) = %q, want %q", c.cmd, got, c.want)
			}
		})
	}
}

func TestIsBuiltinCommand(t *testing.T) {
	builtins := []string{"cd /tmp", "export FOO=bar", "pwd", "source x.sh", ". script", "alias ll", "eval $cmd"}
	for _, c := range builtins {
		if !isBuiltinCommand(c) {
			t.Errorf("isBuiltinCommand(%q) = false, want true", c)
		}
	}
	externals := []string{"ls -la", "df -h", "sleep 10", "ps aux", "/bin/ls", "grep foo", "systemctl restart nginx"}
	for _, c := range externals {
		if isBuiltinCommand(c) {
			t.Errorf("isBuiltinCommand(%q) = true, want false", c)
		}
	}
}
