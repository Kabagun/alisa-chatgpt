import assert from "node:assert/strict";
import test from "node:test";
import worker from "../worker/index.js";

const HOSTS = ["by.iot.dreame.tech", "eu.iot.dreame.tech"];
const PRIVATE = "synthetic-private-dns-marker";
const AUTH = {
  "oai-authenticated-user-id": `${PRIVATE}-user`,
  "oai-authenticated-user-email": `${PRIVATE}@example.invalid`,
  authorization: `Bearer ${PRIVATE}`,
  "x-forwarded-for": "192.0.2.51",
};

function diagnostic({ headers = AUTH, query = "", method = "GET" } = {}) {
  return worker.fetch(new Request(`https://private-probe.invalid/diagnostics/dreame-dns${query}`, {
    method, headers,
  }), {}, {});
}

function mocks(t, implementation) {
  const calls = [];
  const logs = [];
  t.mock.method(console, "log", (...args) => logs.push(args));
  t.mock.method(globalThis, "fetch", (url, options) => {
    calls.push({ url, options });
    return implementation(url, options);
  });
  t.after(() => assert.deepEqual(logs, [], "DNS diagnostics must not emit raw data or identity logs"));
  return calls;
}

function expectedError(category) {
  return {
    global_resolver_observation: "cloudflare_doh",
    results: HOSTS.map((hostname) => ({ hostname, DNSStatus: null, Acount: null, error: category })),
  };
}

test("DNS requires authenticated GET with no parameters and makes no rejected lookups", async (t) => {
  const calls = mocks(t, () => { throw new Error("Must not look up DNS"); });
  for (const headers of [{}, { "oai-authenticated-user-id": " " }, { "oai-authenticated-user-email": PRIVATE }]) {
    const response = await diagnostic({ headers });
    assert.equal(response.status, 401);
    assert.deepEqual(await response.json(), {
      global_resolver_observation: "cloudflare_doh", error: "authentication_required", results: [],
    });
  }
  for (const query of ["?name=example.invalid", "?host=example.invalid&type=AAAA", "?unused=", "?name", "?%6eame=example.invalid"]) {
    const response = await diagnostic({ query });
    assert.equal(response.status, 400);
    assert.equal((await response.json()).error, "parameters_not_allowed");
  }
  for (const method of ["POST", "HEAD", "DELETE"]) {
    const response = await diagnostic({ method });
    assert.equal(response.status, 405);
    assert.equal(response.headers.get("allow"), "GET");
    assert.equal((await response.json()).error, "method_not_allowed");
  }
  assert.equal(calls.length, 0);
});

test("only the two fixed A queries reach Cloudflare without forwarded credentials", async (t) => {
  const calls = mocks(t, async (url) => {
    const hostname = new URL(url).searchParams.get("name");
    return Response.json(hostname === HOSTS[0] ? {
      Status: 3, Question: [{ name: PRIVATE, type: 1 }], Authority: [{ data: "192.0.2.11" }],
    } : {
      Status: 0,
      Answer: [{ name: PRIVATE, type: 5, data: PRIVATE },
        { name: PRIVATE, type: 1, data: "192.0.2.11" }, { name: PRIVATE, type: 1, data: "192.0.2.12" }],
    });
  });
  const response = await diagnostic();
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("cache-control"), "no-store");
  const result = await response.json();
  assert.deepEqual(result, {
    global_resolver_observation: "cloudflare_doh",
    results: [
      { hostname: HOSTS[0], DNSStatus: 3, Acount: 0, error: null },
      { hostname: HOSTS[1], DNSStatus: 0, Acount: 2, error: null },
    ],
  });
  assert.equal(calls.length, 2);
  assert.deepEqual(calls.map((call) => call.url), HOSTS.map((hostname) =>
    `https://cloudflare-dns.com/dns-query?name=${hostname}&type=A`));
  for (const { options } of calls) {
    assert.equal(options.method, "GET");
    assert.equal(options.credentials, "omit");
    assert.equal(options.redirect, "manual");
    assert.deepEqual(Object.fromEntries(new Headers(options.headers)), { accept: "application/dns-json" });
    assert.ok(options.signal instanceof AbortSignal);
  }
  assert.equal(JSON.stringify(result).includes(PRIVATE), false);
  assert.equal(JSON.stringify(result).includes("192.0.2."), false);
});

