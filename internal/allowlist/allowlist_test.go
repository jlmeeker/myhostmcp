package allowlist

import "testing"

func TestValidate(t *testing.T) {
	allow := [][]string{
		{"df"}, {"free"}, {"ps"}, {"ss"}, {"echo"}, {"cd"}, {"pwd"},
		{"systemctl", "restart"}, {"systemctl", "status"},
	}

	// These cases are checked against the `allow` list above. (Every case is
	// also checked against a nil allowlist first, which must always allow.)
	cases := []struct {
		name string
		cmd  string
		ok   bool
	}{
		// simple allowed commands.
		{"df", "df -h", true},
		{"pwd", "pwd", true},
		{"cd", "cd /tmp", true},
		{"echo with vars", "echo $HOME $PWD", true},
		{"systemctl restart nginx", "systemctl restart nginx", true},
		{"systemctl status sshd", "systemctl status sshd", true},

		// assignment prefix is skipped, so the real command is matched.
		{"assignment prefix", "FOO=bar df -h", true},

		// pipelines: every segment must be allowed.
		{"pipeline missing segment", "ps aux | grep nginx", false},

		// not allowed.
		{"rm not allowed", "rm -rf /", false},
		{"sleep not allowed", "sleep 10", false},
		{"systemctl wrong subcommand", "systemctl stop nginx", false},
		{"for not allowed", "for i in 1 2 3; do echo $i; done", false},

		// bypass attempts are rejected regardless of the command word.
		{"redirect bypass", "df > /etc/passwd", false},
		{"append redirect bypass", "df >> /tmp/x", false},
		{"input redirect bypass", "grep foo < /etc/shadow", false},
		{"$() substitution bypass", "cat $(rm -rf /)", false},
		{"backtick substitution bypass", "cat `rm -rf /`", false},
		{"sequence with disallowed tail", "df; rm -rf /", false},
		{"&& with disallowed tail", "df && rm -rf /", false},
		{"|| with disallowed tail", "df || rm -rf /", false},
		{"bg operator with disallowed tail", "df & rm -rf /", false},

		// a pipe character inside quotes must NOT split the segment.
		{"quoted pipe not split", "echo \"hi | there\"", true},
		{"quoted semicolon not split", "echo 'a; b'", true},

		// edge cases.
		{"empty", "", false},
		{"whitespace only", "   ", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A nil allowlist must never reject anything.
			if err := Validate(c.cmd, nil); err != nil {
				t.Fatalf("unrestricted Validate should never error, got: %v", err)
			}
			err := Validate(c.cmd, allow)
			switch {
			case c.ok && err != nil:
				t.Fatalf("expected allowed, got error: %v", err)
			case !c.ok && err == nil:
				t.Fatalf("expected rejection, but command was allowed")
			}
		})
	}
}

func TestValidatePipelineBothAllowed(t *testing.T) {
	// with grep also allowed, the pipeline is permitted.
	allow := [][]string{{"ps"}, {"grep"}}
	if err := Validate("ps aux | grep nginx", allow); err != nil {
		t.Fatalf("expected allowed, got: %v", err)
	}
}

func TestUnrestrictedAllowsDangerous(t *testing.T) {
	// A nil/empty allowlist must permit even dangerous commands; enforcement
	// only happens when an allowlist is configured.
	for _, cmd := range []string{
		"rm -rf /",
		"df > /etc/passwd",
		"cat $(rm -rf /)",
		"for i in 1 2 3; do echo $i; done",
	} {
		if err := Validate(cmd, nil); err != nil {
			t.Fatalf("unrestricted should allow %q, got: %v", cmd, err)
		}
	}
}
