# Installing myhostmcp

`myhostmcp` lets an AI agent run commands on a remote host in a persistent
session, over SSH, using **your normal local SSH credentials** (no credentials
or hosts are ever embedded in the binary). It is a single Go binary with two
modes: `myhostmcp local` (a stdio MCP server your agent spawns) and
`myhostmcp remote` (the executor it uploads to and runs on the remote host).

This guide covers building it, optional configuration (local **and** remote),
and wiring it into **Claude Code**, **OpenCode**, and **pi** (from pi.dev).

---

## 1. Prerequisites

- **Go 1.23+** to build (`go version`).
- **SSH access** to the remote host(s) you want to drive, working from a shell:
  ```sh
  ssh yourhost 'uname -sm; id'
  ```
  `myhostmcp` shells out to the real `ssh` binary, so whatever already works for
  you — `~/.ssh/config` aliases, default keys (`~/.ssh/id_ed25519`, etc.),
  ssh-agent, `ProxyJump` bastions, `known_hosts` — works with zero extra setup.
- **GNU `timeout(1)`** on the remote host for per-command timeouts (present on
  essentially all Linux servers; part of coreutils). If absent, a Go deadline
  backstop still bounds commands (by killing the session on timeout).

## 2. Build

From the repo root:

```sh
./build.sh            # cross-compile remote binaries, embed them, build local
```

This produces `build/myhostmcp` — a single static binary with the cross-compiled
remote halves (linux/amd64, linux/arm64, linux/arm, darwin/amd64, darwin/arm64,
freebsd/amd64, freebsd/arm64) embedded inside. At connect time it picks the
right one for the remote host's `uname -sm` and uploads it.

If you only ever deploy to the same OS/arch as your build machine:

```sh
./build.sh current && ./build.sh local
```

Verify:

```sh
build/myhostmcp version
build/myhostmcp demo                       # executor self-test, no SSH needed
build/myhostmcp demo --allow "df,free,ps,ss,echo,cd,pwd,systemctl restart"
```

## 3. Optional configuration

The local half reads `~/.myhostmcp/config.yaml` by default (override with
`myhostmcp local --config <path>`). All fields are optional. Copy
`config.example.yaml` to get started:

```sh
mkdir -p ~/.myhostmcp
cp config.example.yaml ~/.myhostmcp/config.yaml
```

Notable fields:

| Field | Purpose |
|-------|---------|
| `defaultHost` | used when `remote_connect` omits its `host` argument |
| `defaultUser`, `defaultPort` | defaults for connect |
| `identityFiles` | list of `-i` key paths; empty = whatever `ssh` normally uses |
| `remoteInstallDir` | where the remote binary is placed on the host (default `~/.myhostmcp`) |
| `strictHostKeyChecking` | passed to SSH `StrictHostKeyChecking` (default `accept-new`; set `yes` for stricter host-key policy) |
| `connectTimeout`, `execTimeout` | durations like `15s`, `60s` |
| `logFile` | local-half diagnostics; must NOT be stdout (stdout is MCP). Default: stderr. |

> The command **allowlist is not a local setting**. It lives on the remote
> host at `/etc/myhostmcp/config.yaml` so the remote always has the final say
> over what an agent may run there. See "Remote command allowlist" below.

### Remote command allowlist

The remote half reads `/etc/myhostmcp/config.yaml` by default (override with
`myhostmcp remote --config <path>`). It is the **only** place the command
allowlist is specified; the local half cannot set or relax it, only query it
(via `remote_allowed_commands`). If the file is absent or `allowCommands` is
empty, a safe built-in default is used (`df`, `ss`, `top`, `free`, `ls`) —
the remote is never unrestricted.

Install it on each remote host (requires root):

```sh
sudo mkdir -p /etc/myhostmcp
sudo cp config.remote.example.yaml /etc/myhostmcp/config.yaml
sudoedit /etc/myhostmcp/config.yaml
```

Every segment of a command (split at `;`, `|`, `&&`, `||`, `&`) must begin
with one entry's tokens. Command substitution (`$(...)`, backtick) and I/O
redirection (`<`, `>`, `>>`, `<<`) are rejected while an allowlist is active.

```yaml
allowCommands:
  - df
  - free
  - ps
  - ss
  - "systemctl restart"
  - "systemctl status"
```

