import assert from "node:assert/strict";
import test from "node:test";
import worker from "../worker/index.js";

const TOOL = "alisa_voice_probe_receipt";
const PRIVATE = "synthetic-background-private-marker";
const HOSTS = ["by.iot.dreame.tech", "eu.iot.dreame.tech"];
const AUTH = {
  "oai-authenticated-user-id": `${PRIVATE}-user`,
  "oai-authenticated-user-email": `${PRIVATE}@example.invalid`,
  authorization: `Bearer ${PRIVATE}`,
  "x-forwarded-for": "192.0.2.51",
};

function call(env, ctx, { method = "tools/call", params = { name: TOOL, arguments: {} }, headers = AUTH, notification = false } = {}) {
  const message = { jsonrpc: "2.0", method, params };
  if (!notification) message.id = `${PRIVATE}-request-id`;
  return worker.fetch(new Request("https://private-probe.invalid/mcp", {
    method: "POST", headers: { "content-type": "application/json", ...headers }, body: JSON.stringify(message),
  }), env, ctx);
}

function setup(t, implementation) {
  const logs = [];
  const calls = [];
  t.mock.method(console, "log", (line) => logs.push(JSON.parse(line)));
  t.mock.method(globalThis, "fetch", (url, options) => {
    calls.push({ url, options });
    return implementation(url, options);
  });
  t.after(() => {
    assert.equal(JSON.stringify(logs).includes(PRIVATE), false);
    assert.equal(JSON.stringify(logs).includes("192.0.2."), false);
    for (const event of logs.filter((entry) => entry.event === "dreame_dns_probe")) {
      assert.deepEqual(Object.keys(event).sort(), ["event", "global_resolver_observation", "results"]);
      assert.equal(event.global_resolver_observation, "cloudflare_doh");
      assert.deepEqual(event.results.map((entry) => entry.hostname), HOSTS);
      for (const result of event.results) {
        assert.deepEqual(Object.keys(result).sort(), ["Acount", "DNSStatus", "error", "hostname"]);
        assert.ok([null, "network_error", "timeout", "http_error", "response_too_large", "invalid_response"].includes(result.error));
      }
    }
  });
  return { logs, calls };
}

function assertNonce(body) {
  assert.equal(body.result.isError, false);
  const receipt = JSON.parse(body.result.content[0].text);
  assert.deepEqual(Object.keys(receipt).sort(), ["code", "receipt", "time"]);
  assert.match(receipt.code, /^[1-9][0-9]{5}$/);
}

test("background DNS is off unless the flag is the exact string 1", async (t) => {
  const { logs, calls } = setup(t, () => { throw new Error("No lookup permitted"); });
  const tasks = [];
  for (const value of [undefined, null, false, true, 1, 0, "", "0", "true", "01", "1 "]) {
    const env = value === undefined ? {} : { DREAME_DNS_PROBE_ENABLED: value };
    assertNonce(await (await call(env, { waitUntil: (task) => tasks.push(task) })).json());
  }
  assert.equal(tasks.length, 0);
  assert.equal(calls.length, 0);
  assert.equal(logs.filter((entry) => entry.event === "dreame_dns_probe").length, 0);
});

test("enabled DNS is waitUntil work and cannot delay the nonce response", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const { logs, calls } = setup(t, () => new Promise(() => {}));
  const tasks = [];
  const response = await call({ DREAME_DNS_PROBE_ENABLED: "1" }, { waitUntil: (task) => tasks.push(task) });
  assert.equal(response.status, 200);
  assertNonce(await response.json());
  assert.equal(tasks.length, 1);
  assert.equal(calls.length, 2);
  assert.equal(logs.filter((entry) => entry.event === "dreame_dns_probe").length, 0);
  assert.equal(logs.filter((entry) => entry.event === "voice_probe_called").length, 1);
  t.mock.timers.tick(6000);
  await tasks[0];
  const event = logs.find((entry) => entry.event === "dreame_dns_probe");
  assert.deepEqual(event.results, HOSTS.map((hostname) => ({ hostname, DNSStatus: null, Acount: null, error: "timeout" })));
  assert.ok(calls.every((entry) => entry.options.signal.aborted));
});

