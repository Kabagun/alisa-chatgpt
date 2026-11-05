import assert from "node:assert/strict";
import test from "node:test";
import worker from "../worker/index.js";

const TOOL = "alisa_voice_probe_receipt";
const MAX = 16 * 1024;
const PRIVATE = "synthetic-private-marker";
const auth = {
  "oai-authenticated-user-id": `${PRIVATE}-user`,
  "oai-authenticated-user-email": `${PRIVATE}@example.invalid`,
  "oai-authenticated-user-full-name": `${PRIVATE}-name`,
  "x-forwarded-for": "192.0.2.1",
  authorization: `Bearer ${PRIVATE}-token`,
};

function request(message, headers = {}, raw = false) {
  return new Request("https://private-probe.invalid/mcp", {
    method: "POST",
    headers: { "content-type": "application/json", ...headers },
    body: raw ? message : JSON.stringify(message),
  });
}

async function rpc(method, params, headers = {}, id = 1) {
  const message = { jsonrpc: "2.0", id, method };
  if (params !== undefined) message.params = params;
  const response = await worker.fetch(request(message, headers), {}, {});
  return { response, body: await response.json() };
}

function audit(t) {
  const lines = [];
  t.mock.method(console, "log", (...args) => lines.push(args));
  t.after(() => {
    for (const args of lines) {
      assert.equal(args.length, 1);
      const event = JSON.parse(args[0]);
      if (event.event === "voice_probe_called") {
        assert.deepEqual(Object.keys(event).sort(), ["code", "event", "receipt", "time"]);
      } else {
        assert.equal(event.event, "voice_probe_rpc");
        assert.deepEqual(Object.keys(event).sort(), ["event", "http_status", "method", "rpc_code", "tool_category"]);
        assert.ok(["discover", "list", "call", "initialize", "ping", "other"].includes(event.method));
        assert.ok([200, 204, 400, 401, 413, 415].includes(event.http_status));
        assert.ok([0, -32600, -32601, -32602, -32700, -32001].includes(event.rpc_code));
        assert.ok(["expected", "other"].includes(event.tool_category));
      }
    }
    assert.equal(JSON.stringify(lines).includes(PRIVATE), false);
    assert.equal(JSON.stringify(lines).includes("192.0.2.1"), false);
  });
  return lines;
}

function receiptLogs(lines) {
  return lines.filter((args) => JSON.parse(args[0]).event === "voice_probe_called");
}

function rpcLogs(lines) {
  return lines.filter((args) => JSON.parse(args[0]).event === "voice_probe_rpc").map((args) => JSON.parse(args[0]));
}

test("2026 discovery and tool list do not require user identity", async (t) => {
  const logs = audit(t);
  const discovered = await rpc("server/discover", {
    supportedVersions: ["2026-07-28"], clientInfo: { name: "synthetic-test" },
  });
  assert.equal(discovered.response.status, 200);
  assert.deepEqual(discovered.body, {
    jsonrpc: "2.0", id: 1,
    result: { supportedVersions: ["2026-07-28", "2025-11-25"], capabilities: { tools: {} } },
  });
  const listed = await rpc("tools/list", {});
  assert.equal(listed.body.result.tools.length, 1);
  const tool = listed.body.result.tools[0];
  assert.equal(tool.name, TOOL);
  assert.deepEqual(tool.inputSchema, { type: "object", properties: {}, additionalProperties: false });
  assert.deepEqual(tool.annotations, {
    readOnlyHint: true, destructiveHint: false, idempotentHint: false, openWorldHint: false,
  });
  assert.equal(receiptLogs(logs).length, 0);
  assert.deepEqual(rpcLogs(logs).map((event) => event.method), ["discover", "list"]);
});

