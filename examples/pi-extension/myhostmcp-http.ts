/**
 * myhostmcp — pi extension (HTTP mode)
 *
 * pi (https://pi.dev) has no built-in MCP support by design. This extension
 * bridges pi to a remote `myhostmcp serve-http` endpoint: it speaks the MCP
 * JSON-RPC protocol over HTTPS with token authentication, and registers pi
 * custom tools that proxy through to it.
 *
 * Zero npm dependencies — uses only Node built-ins and pi's own packages.
 *
 * Install:
 *   1. Ensure your remote host is running: myhostmcp serve-http
 *   2. Copy this file to: ~/.pi/agent/extensions/myhostmcp-http.ts
 *   3. Set environment variables:
 *        export MYHOSTMCP_HTTP_ENDPOINT="https://se-jump02.vivintsky.com:8443"
 *        # Choose ONE of the following auth methods:
 *        # Option A: HTTP Basic auth
 *        export MYHOSTMCP_HTTP_USER="agent-prod"
 *        export MYHOSTMCP_HTTP_TOKEN="your-token-here"
 *        # Option B: Bearer token auth
 *        export MYHOSTMCP_HTTP_BEARER="your-bearer-token-here"
 *   4. (Optional) Allow self-signed certs in development:
 *        export NODE_TLS_REJECT_UNAUTHORIZED="0"
 *   5. Start (or /reload) pi. The five `remote_*` tools become available.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

// ----- HTTP MCP client with SSE support -----------------------------------------

interface Pending {
  resolve: (value: any) => void;
  reject: (err: Error) => void;
}

class McpHttpClient {
  private endpoint: string;
  private authValue: string; // The value for the Authorization header
  private sessionId: string | null = null; // Mcp-Session-Id from server
  private nextId = 1;

  constructor(endpoint: string, user?: string, token?: string, bearer?: string) {
    this.endpoint = endpoint.replace(/\/$/, ""); // Remove trailing slash

    // Set up authentication header value
    if (bearer) {
      this.authValue = `Bearer ${bearer}`;
    } else if (user && token) {
      const credentials = Buffer.from(`${user}:${token}`).toString("base64");
      this.authValue = `Basic ${credentials}`;
    } else {
      throw new Error("Must provide either MYHOSTMCP_HTTP_BEARER or both MYHOSTMCP_HTTP_USER and MYHOSTMCP_HTTP_TOKEN");
    }
  }

  /**
   * Parse SSE (Server-Sent Events) response and extract the message with matching id.
   * SSE format:
   *   event: message
   *   data: {"jsonrpc":"2.0","id":1,...}
   *
   *   event: message
   *   data: {"jsonrpc":"2.0","id":1,"result":{...}}
   */
  private async parseSSEResponse(response: Response, id: number): Promise<any> {
    const text = await response.text();
    const lines = text.split("\n");

    for (const line of lines) {
      if (line.startsWith("data: ")) {
        const dataJson = line.slice(6).trim();
        if (dataJson) {
          try {
            const msg = JSON.parse(dataJson);
            // Look for the response to our request ID
            if (msg.id === id) {
              return msg;
            }
          } catch (e) {
            // Ignore parse errors, continue to next line
          }
        }
      }
    }

    throw new Error("No matching response found in SSE stream");
  }

  private buildHeaders(): Record<string, string> {
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      "Authorization": this.authValue,
    };
    if (this.sessionId) {
      headers["Mcp-Session-Id"] = this.sessionId;
    }
    return headers;
  }

  private async sendRequest(method: string, params: any): Promise<any> {
    const id = this.nextId++;
    const payload = { jsonrpc: "2.0", id, method, params };

    try {
      console.error(`[myhostmcp-http] sending ${method} (id=${id}) sessionId=${this.sessionId ?? "none"}`);
      const response = await fetch(this.endpoint, {
        method: "POST",
        headers: this.buildHeaders(),
        body: JSON.stringify(payload),
      });

      console.error(`[myhostmcp-http] response status: ${response.status}`);

      // Capture session ID if the server sends one
      const sid = response.headers.get("Mcp-Session-Id");
      if (sid && !this.sessionId) {
        this.sessionId = sid;
        console.error(`[myhostmcp-http] captured Mcp-Session-Id: ${sid}`);
      }

      if (!response.ok) {
        const text = await response.text();
        throw new Error(`HTTP ${response.status}: ${response.statusText} - ${text}`);
      }

      // Parse SSE response
      const data = await this.parseSSEResponse(response, id);
      console.error(`[myhostmcp-http] response for id ${id}:`, JSON.stringify(data).substring(0, 200));

      if (data.error) {
        throw new Error(data.error.message ?? "MCP error");
      }

      return data.result;
    } catch (err: any) {
      throw new Error(`myhostmcp HTTP request failed: ${err.message}`);
    }
  }

  private async notify(method: string, params: any): Promise<void> {
    const payload = { jsonrpc: "2.0", method, params };
    try {
      const response = await fetch(this.endpoint, {
        method: "POST",
        headers: this.buildHeaders(),
        body: JSON.stringify(payload),
      });
      if (!response.ok) {
        console.error(`[myhostmcp-http] notification ${method} failed: ${response.status}`);
      }
    } catch (err: any) {
      console.error(`[myhostmcp-http] notification ${method} error: ${err.message}`);
    }
  }

  async initialize(): Promise<any> {
    const result = await this.sendRequest("initialize", {
      protocolVersion: "2025-06-18",
      capabilities: {},
      clientInfo: { name: "pi-myhostmcp-http", version: "0.1.0" },
    });
    console.error(`[myhostmcp-http] sending notifications/initialized`);
    await this.notify("notifications/initialized", {});
    return result;
  }

  callTool(name: string, arguments_: Record<string, unknown>): Promise<any> {
    return this.sendRequest("tools/call", { name, arguments: arguments_ });
  }
}

