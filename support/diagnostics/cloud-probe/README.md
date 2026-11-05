# Private Voice tool probe

A dependency-free Site MCP server with one read-only tool,
`alisa_voice_probe_receipt`. Each successful call returns a fresh six-digit
code, UUID receipt, and timestamp. A matching fixed audit event allows an
operator to verify an actual tool invocation independently of model speech.

The probe has no home-device access, credentials, or message actions.
Its optional DNS diagnostics query Cloudflare for two fixed public Dreame hosts. Sites provides the private audience boundary; execution additionally
requires its authenticated-user header. Discovery does not require that header.
No identity or request content is logged.

Build and validate with Node 22 or later:

```sh
node --test tests/*.test.mjs
node scripts/build-probe.mjs
node scripts/validate-artifact.mjs
```

The artifact contains only the Worker module and hosting manifest. Agent-only
logs live in ignored `temp/`; reusable tests are in `tests/`. Passing this probe in an ordinary text
chat does not establish support in a separate Voice client; test that route
independently against a new receipt.