test("authenticated calls return fresh receipts and only the audit allowlist", async (t) => {
  const logs = audit(t);
  let networkCalls = 0;
  t.mock.method(globalThis, "fetch", () => { networkCalls++; throw new Error("No external requests allowed"); });
  const receipts = new Set();
  const codes = new Set();
  const earliest = Date.now();
  for (let i = 0; i < 12; i++) {
    const { response, body } = await rpc("tools/call", { name: TOOL, arguments: {} }, auth, `${PRIVATE}-${i}`);
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("cache-control"), "no-store");
    assert.equal(body.result.isError, false);
    assert.equal(body.result.content.length, 1);
    assert.equal(body.result.content[0].type, "text");
    const receipt = JSON.parse(body.result.content[0].text);
    assert.deepEqual(Object.keys(receipt).sort(), ["code", "receipt", "time"]);
    assert.match(receipt.code, /^[1-9][0-9]{5}$/);
    assert.match(receipt.receipt, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    assert.ok(Date.parse(receipt.time) >= earliest && Date.parse(receipt.time) <= Date.now());
    receipts.add(receipt.receipt);
    codes.add(receipt.code);
    const entry = receiptLogs(logs)[i];
    assert.equal(entry.length, 1);
    const event = JSON.parse(entry[0]);
    assert.deepEqual(event, { event: "voice_probe_called", ...receipt });
    assert.deepEqual(Object.keys(event).sort(), ["code", "event", "receipt", "time"]);
  }
  assert.equal(receipts.size, 12);
  assert.ok(codes.size > 1, "codes must not be a fixed response");
  assert.equal(networkCalls, 0);
  assert.equal(receiptLogs(logs).length, 12);
  assert.equal(rpcLogs(logs).length, 12);
  assert.equal(JSON.stringify(logs).includes(PRIVATE), false);
  assert.equal(JSON.stringify(logs).includes("192.0.2.1"), false);
});

test("missing or blank authenticated user cannot execute tools", async (t) => {
  const logs = audit(t);
  for (const headers of [{}, { "oai-authenticated-user-id": "   " },
    { "oai-authenticated-user-email": "synthetic@example.invalid" }]) {
    const { response, body } = await rpc("tools/call", { name: TOOL, arguments: {} }, headers);
    assert.equal(response.status, 401);
    assert.equal(body.error.code, -32001);
    assert.equal(body.error.message, "Authentication required");
  }
  assert.equal(receiptLogs(logs).length, 0);
  assert.ok(rpcLogs(logs).every((event) => event.http_status === 401 && event.rpc_code === -32001));
});

test("unknown methods/tools and nonempty or malformed arguments are rejected", async (t) => {
  const logs = audit(t);
  const unknown = await rpc(`${PRIVATE}-unknown-method`, {}, auth);
  assert.equal(unknown.body.error.code, -32601);
  const other = await rpc("tools/call", { name: `${PRIVATE}-unknown`, arguments: {} }, auth);
  assert.equal(other.body.error.code, -32602);
  assert.equal(JSON.stringify(other.body).includes(PRIVATE), false);
  for (const args of [null, [], "", 0, false, { command: "clean_kitchen" }, { nonce: PRIVATE }]) {
    const params = { name: TOOL };
    if (args !== undefined) params.arguments = args;
    const { body } = await rpc("tools/call", params, auth);
    assert.equal(body.error.code, -32602);
    assert.equal(body.error.message, "Arguments must be an empty object");
    assert.equal(JSON.stringify(body).includes(PRIVATE), false);
  }
  for (const params of [null, [], "private params", {}, { arguments: {} }, { name: 1, arguments: {} }]) {
    const { body } = await rpc("tools/call", params, auth);
    assert.equal(body.error.code, -32602);
  }
  assert.equal(receiptLogs(logs).length, 0);
});

test("valid notifications return empty 204 and never execute a tool", async (t) => {
  const logs = audit(t);
  for (const message of [
    { jsonrpc: "2.0", method: "notifications/initialized" },
    { jsonrpc: "2.0", method: "unknown", params: {} },
    { jsonrpc: "2.0", method: "tools/call", params: { name: TOOL, arguments: {} } },
  ]) {
    const response = await worker.fetch(request(message, auth), {}, {});
    assert.equal(response.status, 204);
    assert.equal(await response.text(), "");
  }
  assert.equal(receiptLogs(logs).length, 0);
  assert.ok(rpcLogs(logs).every((event) => event.http_status === 204));
});