// ----- extension wiring --------------------------------------------------------

// The HTTP client is session-scoped: created on first tool call, reused across calls.
let client: McpHttpClient | null = null;

function getConfig(): { endpoint: string; user?: string; token?: string; bearer?: string } {
  const endpoint = process.env.MYHOSTMCP_HTTP_ENDPOINT || "https://se-jump02.vivintsky.com:8443";
  const user = process.env.MYHOSTMCP_HTTP_USER;
  const token = process.env.MYHOSTMCP_HTTP_TOKEN;
  const bearer = process.env.MYHOSTMCP_HTTP_BEARER;

  if (!endpoint) {
    throw new Error("MYHOSTMCP_HTTP_ENDPOINT environment variable is required");
  }

  return { endpoint, user, token, bearer };
}

async function ensureClient(): Promise<McpHttpClient> {
  if (client) return client;

  const { endpoint, user, token, bearer } = getConfig();
  console.error(`[myhostmcp-http] initializing client to ${endpoint}`);
  console.error(`[myhostmcp-http] auth method: ${bearer ? "Bearer token" : "HTTP Basic auth"}`);
  const c = new McpHttpClient(endpoint, user, token, bearer);
  console.error(`[myhostmcp-http] calling initialize()...`);
  await c.initialize();
  console.error(`[myhostmcp-http] client initialized successfully`);
  client = c;
  return c;
}

/** Map an MCP tools/call result onto pi's AgentToolResult shape. */
function toPiResult(mcp: any): { content: any[]; details: any } {
  const content = Array.isArray(mcp?.content) ? [...mcp.content] : [];
  // The server puts full structured data in structuredContent but only a short
  // summary in content. Append the structured data as JSON so the agent sees it.
  if (mcp?.structuredContent != null) {
    content.push({ type: "text", text: JSON.stringify(mcp.structuredContent, null, 2) });
  }
  return {
    content,
    details: { isError: !!mcp?.isError, structuredContent: mcp?.structuredContent ?? null },
  };
}

function errResult(msg: string) {
  return { content: [{ type: "text", text: `myhostmcp error: ${msg}` }], details: { isError: true } };
}

export default function (pi: ExtensionAPI) {
  // Pre-warm the MCP client when a session starts.
  pi.on("session_start", async () => {
    try {
      await ensureClient();
    } catch (e: any) {
      // Don't crash the session; the first tool call will surface the error.
      console.error(`[myhostmcp-http] failed to initialize: ${e?.message ?? e}`);
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
        } catch (e: any) {
          return errResult(e?.message ?? String(e));
        }
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
        } catch (e: any) {
          return errResult(e?.message ?? String(e));
        }
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
        } catch (e: any) {
          return errResult(e?.message ?? String(e));
        }
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
        } catch (e: any) {
          return errResult(e?.message ?? String(e));
        }
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
        } catch (e: any) {
          return errResult(e?.message ?? String(e));
        }
      },
    }),
  );
}