Enforcement happens **on the remote half**, so the policy cannot be bypassed
by anything the local agent does; disallowed commands come back as remote
errors. The remote must be version-matched to the local half (the local half
re-uploads on a version mismatch), so after changing `/etc/myhostmcp/config.yaml`
just reconnect — no restart needed on the remote.

---

## 4. Wire it into your agent

The local half is a **stdio MCP server**: the agent launches it as a child
process and talks MCP over its stdin/stdout. It does **nothing** (no SSH) on
startup — it connects lazily on the first `remote_connect` (or `remote_exec`
against a configured `defaultHost`).

It exposes five tools:

| Tool | Purpose |
|------|---------|
| `remote_connect` | Open (or reuse) a persistent SSH session; uploads & starts the remote executor. The result includes the remote's enforced allowlist. Args: `host`, `user`, `port`, `identityFile`, `session`. |
| `remote_exec` | Run a command in the persistent remote shell (`cd`/`export` persist). Rejected by the remote if not in its allowlist. Args: `command`, `session`, `cwd`, `timeout`, `pty`. |
| `remote_allowed_commands` | Query the remote host for the allowlist it enforces. Args: `session`. |
| `remote_status` | List open sessions and the default. (Per-host allowlists are reported by `remote_allowed_commands`.) |
| `remote_disconnect` | Close one (`session`), the default (omit `session`), or all (`session: "*"`). |

In the snippets below, replace `/path/to/build/myhostmcp` with the absolute path
to your built binary (e.g. `/home/you/myhostmcp/build/myhostmcp`).

### Claude Code

Claude Code reads stdio MCP servers from `.mcp.json` (project scope),
`~/.claude.json` (user scope), or via the `claude mcp add` command. An entry
with no `url`/`type` is treated as a stdio server.

**Option A — CLI (writes to local scope by default; add `--scope user` for all
projects, or `--scope project` for a shared `.mcp.json`):**

```sh
claude mcp add myhost --scope user -- /path/to/build/myhostmcp local
```

To point at a config file, append its args after `--`:

```sh
claude mcp add myhost --scope user -- /path/to/build/myhostmcp local --config /home/you/.myhostmcp/config.yaml
```

**Option B — JSON (`.mcp.json` in the project root, or the `mcpServers` object
in `~/.claude.json`):**

```json
{
  "mcpServers": {
    "myhost": {
      "command": "/path/to/build/myhostmcp",
      "args": ["local"]
    }
  }
}
```

With a config file:

```json
{
  "mcpServers": {
    "myhost": {
      "command": "/path/to/build/myhostmcp",
      "args": ["local", "--config", "/home/you/.myhostmcp/config.yaml"]
    }
  }
}
```

Verify inside Claude Code with `/mcp` (lists servers and connection status).
The first time you use a project-scoped server you may be asked to approve it.

### OpenCode

OpenCode reads MCP servers from its JSON config (`opencode.json` or
`opencode.jsonc`) at the project root, or `~/.config/opencode/opencode.json`
for a global setup. Local stdio servers use `"type": "local"` with a `command`
array.

`opencode.json` (global or project):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "myhost": {
      "type": "local",
      "command": ["/path/to/build/myhostmcp", "local"],
      "enabled": true
    }
  }
}
```

With a config file and/or environment:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "myhost": {
      "type": "local",
      "command": ["/path/to/build/myhostmcp", "local", "--config", "/home/you/.myhostmcp/config.yaml"],
      "enabled": true,
      "environment": {
        "HOME": "/home/you"
      },
      "timeout": 10000
    }
  }
}
```

`timeout` is for fetching the tool list at startup (ms; default 5000). Once
added, the `remote_*` tools are available to the LLM alongside built-in tools.

### pi (from pi.dev)