test("malformed JSON/UTF-8 and invalid JSON-RPC have fixed errors", async (t) => {
  const logs = audit(t);
  for (const raw of ["", "{", `not-json-${PRIVATE}`]) {
    const response = await worker.fetch(request(raw, auth, true), {}, {});
    const body = await response.json();
    assert.equal(response.status, 400);
    assert.equal(body.error.code, -32700);
    assert.equal(body.error.message, "Parse error");
    assert.equal(JSON.stringify(body).includes(PRIVATE), false);
  }
  const invalidUtf8 = new Request("https://private-probe.invalid/mcp", {
    method: "POST", headers: { "content-type": "application/json" }, body: new Uint8Array([0xff, 0xfe]),
  });
  assert.equal((await (await worker.fetch(invalidUtf8, {}, {})).json()).error.code, -32700);
  for (const message of [null, [], 1, "text", {}, { jsonrpc: "1.0", id: 1, method: "tools/list" },
    { jsonrpc: "2.0", id: {}, method: "tools/list" }, { jsonrpc: "2.0", id: 1.5, method: "tools/list" },
    { jsonrpc: "2.0", id: 1, method: 9 }]) {
    const response = await worker.fetch(request(message), {}, {});
    assert.equal(response.status, 400);
    assert.equal((await response.json()).error.code, -32600);
  }
  assert.equal(receiptLogs(logs).length, 0);
});

