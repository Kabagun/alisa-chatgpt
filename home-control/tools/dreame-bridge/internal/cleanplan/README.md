# Single-room DRAFT planner

This package is a pure standard-library planner. It does not read files or clocks,
open connections, dispatch actions, grant permission, or claim firmware acceptance.
Its only supported model is `dreame.vacuum.r2422`, using the pinned Tasshack
`f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585` room-command baseline.

## API

- `Build(Observations) (*Draft, error)` checks backend observations and returns an
  opaque draft. The zero-value draft cannot produce a preview or logical payload.
- `(*Draft).Preview() (Preview, error)` returns concrete scalar mode, fan, plain
  water, repeat, order, battery and settings source. It always says `DRAFT` and
  `firmware_acceptance: unverified`, and contains no account, device, map, snapshot
  or room identifier. The caller separately binds the friendly room/target name.
- `(*Draft).LogicalPayloadJSON(nowMillis) ([]byte, error)` rechecks age and only
  serializes the fixed logical START_CUSTOM RPC for offline review/tests. It has
  no transport, endpoint, signature, credentials or approval input.
- `ErrorCode(error) Code` and `(*Error).Code()` return fixed safe codes. Errors
  never carry an underlying error or values from the observations.

`Observations` is an internal Go interface, not an MCP or JSON input schema.
Construct it only in a trusted adapter from authenticated catalog and current-map
reader results. JSON unmarshalling into observations or a draft is rejected and
clears the destination. The package detects inconsistent facts; it cannot
authenticate the producer of an in-process Go struct. Do not turn user claims
into these evidence fields.

All proof objects and drafts redact normal Go formatting, including `%+v` and
`%#v`, and JSON formatting. Explicit draft JSON is the safe preview, never the
logical payload. Obtain the latter only through the named method and keep those
private bytes out of logs.

## Required trusted facts

`Binding` contains bounded opaque account/device identities and the exact model.
Identities and snapshot IDs allow only ASCII letters, digits, `-`, `_`, `.` and
at most 128 bytes; do not supply emails, credentials, URLs, references or raw maps.
All catalog/map/room/state/settings facts repeat the same binding and reader-owned
snapshot ID. Catalog `Matches` counts exact target matches, not the account's
total number of devices. Both catalog and room resolution must have one match.

The map must come from a response-coupled refresh or a reader-proven advanced
cached read, contain a complete I frame (73), use the pinned plain model's version
1/2 contract, have observed saved status 2 and known valid format markers, and be
nonempty with no temporary/new/restored flags. Current saved-map linkage must
equal the selected saved map. Map IDs are **canonical signed-16 decimal strings**,
including negative values and zero; segment IDs are separately limited to 1..63.
The room must belong to both current and selected saved maps and be visible and
mapped. No room category, unique room identifier or array position replaces its
actual segment ID.

Every snapshot supplies injected backend `ReadAtMillis`; `NowMillis` is injected
too. The 30-second maximum age is an explicit engineering guard, not a vendor
SLA. Missing/future/stale timestamps or a reader's `Stale` flag fail closed. The
logical payload method rechecks the oldest observation and refuses clock rollback
before the build time. A preview may remain available for an older owner review;
this does not preserve freshness or authorize later use.

For required scalar reads, `Quality: Observed` is necessary even when the value
is zero. `Missing`, `Unsupported`, `Invalid`, and unknown quality values do not
become valid settings or zero task/error states.

## Resting state and plain settings

The first plan requires observed state/status/task/error/battery/charging and a
false cleaning-paused observation. Task and error must be zero. The conservative
resting allowlist is state 2/6/13, status 0/6/14/17, charging 1/2/3, with charging
consistency checks. Working, returning, paused, mapping, repair, upgrade, and
draining states therefore cannot become a new room task. Charging at rest is
allowed; charging with a pending task is rejected.

Drainage can be observed idle, or explicitly unsupported by the pinned model /
trusted reader. Its missing state is not silently ignored, and an unsupported
observation must not carry a contradictory nonzero value. Battery must be
observed in 0..100; the planner intentionally invents no start threshold. A draft
with a low battery is still only a draft requiring concrete review, not a claim
that the device can or should start.

Settings must explicitly use the plain water-volume encoding. Global fan 0..3,
water 1..3, mode 0..2, tank and customized-cleaning provenance are required.
Mode 0 requires observed tank 0; modes 1/2 require tank 1 or 10. No mode, tank,
map or persistent setting is changed to make a combination valid. Unsupported
advanced combinations fail rather than being coerced.

- Observed customized value 0 uses genuine observed global fan/water. Explicit
  non-support with value zero also means the capability is absent/disabled;
  `Missing` is not equivalent to non-support. Omit `PerRoom` in this branch.
- Observed customized value 1 requires `PerRoom` with matching binding, snapshot,
  selected map and segment, and `GenuineObservedEntry` provenance. Decoder-created
  fallback entries are rejected. Fan, raw cleanset water and repeat must each be
  observed. Raw water 2..4 is converted to row water 1..3; already converted row
  water must not be passed as raw input. A per-room mode observation is an
  unsupported advanced feature for this initial scope.
- This product plan is explicitly one pass and order 1. Genuine per-room repeat
  must also be 1; another count is rejected rather than silently changed. This
  is a scope limit, not a firmware repeat-range assertion. When customized
  cleaning is disabled, one pass is the explicit product plan, not a guessed
  observed preference.

The exact payload is method `action`, service 4/action 1, inputs PIID 1/status 18
and PIID 10/compact `selects` JSON with exactly one five-integer row. It contains
no map selector, mode-setting operation, timestamp, whole-home fallback, generic
RPC surface or extra action. Mode stays the already observed global mode.

## Integration boundary

No helper, MCP schema, manifest or distribution build is changed by this package.
A future dispatch layer must rebuild a fresh draft immediately before dispatch
and compare the concrete target/settings against the approved review. It must
not automatically accept changed values, reuse a preview as permission, or retry
an ambiguous start. Map evidence is distinct from firmware acceptance; the latter
remains unverified here. The first physical operation needs the concrete review
and approval outside this package.

## Checks

Using the existing pinned Go 1.27.3 WSL environment with downloads disabled:

```text
go test -count=1 -cover ./internal/cleanplan
go vet ./internal/cleanplan
```

Tests cover the exact payload and privacy boundaries, signed map IDs, identity /
snapshot collisions, missing and unsupported observations, stale/future reads,
map flags, hidden/unmapped rooms, non-resting states, zero-valued legitimate
settings, mode/tank conflicts, genuine versus synthesized per-room settings,
raw-water conversion, one-pass limits, opaque/zero drafts, denied JSON input and
serialization freshness. No live credentials or robot requests are used.