test("successful background observation has only fixed hosts/status/count/error", async (t) => {
  const { logs, calls } = setup(t, async (url) => Response.json(new URL(url).searchParams.get("name") === HOSTS[0]
    ? { Status: 3, Authority: [{ data: `${PRIVATE} 192.0.2.11` }] }
    : { Status: 0, Answer: [{ type: 5, data: PRIVATE }, { type: 1, data: "192.0.2.11" }] }));
  const tasks = [];
  assertNonce(await (await call({ DREAME_DNS_PROBE_ENABLED: "1", UNRELATED_SECRET: PRIVATE }, { waitUntil: (task) => tasks.push(task) })).json());
  await tasks[0];
  assert.deepEqual(logs.find((entry) => entry.event === "dreame_dns_probe"), {
    event: "dreame_dns_probe", global_resolver_observation: "cloudflare_doh", results: [
      { hostname: HOSTS[0], DNSStatus: 3, Acount: 0, error: null },
      { hostname: HOSTS[1], DNSStatus: 0, Acount: 1, error: null },
    ],
  });
  assert.equal(calls.length, 2);
  assert.deepEqual(calls.map((entry) => entry.url), HOSTS.map((hostname) => `https://cloudflare-dns.com/dns-query?name=${hostname}&type=A`));
  assert.ok(calls.every((entry) => entry.options.credentials === "omit" && entry.options.redirect === "manual"));
  assert.ok(calls.every((entry) => Object.keys(entry.options.headers).join() === "Accept"));
  const original = logs.find((entry) => entry.event === "voice_probe_called");
  assert.deepEqual(Object.keys(original).sort(), ["code", "event", "receipt", "time"]);
});

test("only successful authenticated nonce calls schedule diagnostics", async (t) => {
  const { calls } = setup(t, () => { throw new Error("No lookup permitted"); });
  const tasks = [];
  const env = { DREAME_DNS_PROBE_ENABLED: "1" };
  const ctx = { waitUntil: (task) => tasks.push(task) };
  for (const options of [
    { headers: {} },
    { params: { name: "other", arguments: {} } },
    { params: { name: TOOL, arguments: { input: PRIVATE } } },
    { method: "tools/list", params: {} },
    { method: "server/discover", params: {} },
    { notification: true },
  ]) {
    await call(env, ctx, options);
  }
  t.mock.method(crypto, "randomUUID", () => { throw new Error(PRIVATE); });
  const failed = await (await call(env, ctx)).json();
  assert.equal(failed.result.isError, true);
  assert.equal(tasks.length, 0);
  assert.equal(calls.length, 0);
});

test("network errors stay bounded and waitUntil work has no rejected promise", async (t) => {
  const { logs } = setup(t, async () => { throw new Error(`${PRIVATE}: 192.0.2.11`); });
  const tasks = [];
  assertNonce(await (await call({ DREAME_DNS_PROBE_ENABLED: "1" }, { waitUntil: (task) => tasks.push(task) })).json());
  await assert.doesNotReject(tasks[0]);
  assert.ok(logs.find((entry) => entry.event === "dreame_dns_probe").results.every((result) => result.error === "network_error"));
});

test("missing or failed waitUntil does not start diagnostics or fail the nonce", async (t) => {
  const { logs, calls } = setup(t, () => { throw new Error("No lookup permitted"); });
  for (const ctx of [{}, undefined, { waitUntil() { throw new Error(PRIVATE); } }]) {
    assertNonce(await (await call({ DREAME_DNS_PROBE_ENABLED: "1" }, ctx)).json());
  }
  await Promise.resolve();
  assert.equal(calls.length, 0);
  assert.equal(logs.filter((entry) => entry.event === "dreame_dns_probe").length, 0);
});
