/**
 * myhostmcp — pi extension
 *
 * pi (https://pi.dev) has no built-in MCP support by design. This extension
 * bridges pi to the `myhostmcp local` stdio MCP server: it spawns the local
 * half as a child process, speaks the minimal MCP JSON-RPC protocol over its
 * stdin/stdout, and registers pi custom tools that proxy through to it.
 *
 * Zero npm dependencies — uses only Node built-ins and pi's own packages.
 *
 * Install:
 *   1. Build myhostmcp:  ./build.sh        (from the myhostmcp repo)
 *   2. Copy this file to:  ~/.pi/agent/extensions/myhostmcp.ts
 *   3. (Optional) set env so the extension can find the binary:
 *        export MYHOSTMCP_BINARY=/path/to/build/myhostmcp
 *      If unset, it falls back to ./build/myhostmcp, then "myhostmcp" on $PATH.
 *   4. (Optional) point at a config file:
 *        export MYHOSTMCP_CONFIG=/path/to/config.yaml
 *   5. Start (or /reload) pi. The five `remote_*` tools become available.
 */

import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { existsSync } from "node:fs";
import { Type } from "@earendil-works/pi-ai";
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

// ----- minimal MCP stdio client -------------------------------------------------

interface Pending {
  resolve: (value: any) => void;
  reject: (err: Error) => void;
}

class McpStdioClient {
  private proc: ChildProcessWithoutNullStreams;
  private nextId = 1;
  private pending = new Map<number, Pending>();
  private buffer = "";
  private closed = false;

  constructor(binary: string, args: string[]) {
    this.proc = spawn(binary, args, { stdio: ["pipe", "pipe", "inherit"] });
    this.proc.stdout.setEncoding("utf8");
    this.proc.stdout.on("data", (chunk: string) => this.onData(chunk));
    this.proc.on("error", (err) => this.failAll(err));
    this.proc.on("exit", (code) => {
      this.closed = true;
      this.failAll(new Error(`myhostmcp local exited (code ${code})`));
    });
  }

  private onData(chunk: string) {
    this.buffer += chunk;
    let nl: number;
    while ((nl = this.buffer.indexOf("\n")) >= 0) {
      const line = this.buffer.slice(0, nl).trim();
      this.buffer = this.buffer.slice(nl + 1);
      if (!line) continue;
      let msg: any;
      try {
        msg = JSON.parse(line);
      } catch {
        continue; // ignore non-JSON lines
      }
      // Match responses to pending requests; ignore notifications (no id).
      if (msg && typeof msg.id === "number" && this.pending.has(msg.id)) {
        const p = this.pending.get(msg.id)!;
        this.pending.delete(msg.id);
        if (msg.error) p.reject(new Error(msg.error.message ?? "MCP error"));
        else p.resolve(msg.result);
      }
    }
  }

  private failAll(err: Error) {
    for (const p of this.pending.values()) p.reject(err);
    this.pending.clear();
  }

  /** Send a request and await its result. */
  request(method: string, params: any): Promise<any> {
    if (this.closed) return Promise.reject(new Error("myhostmcp local is not running"));
    return new Promise((resolve, reject) => {
      const id = this.nextId++;
      this.pending.set(id, { resolve, reject });
      const payload = JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n";
      this.proc.stdin.write(payload, (err) => {
        if (err) {
          this.pending.delete(id);
          reject(new Error(`write to myhostmcp: ${err.message}`));
        }
      });
    });
  }

  /** Send a notification (no response expected). */
  notify(method: string, params: any) {
    if (this.closed) return;
    const payload = JSON.stringify({ jsonrpc: "2.0", method, params }) + "\n";
    this.proc.stdin.write(payload);
  }

  async initialize(): Promise<any> {
    const result = await this.request("initialize", {
      protocolVersion: "2025-06-18",
      capabilities: {},
      clientInfo: { name: "pi-myhostmcp", version: "0.1.0" },
    });
    this.notify("notifications/initialized", {});
    return result;
  }

  callTool(name: string, arguments_: Record<string, unknown>): Promise<any> {
    return this.request("tools/call", { name, arguments: arguments_ });
  }

  close() {
    this.closed = true;
    this.failAll(new Error("myhostmcp local closed"));
    try { this.proc.stdin.end(); } catch { /* noop */ }
    try { this.proc.kill(); } catch { /* noop */ }
  }
}

// ----- extension wiring --------------------------------------------------------

// The MCP client is session-scoped: started on session_start (or lazily on the
// first tool call), stopped on session_shutdown. Per pi's guidance, we never
// spawn processes from the factory — only from session_start or tool execute.
let client: McpStdioClient | null = null;

function binaryPath(): string {
  if (process.env.MYHOSTMCP_BINARY) return process.env.MYHOSTMCP_BINARY;
  if (existsSync("./build/myhostmcp")) return "./build/myhostmcp";
  return "myhostmcp";
}

function localArgs(): string[] {
  const args = ["local"];
  if (process.env.MYHOSTMCP_CONFIG) {
    args.push("--config", process.env.MYHOSTMCP_CONFIG);
  }
  return args;
}

async function ensureClient(): Promise<McpStdioClient> {
  if (client) return client;
  const c = new McpStdioClient(binaryPath(), localArgs());
  await c.initialize();
  client = c;
  return c;
}