**pi has no built-in MCP support** (by design — see pi's
[extensions docs](https://pi.dev)). You bridge to `myhostmcp local` with a
small extension that spawns it as a subprocess and proxies pi tools through. A
ready-to-use, zero-dependency extension is included at
`examples/pi-extension/myhostmcp.ts`.

1. Build myhostmcp (step 2 above).

2. Copy the extension into pi's global extensions directory:
   ```sh
   mkdir -p ~/.pi/agent/extensions
   cp examples/pi-extension/myhostmcp.ts ~/.pi/agent/extensions/myhostmcp.ts
   ```
   (Use `.pi/extensions/` instead for a single project.)

3. Tell the extension where the binary is. Set in your shell environment
   (e.g. `~/.bashrc` / `~/.zshrc`), since pi reads the process environment:
   ```sh
   export MYHOSTMCP_BINARY=/path/to/build/myhostmcp
   # optional:
   export MYHOSTMCP_CONFIG=/home/you/.myhostmcp/config.yaml
   ```
   If `MYHOSTMCP_BINARY` is unset it falls back to `./build/myhostmcp`, then
   `myhostmcp` on `$PATH`, so alternatively copy/symlink the binary somewhere
   on `$PATH`.

4. Start pi (or run `/reload` in a running session). The five `remote_*` tools
   become available. The extension starts `myhostmcp local` when a session
   begins and stops it when the session ends.

How the extension works: on `session_start` it spawns `myhostmcp local`, does
the MCP `initialize` handshake, and registers five pi custom tools
(`remote_connect`, `remote_exec`, `remote_allowed_commands`, `remote_status`,
`remote_disconnect`) whose `execute` functions forward calls to the MCP server
and map the results back to pi's tool-result shape. See
`examples/pi-extension/myhostmcp.ts` for the source.

---

## 5. Verify it works

Once wired in, ask your agent to connect and run something on a host you can SSH
to. For example:

> Use the remote_connect tool to connect to `myserver` (host alias from my
> ~/.ssh/config). Then use remote_exec to run `uname -a` and `df -h`.

You should see the agent call `remote_connect`, then `remote_exec`, and report
the remote output. `cd` and `export` persist across `remote_exec` calls within
the same session — try "cd /var/log then ls" as two separate commands.

You can also drive the full stdio path directly (no agent) to debug:

```sh
go run ./examples/stdio_client -binary ./build/myhostmcp -host myserver
```

## 6. Known limitations

- PTY mode (`remote_exec` with `pty=true`) is not implemented yet.
- Allowlist parsing is intentionally conservative: redirection and command
  substitution are blocked while an allowlist is active.
- If GNU `timeout(1)` is missing on the remote, timeouts still work via a Go
  backstop, but timeout handling may terminate the session.

## 7. Troubleshooting

**"failed to connect to HOST: ..." / SSH auth errors.** Make sure plain `ssh
HOST` works from the same shell you launch the agent from. `myhostmcp` passes
`-o BatchMode=yes`, so it will never prompt — if your key needs a passphrase,
load it into `ssh-agent` (`ssh-add`). To use a specific key, pass
`identityFile` to `remote_connect` or set `identityFiles` in config.

**"unsupported remote platform ... no prebuilt binary".** The embedded set
doesn't include your remote's OS/arch. Run `./build.sh` (not `current`) to embed
all targets, or tell the build script to add your target. Check the remote with
`ssh HOST 'uname -sm'` and confirm that `GOOS-GOARCH` appears in `build.sh`'s
`TARGETS`.

**Stale remote binary / version mismatch.** On each connect, the local half
checks the installed remote's version; if it differs from the local build it
re-uploads. So after rebuilding, just reconnect (or restart the agent session).
To force a clean re-install, remove `~/.myhostmcp/myhostmcp` on the remote.

**Allowlist rejected my command.** The allowlist is enforced **on the remote
host** (`/etc/myhostmcp/config.yaml`; default `df, ss, top, free, ls`). Every
piped/sequenced segment must match an entry, and redirection/command
substitution are blocked entirely. To see what's allowed, call
`remote_allowed_commands`. To allow more, edit `/etc/myhostmcp/config.yaml` on
the remote host (then reconnect — no remote restart needed). See
`config.remote.example.yaml`.

**Where are the logs.** The local half writes diagnostics to `logFile`
(default: stderr, which the agent may capture). The remote half logs to its own
stderr, which the local half forwards into the local log prefixed with
`[session <name> remote]`. On the remote, `~/.myhostmcp/` holds the uploaded
binary (no logs are kept there).

**`remote_exec` says "pty mode is not implemented".** PTY support is planned for
Phase 2. For now, commands run over pipes (clean output, no ANSI escapes).

## 8. Updating

```sh
git pull
./build.sh
```

Then restart your agent (or, for pi, `/reload`; for Claude Code, re-run the
session or `/mcp` to reconnect). The remote half auto-updates on the next
`remote_connect` because of the version check.
