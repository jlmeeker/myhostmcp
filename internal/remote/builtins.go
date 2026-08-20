package remote

import "strings"

// bashBuiltins is the set of bash builtin commands. These MUST run in the
// current shell (never wrapped with the external `timeout` command) for two
// reasons:
//   1. Most have no external binary (e.g. `cd`, `export`), so `timeout cd`
//      fails with exit 127.
//   2. The ones that modify shell state (cd, export, alias, source, ...) only
//      persist when run in the current shell; `timeout` spawns a subprocess,
//      so state changes would be lost.
//
// For builtins we rely on the executor's deadline backstop instead of
// `timeout(1)`. Builtins are normally instantaneous; the rare long-running
// case (e.g. `source bigscript.sh`) will be bounded by the backstop, which
// kills the session on deadline.
var bashBuiltins = map[string]bool{
	".": true, "[": true, "alias": true, "bg": true, "bind": true, "break": true,
	"builtin": true, "caller": true, "cd": true, "command": true, "compgen": true,
	"complete": true, "compopt": true, "continue": true, "declare": true, "dirs": true,
	"disown": true, "echo": true, "enable": true, "eval": true, "exec": true,
	"exit": true, "export": true, "false": true, "fc": true, "fg": true,
	"getopts": true, "hash": true, "help": true, "history": true, "jobs": true,
	"kill": true, "let": true, "local": true, "logout": true, "mapfile": true,
	"popd": true, "printf": true, "pushd": true, "pwd": true, "read": true,
	"readarray": true, "readonly": true, "return": true, "set": true, "shift": true,
	"shopt": true, "source": true, "suspend": true, "test": true, "times": true,
	"trap": true, "true": true, "type": true, "typeset": true, "ulimit": true,
	"umask": true, "unalias": true, "unset": true, "wait": true,
}

// firstWord returns the first command word of cmd, skipping any leading
// VAR=value assignments and honouring quotes. It returns "" if no command
// word can be found. This is a lightweight scan — good enough to decide
// whether to wrap with `timeout`; it does not need to be a full shell parser.
func firstWord(cmd string) string {
	i, n := 0, len(cmd)
	// skip leading whitespace
	for i < n && (cmd[i] == ' ' || cmd[i] == '\t') {
		i++
	}
	for i < n {
		// Skip a leading VAR=value assignment (e.g. "FOO=bar cmd").
		if isAssignmentWord(cmd, i) {
			// advance past the assignment token
			j := i
			for j < n && cmd[j] != ' ' && cmd[j] != '\t' {
				if cmd[j] == '\'' || cmd[j] == '"' {
					q := cmd[j]
					j++
					for j < n && cmd[j] != q {
						j++
					}
					if j < n {
						j++
					}
				} else {
					j++
				}
			}
			for j < n && (cmd[j] == ' ' || cmd[j] == '\t') {
				j++
			}
			i = j
			continue
		}
		// Read the command word, honouring quotes.
		var b strings.Builder
		for i < n {
			c := cmd[i]
			if c == ' ' || c == '\t' {
				break
			}
			if c == '\'' {
				i++
				for i < n && cmd[i] != '\'' {
					b.WriteByte(cmd[i])
					i++
				}
				if i < n {
					i++
				}
				continue
			}
			if c == '"' {
				i++
				for i < n && cmd[i] != '"' {
					if cmd[i] == '\\' && i+1 < n {
						b.WriteByte(cmd[i+1])
						i += 2
						continue
					}
					b.WriteByte(cmd[i])
					i++
				}
				if i < n {
					i++
				}
				continue
			}
			if c == '\\' && i+1 < n {
				b.WriteByte(cmd[i+1])
				i += 2
				continue
			}
			b.WriteByte(c)
			i++
		}
		return b.String()
	}
	return ""
}

// isAssignmentWord reports whether the token at cmd[i] looks like a VAR=value
// assignment (NAME immediately followed by '='), where NAME is a valid shell
// identifier.
func isAssignmentWord(cmd string, i int) bool {
	j := i
	if j >= len(cmd) || !isIdentStart(cmd[j]) {
		return false
	}
	for j < len(cmd) && isIdentChar(cmd[j]) {
		j++
	}
	return j < len(cmd) && cmd[j] == '='
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// isBuiltinCommand reports whether the command's first word is a bash builtin.
func isBuiltinCommand(cmd string) bool {
	w := firstWord(cmd)
	if w == "" {
		return false
	}
	return bashBuiltins[w]
}