/** Map an MCP tools/call result onto pi's AgentToolResult shape. */
function toPiResult(mcp: any): { content: any[]; details: any } {
  const content = Array.isArray(mcp?.content) ? mcp.content : [];
  return {
    content,
    details: { isError: !!mcp?.isError, structuredContent: mcp?.structuredContent ?? null },
  };
}

function errResult(msg: string) {
  return { content: [{ type: "text", text: `myhostmcp error: ${msg}` }], details: { isError: true } };
}

export default function (pi: ExtensionAPI) {
  // Pre-warm the MCP server when a session starts; tear it down on shutdown.
  pi.on("session_start", async () => {
    try {
      await ensureClient();
    } catch (e: any) {
      // Don't crash the session; the first tool call will surface the error.
      console.error(`[myhostmcp] failed to start: ${e?.message ?? e}`);
    }
  });

  pi.on("session_shutdown", async () => {
    if (client) {
      client.close();
      client = null;
    }
  });

  // --- remote_connect ---
  pi.registerTool(
    defineTool({
      name: "remote_connect",
      label: "Remote Connect",
      description:
        "Open (or reuse) a persistent SSH session to a remote host and start the remote myhostmcp executor there. The session stays open until remote_disconnect or the pi session ends. No SSH activity happens until this is called.",
      promptSnippet: "Open a persistent SSH session to a remote host.",
      parameters: Type.Object({
        host: Type.Optional(Type.String({ description: "SSH host (as in ~/.ssh/config). If omitted, uses defaultHost from config." })),
        user: Type.Optional(Type.String({ description: "remote login user; if omitted, ssh's default" })),
        port: Type.Optional(Type.Number({ description: "SSH port; defaults to 22" })),
        identityFile: Type.Optional(Type.String({ description: "optional path to a private key, overriding config/ssh defaults" })),
        session: Type.Optional(Type.String({ description: "optional friendly name for this session; auto-named from host if omitted" })),
      }),
      async execute(_id, params) {
        try {
          const c = await ensureClient();
          const r = await c.callTool("remote_connect", params);
          return toPiResult(r);
        } catch (e: any) { return errResult(e?.message ?? String(e)); }
      },
    }),
  );

  // --- remote_exec ---
  pi.registerTool(
    defineTool({
      name: "remote_exec",
      label: "Remote Exec",
      description:
        "Run a shell command in the persistent remote shell of an open session. cd/export/aliases persist across calls. Returns captured stdout, stderr, exit code, and cwd. Subject to the remote host's command allowlist (query it with remote_allowed_commands); the remote has the final say.",
      promptSnippet: "Run a command in the persistent remote shell (cd/export persist).",
      parameters: Type.Object({
        command: Type.String({ description: "the shell command to run in the persistent remote shell" }),
        session: Type.Optional(Type.String({ description: "session to run in; defaults to the most recently connected session" })),
        cwd: Type.Optional(Type.String({ description: "optional working directory to cd into before running the command" })),
        timeout: Type.Optional(Type.String({ description: 'per-command timeout, e.g. "30s" or "2m"; defaults to config execTimeout' })),
        pty: Type.Optional(Type.Boolean({ description: "run in a pseudo-terminal (for sudo/TUIs). Not yet implemented." })),
      }),
      async execute(_id, params) {
        try {
          const c = await ensureClient();
          const r = await c.callTool("remote_exec", params);
          return toPiResult(r);
        } catch (e: any) { return errResult(e?.message ?? String(e)); }
      },
    }),
  );

  // --- remote_allowed_commands ---
  pi.registerTool(
    defineTool({
      name: "remote_allowed_commands",
      label: "Remote Allowed Commands",
      description:
        "Query the remote host for the command allowlist it enforces. The allowlist is configured on the remote host (default: /etc/myhostmcp/config.yaml); the remote always has the final say over what remote_exec may run. Use this to learn which commands are available before running them.",
      promptSnippet: "List the commands the remote host allows remote_exec to run.",
      parameters: Type.Object({
        session: Type.Optional(Type.String({ description: "session to query; defaults to the most recently connected session" })),
      }),
      async execute(_id, params) {
        try {
          const c = await ensureClient();
          const r = await c.callTool("remote_allowed_commands", params);
          return toPiResult(r);
        } catch (e: any) { return errResult(e?.message ?? String(e)); }
      },
    }),
  );

  // --- remote_status ---
  pi.registerTool(
    defineTool({
      name: "remote_status",
      label: "Remote Status",
      description: "Report open remote sessions and which one is the default. (Per-host command allowlists are reported by remote_allowed_commands.)",
      promptSnippet: "List open remote sessions and the default.",
      parameters: Type.Object({}),
      async execute() {
        try {
          const c = await ensureClient();
          const r = await c.callTool("remote_status", {});
          return toPiResult(r);
        } catch (e: any) { return errResult(e?.message ?? String(e)); }
      },
    }),
  );

  // --- remote_disconnect ---
  pi.registerTool(
    defineTool({
      name: "remote_disconnect",
      label: "Remote Disconnect",
      description: 'Close one remote session (by name), the default (if name omitted), or all (name "*"). Releases the remote executor.',
      promptSnippet: "Close a remote session (one, default, or all).",
      parameters: Type.Object({
        session: Type.Optional(Type.String({ description: 'session name; if omitted, disconnects the default; if "*", disconnects all' })),
      }),
      async execute(_id, params) {
        try {
          const c = await ensureClient();
          const r = await c.callTool("remote_disconnect", params);
          return toPiResult(r);
        } catch (e: any) { return errResult(e?.message ?? String(e)); }
      },
    }),
  );
}
