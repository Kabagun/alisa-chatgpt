const PROTOCOL_VERSION = "2026-07-28";
const LEGACY_PROTOCOL_VERSION = "2025-11-25";
const SUPPORTED_VERSIONS = [PROTOCOL_VERSION, LEGACY_PROTOCOL_VERSION];
const SERVER_INFO = { name: "alisa-voice-probe", version: "1.1.0" };
const TOOL_NAME = "alisa_voice_probe_receipt";
const MAX_BODY_BYTES = 16 * 1024;
const DNS_HOSTS = ["by.iot.dreame.tech", "eu.iot.dreame.tech"];
const DNS_TIMEOUT_MS = 6000;

const page = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Private Voice tool probe</title>
<style>body{font:18px/1.6 system-ui,sans-serif;max-width:38rem;margin:12vh auto;padding:1.5rem;color:#183047;background:#f6f8fa}h1{font-size:1.7rem}</style></head>
<body><main><h1>Private Voice tool probe</h1>
<p>This test returns a fresh code and receipt to confirm a tool was called.</p>
<p>It does not control home devices, read accounts, or send messages.</p>
<p>Status: test endpoint available.</p>
<p>MCP endpoint: <code>POST /mcp</code></p>
<p><a href="/diagnostics/dreame-dns">Check Dreame public DNS with Cloudflare</a></p>
<p>The diagnostic reports a global resolver observation, not an account-region setting.</p>
</main></body></html>`;

const tool = {
  name: TOOL_NAME,
  description: "Read-only Voice connectivity test. Returns a fresh six-digit code, random receipt, and time. Does not access or control home devices, accounts, or messages.",
  inputSchema: { type: "object", properties: {}, additionalProperties: false },
  annotations: {
    readOnlyHint: true,
    destructiveHint: false,
    idempotentHint: false,
    openWorldHint: false,
  },
};

function json(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
      "cache-control": "no-store",
      "x-content-type-options": "nosniff",
    },
  });
}

function rpcError(id, code, message, status = 200) {
  return json({ jsonrpc: "2.0", id, error: { code, message } }, status);
}

function object(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function validId(value) {
  return value === null || typeof value === "string" || Number.isSafeInteger(value);
}

async function cancelBody(request) {
  try {
    await request.body?.cancel();
  } catch {
    // A rejected/aborted body must not expose raw runtime or request details.
  }
}

async function readBoundedJson(request) {
  const declared = request.headers.get("content-length");
  if (declared !== null && !/^\d+$/.test(declared)) {
    await cancelBody(request);
    return { error: rpcError(null, -32600, "Invalid content length", 400), rpcCode: -32600 };
  }
  if (declared !== null && Number(declared) > MAX_BODY_BYTES) {
    await cancelBody(request);
    return { error: rpcError(null, -32600, "Request body too large", 413), rpcCode: -32600 };
  }
  if (!request.body) {
    return { error: rpcError(null, -32700, "Parse error", 400), rpcCode: -32700 };
  }
  const reader = request.body.getReader();
  const bytes = new Uint8Array(MAX_BODY_BYTES);
  let size = 0;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      if (size + value.byteLength > MAX_BODY_BYTES) {
        try { await reader.cancel(); } catch { /* Keep the fixed size error. */ }
        return { error: rpcError(null, -32600, "Request body too large", 413), rpcCode: -32600 };
      }
      bytes.set(value, size);
      size += value.byteLength;
    }
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes.subarray(0, size));
    return { value: JSON.parse(text) };
  } catch {
    return { error: rpcError(null, -32700, "Parse error", 400), rpcCode: -32700 };
  } finally {
    reader.releaseLock();
  }
}

function sixDigitCode() {
  const values = new Uint32Array(1);
  const range = 900_000;
  const ceiling = Math.floor(2 ** 32 / range) * range;
  do {
    crypto.getRandomValues(values);
  } while (values[0] >= ceiling);
  return String(100_000 + (values[0] % range));
}

class DnsDiagnosticError extends Error {
  constructor(category) {
    super("DNS diagnostic unavailable");
    this.category = category;
  }
}

async function observeDns(hostname) {
  const controller = new AbortController();
  let reader;
  let timer;
  const deadline = new Promise((_, reject) => {
    timer = setTimeout(() => {
      controller.abort();
      if (reader) void reader.cancel().catch(() => {});
      reject(new DnsDiagnosticError("timeout"));
    }, DNS_TIMEOUT_MS);
  });
  const operation = async () => {
    // hostname comes only from DNS_HOSTS, never a request URL or parameter.
    const url = new URL("https://cloudflare-dns.com/dns-query");
    url.searchParams.set("name", hostname);
    url.searchParams.set("type", "A");
    const response = await fetch(url.toString(), {
      method: "GET", headers: { Accept: "application/dns-json" },
      credentials: "omit", redirect: "manual", signal: controller.signal,
    });
    if (controller.signal.aborted) {
      await cancelBody(response);
      throw new DnsDiagnosticError("timeout");
    }
    if (!response.ok) {
      await cancelBody(response);
      throw new DnsDiagnosticError("http_error");
    }
    const declared = response.headers.get("content-length");
    if (declared !== null && !/^\d+$/.test(declared)) {
      await cancelBody(response);
      throw new DnsDiagnosticError("invalid_response");
    }
    if (declared !== null && Number(declared) > MAX_BODY_BYTES) {
      await cancelBody(response);
      throw new DnsDiagnosticError("response_too_large");
    }
    if (!response.body) throw new DnsDiagnosticError("invalid_response");
    reader = response.body.getReader();
    const bytes = new Uint8Array(MAX_BODY_BYTES);
    let size = 0;
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (controller.signal.aborted) throw new DnsDiagnosticError("timeout");
        if (done) break;
        if (size + value.byteLength > MAX_BODY_BYTES) {
          try { await reader.cancel(); } catch { /* No upstream details. */ }
          throw new DnsDiagnosticError("response_too_large");
        }
        bytes.set(value, size);
        size += value.byteLength;
      }
    } finally {
      reader.releaseLock();
      reader = undefined;
    }
    let data;
    try {
      data = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes.subarray(0, size)));
    } catch {
      throw new DnsDiagnosticError("invalid_response");
    }
    if (!object(data) || !Number.isInteger(data.Status) || data.Status < 0 || data.Status > 4095 ||
        (data.Answer !== undefined && (!Array.isArray(data.Answer) ||
          !data.Answer.every((answer) => object(answer) && Number.isInteger(answer.type))))) {
      throw new DnsDiagnosticError("invalid_response");
    }
    // Never access or return Answer.data (IP addresses), Question, or raw text.
    return { hostname, DNSStatus: data.Status,
      Acount: (data.Answer ?? []).filter((answer) => answer.type === 1).length, error: null };
  };
  try {
    return await Promise.race([operation(), deadline]);
  } catch (error) {
    const category = controller.signal.aborted ? "timeout" :
      error instanceof DnsDiagnosticError ? error.category : "network_error";
    return { hostname, DNSStatus: null, Acount: null, error: category };
  } finally {
    clearTimeout(timer);
  }
}

async function dreameDns(request, url) {
  const reject = (error, status, headers) => {
    const response = json({ global_resolver_observation: "cloudflare_doh", error, results: [] }, status);
    if (headers) for (const [key, value] of Object.entries(headers)) response.headers.set(key, value);
    return response;
  };
  if (request.method !== "GET") return reject("method_not_allowed", 405, { allow: "GET" });
  if (!request.headers.get("oai-authenticated-user-id")?.trim()) return reject("authentication_required", 401);
  if (url.search !== "") return reject("parameters_not_allowed", 400);
  const results = await Promise.all(DNS_HOSTS.map(observeDns));
  return json({ global_resolver_observation: "cloudflare_doh", results });
}

function scheduleDnsProbe(env, ctx) {
  if (env?.DREAME_DNS_PROBE_ENABLED !== "1" || typeof ctx?.waitUntil !== "function") return;
  let scheduled = false;
  const task = Promise.resolve().then(async () => {
    if (!scheduled) return;
    const results = await Promise.all(DNS_HOSTS.map(observeDns));
    console.log(JSON.stringify({ event: "dreame_dns_probe",
      global_resolver_observation: "cloudflare_doh", results }));
  }).catch(() => {
    // Background diagnostics never reject the nonce response or expose raw
    // resolver/runtime details, even if the runtime cannot retain this task.
  });
  try {
    ctx.waitUntil(task);
    scheduled = true;
  } catch {
    // If registration fails, the queued task remains inert.
  }
}

async function mcp(request, env, ctx) {
  let methodCategory = "other";
  let toolCategory = "other";
  const finish = (response, rpcCode = 0) => {
    // Fixed categories only: never log dynamic names, arguments, request IDs,
    // identity, headers or input text, including on rejected requests.
    console.log(JSON.stringify({ event: "voice_probe_rpc", method: methodCategory,
      http_status: response.status, rpc_code: rpcCode, tool_category: toolCategory }));
    return response;
  };
  const reject = (id, code, message, status = 200) => finish(rpcError(id, code, message, status), code);
  const mediaType = request.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase();
  if (mediaType !== "application/json") {
    await cancelBody(request);
    return reject(null, -32600, "Expected application/json", 415);
  }
  const parsed = await readBoundedJson(request);
  if (parsed.error) return finish(parsed.error, parsed.rpcCode);
  const message = parsed.value;
  if (object(message)) {
    switch (message.method) {
      case "server/discover": methodCategory = "discover"; break;
      case "tools/list": methodCategory = "list"; break;
      case "tools/call": methodCategory = "call"; break;
      case "initialize": methodCategory = "initialize"; break;
      case "ping": methodCategory = "ping"; break;
    }
    if (message.method === "tools/call" && object(message.params) && message.params.name === TOOL_NAME) toolCategory = "expected";
  }
  if (!object(message) || message.jsonrpc !== "2.0" || typeof message.method !== "string" ||
      (Object.hasOwn(message, "id") && !validId(message.id))) {
    return reject(object(message) && validId(message.id) ? message.id : null, -32600, "Invalid request", 400);
  }
  // Notifications never execute a probe or generate an audit receipt.
  if (!Object.hasOwn(message, "id")) return finish(new Response(null, { status: 204 }));
  const { id, method } = message;
  if (message.params !== undefined && !object(message.params)) {
    return reject(id, -32602, "Invalid params");
  }
  if (method === "server/discover") {
    return finish(json({ jsonrpc: "2.0", id, result: {
      supportedVersions: SUPPORTED_VERSIONS, capabilities: { tools: {} },
    } }));
  }
  // Standard MCP 2025 lifecycle compatibility, alongside Sites discovery.
  // https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle
  if (method === "initialize") {
    const params = message.params;
    if (!object(params) || typeof params.protocolVersion !== "string" ||
        !object(params.capabilities) || !object(params.clientInfo) ||
        typeof params.clientInfo.name !== "string" || typeof params.clientInfo.version !== "string") {
      return reject(id, -32602, "Invalid initialize params");
    }
    const protocolVersion = SUPPORTED_VERSIONS.includes(params.protocolVersion) ? params.protocolVersion : PROTOCOL_VERSION;
    return finish(json({ jsonrpc: "2.0", id, result: {
      protocolVersion, capabilities: { tools: {} }, serverInfo: SERVER_INFO,
    } }));
  }
  // https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/ping
  if (method === "ping") return finish(json({ jsonrpc: "2.0", id, result: {} }));
  if (method === "tools/list") {
    return finish(json({ jsonrpc: "2.0", id, result: { tools: [tool] } }));
  }
  if (method !== "tools/call") return reject(id, -32601, "Method not found");

  const userId = request.headers.get("oai-authenticated-user-id");
  if (!userId?.trim()) return reject(id, -32001, "Authentication required", 401);
  const params = message.params;
  if (!object(params) || typeof params.name !== "string") {
    return reject(id, -32602, "Invalid params");
  }
  if (params.name !== TOOL_NAME) return reject(id, -32602, "Unknown tool");
  // CallToolRequestParams.arguments is optional. Only omission means {};
  // explicit null, arrays and any supplied properties remain invalid.
  // https://modelcontextprotocol.io/specification/2025-11-25/schema#calltoolrequestparams
  const args = Object.hasOwn(params, "arguments") ? params.arguments : {};
  if (!object(args) || Object.keys(args).length !== 0) {
    return reject(id, -32602, "Arguments must be an empty object");
  }

  let receipt;
  try {
    receipt = { code: sixDigitCode(), receipt: crypto.randomUUID(), time: new Date().toISOString() };
  } catch {
    return finish(json({ jsonrpc: "2.0", id, result: {
      content: [{ type: "text", text: "Probe unavailable" }], isError: true,
    } }));
  }
  // This is the complete audit allowlist. Never add headers, identity, IP,
  // request IDs, input text, or request bodies to this event.
  console.log(JSON.stringify({ event: "voice_probe_called", ...receipt }));
  scheduleDnsProbe(env, ctx);
  return finish(json({ jsonrpc: "2.0", id, result: {
    content: [{ type: "text", text: JSON.stringify(receipt) }], isError: false,
  } }));
}

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const path = url.pathname;
    if (path === "/" && (request.method === "GET" || request.method === "HEAD")) {
      return new Response(request.method === "HEAD" ? null : page, {
        headers: { "content-type": "text/html; charset=utf-8", "cache-control": "no-store" },
      });
    }
    if (path === "/diagnostics/dreame-dns") return dreameDns(request, url);
    if (path !== "/mcp") return new Response("Not found", { status: 404 });
    if (request.method !== "POST") {
      return new Response("Method not allowed", { status: 405, headers: { allow: "POST" } });
    }
    return mcp(request, env, ctx);
  },
};