test("resolver HTTP redirects/errors are not followed or disclosed", async (t) => {
  const calls = mocks(t, async () => new Response(PRIVATE, {
    status: 302, headers: { location: `https://${PRIVATE}.invalid/`, "x-private": PRIVATE },
  }));
  const response = await diagnostic();
  assert.deepEqual(await response.json(), expectedError("http_error"));
  assert.equal(calls.length, 2);
  assert.ok(calls.every((call) => call.options.redirect === "manual"));
});

test("network failures are fixed enums without raw details", async (t) => {
  mocks(t, async () => { throw new Error(`${PRIVATE}: 192.0.2.11`); });
  const response = await diagnostic();
  assert.deepEqual(await response.json(), expectedError("network_error"));
});

test("malformed resolver responses cannot expose raw JSON or addresses", async (t) => {
  let body;
  mocks(t, async () => typeof body === "string" ? new Response(body) : Response.json(body));
  for (body of [PRIVATE, null, [], {}, { Status: "0" }, { Status: -1 }, { Status: 4096 },
    { Status: 0, Answer: PRIVATE }, { Status: 0, Answer: null }, { Status: 0, Answer: [PRIVATE] },
    { Status: 0, Answer: [{ type: "1", data: "192.0.2.11" }] }]) {
    const response = await diagnostic();
    assert.deepEqual(await response.json(), expectedError("invalid_response"));
  }
});

test("resolver body is capped at 16 KiB by actual bytes and declared length", async (t) => {
  let makeResponse;
  mocks(t, async () => makeResponse());
  const valid = JSON.stringify({ Status: 0, Answer: [] });
  makeResponse = () => new Response(valid.padEnd(16 * 1024, " "));
  const exact = await (await diagnostic()).json();
  assert.ok(exact.results.every((result) => result.DNSStatus === 0 && result.Acount === 0 && result.error === null));
  for (makeResponse of [
    () => new Response(valid.padEnd(16 * 1024 + 1, " "), { headers: { "content-length": "2" } }),
    () => new Response(JSON.stringify({ Status: 0, ignored: "🙂".repeat(5000) })),
    () => new Response(valid, { headers: { "content-length": "999999999999999999" } }),
  ]) {
    assert.deepEqual(await (await diagnostic()).json(), expectedError("response_too_large"));
  }
  makeResponse = () => new Response(valid, { headers: { "content-length": PRIVATE } });
  assert.deepEqual(await (await diagnostic()).json(), expectedError("invalid_response"));
  makeResponse = () => new Response(new Uint8Array([0xff, 0xfe]));
  assert.deepEqual(await (await diagnostic()).json(), expectedError("invalid_response"));
});

test("six-second deadline bounds even a mocked fetch ignoring abort", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const calls = mocks(t, () => new Promise(() => {}));
  const pending = diagnostic();
  assert.equal(calls.length, 2);
  let settled = false;
  pending.then(() => { settled = true; });
  t.mock.timers.tick(5999);
  await Promise.resolve();
  assert.equal(settled, false);
  t.mock.timers.tick(1);
  assert.deepEqual(await (await pending).json(), expectedError("timeout"));
  assert.ok(calls.every((call) => call.options.signal.aborted));
});

test("six-second deadline also cancels stalled resolver response bodies", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let canceled = 0;
  mocks(t, async () => new Response(new ReadableStream({ cancel() { canceled++; } })));
  const pending = diagnostic();
  await Promise.resolve();
  await Promise.resolve();
  t.mock.timers.tick(6000);
  assert.deepEqual(await (await pending).json(), expectedError("timeout"));
  assert.equal(canceled, 2);
});

test("landing link never triggers a DNS lookup", async (t) => {
  const calls = mocks(t, () => { throw new Error("Must not look up DNS"); });
  const response = await worker.fetch(new Request("https://private-probe.invalid/"), {}, {});
  const html = await response.text();
  assert.match(html, /href="\/diagnostics\/dreame-dns"/);
  assert.doesNotMatch(html, /<script|<button|<form/i);
  assert.equal(calls.length, 0);
});