test("body limit is byte-based, exact at 16 KiB, and independent of declared length", async (t) => {
  const logs = audit(t);
  const base = JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" });
  const exact = await worker.fetch(request(base.padEnd(MAX, " "), {}, true), {}, {});
  assert.equal(exact.status, 200);
  assert.equal((await exact.json()).result.tools[0].name, TOOL);
  for (const body of [base.padEnd(MAX + 1, " "), JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list", params: { padding: "🙂".repeat(5000) } })]) {
    const response = await worker.fetch(request(body, { "content-length": "2" }, true), {}, {});
    assert.equal(response.status, 413);
    assert.equal((await response.json()).error.message, "Request body too large");
  }
  let canceled = 0;
  const stream = new ReadableStream({
    start(controller) { controller.enqueue(new Uint8Array(MAX + 1)); },
    cancel() { canceled++; },
  });
  const response = await worker.fetch(new Request("https://private-probe.invalid/mcp", {
    method: "POST", headers: { "content-type": "application/json" }, body: stream, duplex: "half",
  }), {}, {});
  assert.equal(response.status, 413);
  assert.equal(canceled, 1);
  for (const length of ["16385", "99999999999999999999999999"]) {
    const large = await worker.fetch(request("{}", { "content-length": length }, true), {}, {});
    assert.equal(large.status, 413);
  }
  const malformed = await worker.fetch(request("{}", { "content-length": "invalid" }, true), {}, {});
  assert.equal(malformed.status, 400);
  assert.equal((await malformed.json()).error.message, "Invalid content length");
  assert.equal(receiptLogs(logs).length, 0);
});

test("unreadable input is sanitized and emits no audit event", async (t) => {
  const logs = audit(t);
  const stream = new ReadableStream({ start(controller) { controller.error(new Error(PRIVATE)); } });
  const response = await worker.fetch(new Request("https://private-probe.invalid/mcp", {
    method: "POST", headers: { "content-type": "application/json" }, body: stream, duplex: "half",
  }), {}, {});
  assert.equal(response.status, 400);
  const body = await response.json();
  assert.equal(body.error.message, "Parse error");
  assert.equal(JSON.stringify(body).includes(PRIVATE), false);
  assert.equal(receiptLogs(logs).length, 0);
});

test("tool runtime failures use isError without disclosing raw details", async (t) => {
  const logs = audit(t);
  t.mock.method(crypto, "randomUUID", () => { throw new Error(PRIVATE); });
  const { response, body } = await rpc("tools/call", { name: TOOL, arguments: {} }, auth);
  assert.equal(response.status, 200);
  assert.equal(body.result.isError, true);
  assert.deepEqual(body.result.content, [{ type: "text", text: "Probe unavailable" }]);
  assert.equal(JSON.stringify(body).includes(PRIVATE), false);
  assert.equal(receiptLogs(logs).length, 0);
});

test("landing page is static, routes are bounded, and POST uses JSON media type", async (t) => {
  const logs = audit(t);
  const page = await worker.fetch(new Request("https://private-probe.invalid/"), {}, {});
  assert.equal(page.status, 200);
  const html = await page.text();
  assert.match(html, /Private Voice tool probe/);
  assert.doesNotMatch(html, /<script|<button|<form/i);
  const head = await worker.fetch(new Request("https://private-probe.invalid/", { method: "HEAD" }), {}, {});
  assert.equal(head.status, 200);
  assert.equal(await head.text(), "");
  const missing = await worker.fetch(new Request("https://private-probe.invalid/unknown"), {}, {});
  assert.equal(missing.status, 404);
  const get = await worker.fetch(new Request("https://private-probe.invalid/mcp"), {}, {});
  assert.equal(get.status, 405);
  assert.equal(get.headers.get("allow"), "POST");
  const type = await worker.fetch(request("{}", { "content-type": "text/plain" }, true), {}, {});
  assert.equal(type.status, 415);
  assert.equal((await type.json()).error.message, "Expected application/json");
  const parameterized = await worker.fetch(request({ jsonrpc: "2.0", id: null, method: "server/discover" }, { "content-type": "Application/JSON; charset=UTF-8" }), {}, {});
  assert.equal(parameterized.status, 200);
  assert.equal((await parameterized.json()).id, null);
  assert.equal(receiptLogs(logs).length, 0);
});

test("omitted arguments are the same empty input as explicit {}", async (t) => {
  const logs = audit(t);
  for (const params of [{ name: TOOL }, { name: TOOL, arguments: {} }]) {
    const { response, body } = await rpc("tools/call", params, auth);
    assert.equal(response.status, 200);
    assert.equal(body.result.isError, false);
    assert.match(JSON.parse(body.result.content[0].text).code, /^[1-9][0-9]{5}$/);
  }
  assert.equal(receiptLogs(logs).length, 2);
  assert.ok(rpcLogs(logs).every((event) => event.method === "call" && event.tool_category === "expected" && event.rpc_code === 0));
});

test("2025 initialize negotiates versions and ping has an empty result", async (t) => {
  const logs = audit(t);
  for (const offered of ["2025-11-25", "2026-07-28", `${PRIVATE}-unsupported-version`]) {
    const { response, body } = await rpc("initialize", {
      protocolVersion: offered, capabilities: {}, clientInfo: { name: PRIVATE, version: PRIVATE },
    });
    assert.equal(response.status, 200);
    assert.equal(body.result.protocolVersion, offered === "2025-11-25" ? "2025-11-25" : "2026-07-28");
    assert.deepEqual(body.result.capabilities, { tools: {} });
    assert.deepEqual(body.result.serverInfo, { name: "alisa-voice-probe", version: "1.1.0" });
    assert.equal(JSON.stringify(body).includes(PRIVATE), false);
  }
  for (const params of [undefined, {}, { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "test" } }]) {
    const { body } = await rpc("initialize", params);
    assert.equal(body.error.code, -32602);
  }
  for (const params of [undefined, {}]) {
    const { body } = await rpc("ping", params);
    assert.deepEqual(body.result, {});
  }
  assert.equal(receiptLogs(logs).length, 0);
  assert.ok(rpcLogs(logs).some((event) => event.method === "initialize"));
  assert.ok(rpcLogs(logs).some((event) => event.method === "ping"));
});

test("RPC audit categories identify failures without dynamic input or identifiers", async (t) => {
  const logs = audit(t);
  await rpc(`${PRIVATE}-method`, { name: `${PRIVATE}-tool` }, auth, `${PRIVATE}-request-id`);
  await rpc("tools/call", { name: `${PRIVATE}-tool`, arguments: {} }, auth, `${PRIVATE}-request-id`);
  await rpc("tools/call", { name: TOOL, arguments: { [PRIVATE]: PRIVATE } }, auth, `${PRIVATE}-request-id`);
  await rpc("tools/call", { name: TOOL }, {}, `${PRIVATE}-request-id`);
  assert.deepEqual(rpcLogs(logs), [
    { event: "voice_probe_rpc", method: "other", http_status: 200, rpc_code: -32601, tool_category: "other" },
    { event: "voice_probe_rpc", method: "call", http_status: 200, rpc_code: -32602, tool_category: "other" },
    { event: "voice_probe_rpc", method: "call", http_status: 200, rpc_code: -32602, tool_category: "expected" },
    { event: "voice_probe_rpc", method: "call", http_status: 401, rpc_code: -32001, tool_category: "expected" },
  ]);
  assert.equal(receiptLogs(logs).length, 0);
});
