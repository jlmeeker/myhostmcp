# myhostmcp — a split MCP for running commands on a remote host

> **AI assistance disclosure:** This project/documentation was written with the assistance of AI tools, with human review and edits.

`myhostmcp` lets an AI agent run commands on a remote host *in a persistent
session*, over SSH, using your normal local SSH credentials. It is a single
binary with three modes:

- **`myhostmcp local`** — a stdio MCP server that your agent spawns. It exposes
  a few tools (`remote_connect`, `remote_exec`, `remote_status`,
  `remote_disconnect`). On the first call it SSHes to the target host, uploads
  the remote half, starts it, and from then on forwards commands to it.
- **`myhostmcp remote`** — the executor that runs *on the remote host*. It owns
  one persistent shell and runs commands in it, returning captured stdout,
  stderr, exit code and current working directory.
- **`myhostmcp serve-http`** — an HTTPS MCP endpoint for non-local agents.
  Clients authenticate with token auth from a root-managed auth file
  (HTTP Basic `username:token` and `Authorization: Bearer <token>`).
  The auth file supports plaintext `tokens` and preferred bcrypt
  `tokenHashes`. Tool names are the same (`remote_connect`, `remote_exec`, ...),
  but sessions run locally on the host (no SSH hop).

```
 ┌───────────┐  stdio (MCP)   ┌──────────────┐  ssh (os/exec)  ┌────────────────────────┐
 │  AI Agent │ ←─────────────→│ myhostmcp    │ ←──────────────→│  remote machine         │
 │ (client)  │                │  local       │  newline JSON   │  myhostmcp remote        │
 └───────────┘                └──────────────┘  over SSH stdin │   ├─ persistent bash     │
                              agent launches it;               │   │   (cd/export persist)│
                              no SSH until first call          │   └─ sentinel framing    │
                                                                └────────────────────────┘
```

## Configuration

There are two independent config files:

- **Local half** — `~/.myhostmcp/config.yaml` by default (override with
  `myhostmcp local --config <path>`). All fields optional: SSH defaults
  (`defaultHost`, `defaultUser`, `defaultPort`, `identityFiles`),
  `remoteInstallDir`, `strictHostKeyChecking`, timeouts, `logFile`.
  `strictHostKeyChecking` defaults to `accept-new` (trust-on-first-use);
  set `yes` in stricter environments. See `config.example.yaml`.
  **No SSH credentials or hosts are embedded** — authentication is delegated
  entirely to the real `ssh` binary (`~/.ssh/config`, default keys, ssh-agent,
  bastions, `known_hosts`).
- **Remote half** — `/etc/myhostmcp/config.yaml` by default (override with
  `myhostmcp remote --config <path>`). This is the **only** place the command
  allowlist is specified, so the remote host always has the final say over what
  an agent may run there. If the file is absent or `allowCommands` is empty, a
  safe built-in default is used (`df`, `ss`, `top`, `free`, `ls`) — the remote
  is never unrestricted. See `config.remote.example.yaml`.

## Project layout

```
cmd/myhostmcp/                   dispatch: local | remote | demo | serve-http | version
  main.go                        (cmd_local.go excluded from remote_only builds)
internal/version/                shared build version
internal/protocol/               JSON messages over the SSH channel (private, not MCP)
internal/allowlist/              shell-aware command allowlist + bypass protection
internal/config/                 local-half YAML config loading + defaults
internal/remoteconfig/           remote-half YAML config (/etc/myhostmcp/config.yaml) + default allowlist
internal/remote/                 the executor (persistent bash, sentinel, capture, timeout, builtins)
internal/transport/              ssh via os/exec: dial, embedded-binary upload, framing IO
internal/local/                  stdio MCP server + tool handlers + session mgmt
internal/httpconfig/             HTTPS server-mode config (/etc/myhostmcp/http-server.yaml)
internal/httpauth/               HTTP token auth config loader (/etc/myhostmcp/http-auth.yaml)
internal/httpserver/             HTTPS MCP server mode (streamable HTTP + token auth)
internal/embed/                  go:embed of cross-compiled remote binaries (build.sh-generated)
build.sh                         cross-compile remote targets → embed → build local
examples/stdio_client/           example MCP client that spawns `myhostmcp local` over stdio
examples/pi-extension/           pi extension (pi has no built-in MCP; this bridges to `myhostmcp local`)
```

## Why split it this way

- **Uses your real SSH / Teleport setup.** The local half shells out to `ssh`
  (or `tsh` for Teleport), so your `~/.ssh/config`, default keys, agent,
  bastions and `known_hosts` all work with zero extra configuration.
- **No daemon to run.** The agent launches the local half on startup; it does
  nothing (no SSH) until a tool is actually called — lazy connect.
- **No locking / no blocking.** Each connection starts its own
  `myhostmcp remote` process on the host. A second agent instance simply gets
  its own remote process; nobody is blocked. Exclusivity is automatic: each
  remote reads only its own SSH stdin, so no other client can inject into your
  instance.
- **Per-call host.** `remote_connect` takes a `host` (plus optional `user`,
  `port`, `identityFile`). One local half can hold several open sessions.
- **Persistent session.** The remote drives one long-lived `bash`, so `cd`,
  `export`, aliases and functions survive between calls.
- **Optional command allowlist.** The remote host reads
  `/etc/myhostmcp/config.yaml` and enforces a fixed set of commands (`df`,
  `free`, `ps`, `ss`, `systemctl restart`, …) with shell-bypass protection
  (command substitution and redirection are rejected). It is the *only* place
  the allowlist is set, so the remote has the final say; a local agent can
  query it (`remote_allowed_commands`) but cannot override or relax it. A safe
  default (`df, ss, top, free, ls`) applies when no config is present, so the
  remote is never unrestricted.
- **Self-contained deployment.** The local binary embeds cross-compiled remote
  binaries for common targets and uploads the right one on connect.

## Status

| Phase | What | Done |
|-------|------|------|
| 0 | Executor: persistent shell, sentinel framing, separated stdout/stderr, cwd tracking, timeouts, allowlist | ✅ |
| 1 | SSH transport + `myhostmcp local` stdio MCP server with the 5 tools + embedded-binary upload | ✅ |
| 1.5 | Optional HTTPS MCP endpoint (`serve-http`) with token auth (Basic + Bearer, bcrypt hashes supported) | ✅ |
| 2 | Robustness: drop/reconnect, large-output streaming, pty mode, takeover option | planned |
| 3 | Convenience tools (`read_file`/`write_file`), polish | planned |

### Try it / install it

Full build, configuration, and agent-wiring instructions (Claude Code, OpenCode,
pi) are in **[INSTALL.md](INSTALL.md)**. Quick start:

```sh
./build.sh                               # build local + all remote targets
build/myhostmcp demo                     # executor self-test (no SSH)
go run ./examples/stdio_client -binary ./build/myhostmcp -host 127.0.0.1
```

To wire it into an agent, add `myhostmcp local` as a stdio MCP server (see
INSTALL.md for per-agent details and the pi extension).

## Tools

| Tool | Purpose |
|------|---------|
| `remote_connect` | Open (or reuse) a persistent session (via tsh or ssh). Runs `tsh login` automatically if not authenticated. Args: `host` (req unless `defaultHost` set), `user`, `port`, `identityFile`, `session`, `transport` (`"auto"`/`"ssh"`/`"tsh"`), `teleportProxy`, `teleportCluster`. |
| `remote_exec` | Run a command in the persistent remote shell. `cd`/`export`/`alias`/`source` persist. Args: `command` (req), `session`, `cwd`, `timeout` (e.g. `"30s"`), `pty` (Phase 2). Rejected by the remote if not in its allowlist. |
| `remote_allowed_commands` | Query the remote host for the command allowlist it enforces. Args: `session`. |
| `remote_status` | List open sessions and which one is the default. (Per-host allowlists are reported by `remote_allowed_commands`.) |
| `remote_disconnect` | Close one (`session`), the default (omit `session`), or all (`session: "*"`). |

Each result returns both human-readable text and structured JSON (`exitCode`,
`stdout`, `stderr`, `cwd`, `durationMs`, `timedOut`, `session`).

## How it works

### Persistent session + output framing

`myhostmcp remote` drives one long-lived `bash`. Each command is wrapped in a
`{ ...; } </dev/null` group (so commands that read stdin can't swallow the
sentinel) and followed by a sentinel line:

```
printf '\x01\x02EXIT:%d|CWD:%s\x03\x04\n' "$?" "$PWD"
```

The remote reads stdout until it sees the sentinel, then returns everything
before it as `stdout`, plus the exit code and the new `cwd`. `\x01`/`\x02`/
`\x03`/`\x04` essentially never appear in real output.

### Timeouts and shell builtins

For **external commands** with a timeout set, the remote wraps the command
with GNU `timeout(1)` (exit 124/137 on timeout). On hosts without `timeout(1)`,
or if the sentinel still hasn't arrived, a Go deadline backstop kills the
session.

**Shell builtins** (`cd`, `export`, `alias`, `source`, `eval`, ...) are
**never** wrapped with `timeout`, for two reasons: most have no external
binary (so `timeout cd` would fail with exit 127), and — more importantly —
state changes only persist when a builtin runs in the current shell (`timeout`
spawns a subprocess, so `cd` would be lost). Builtins rely on the deadline
backstop instead. The practical effect: `cd /tmp` persists across calls (the
whole point of the persistent session); a long-running `source bigscript.sh`
is bounded by the backstop rather than `timeout(1)`.

### No locking, no blocking

Each `remote_connect` starts its own `myhostmcp remote` process on the host.
A second agent instance (or a second session in the same chat) simply gets its
own remote process — nobody is blocked. Exclusivity is automatic: each remote
reads only its own SSH stdin, so no other client can inject into your instance.

## Teleport (tsh) support

With `transport: auto` (the default), myhostmcp tries `tsh` first when it is
found in `$PATH`:

1. **Login** — `tsh status` is checked first. If the certificate is missing or
   expired, `tsh login` is run automatically before connecting. For browser-based
   SSO a browser window opens on your desktop and the agent waits for it to
   complete. Terminal-based auth (password / TOTP) requires a tty — run
   `tsh login` manually first, or use SSO / device trust.
2. **Session recording** — the persistent session is started with `-tt` (force
   PTY) so Teleport can record it. The PTY is immediately put into raw mode
   (`stty raw -echo`) so JSON protocol framing is unaffected.
3. **Fallback** — if tsh is unavailable, login fails, or the host is not a
   Teleport node, the connection falls back transparently to `ssh`. The
   `fallbackNote` field in the connect result explains what happened.

Force a specific transport with `remote_connect { transport: "tsh" }` (no
fallback), `{ transport: "ssh" }` (skip tsh entirely), or set `transport` in
`~/.myhostmcp/config.yaml` as the default for all sessions.

## Known limitations

- PTY execution (`remote_exec` with `pty=true`) is not implemented yet.
- Allowlist parsing is intentionally conservative: command substitution and
  redirection are rejected while an allowlist is active.
- If GNU `timeout(1)` is absent on the remote host, timeouts still work via a
  Go backstop but may terminate the whole session when exceeded.
- Teleport terminal-based auth (password/TOTP) requires `tsh login` to be run
  manually before starting the agent; browser-based SSO works automatically.
