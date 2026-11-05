# Protected single-room control protocol

Normal reader, fixture, issued-session import and stdin-session modes remain
read-only and reject `clean_rooms`. The separate `--control` mode requires a
protected `--session` file and a staged protected `--map-profile`; it cannot be
combined with fixture/import/stdin-session modes. Its bounded strict JSON has
exactly schema1, op `clean_rooms`, device_id, canonical signed16 map_id and
exactly one canonical segment1..63 in room_ids. No caller verification facts,
approval flag, URL, method, settings override, profile or payload is accepted.

The backend must authorize its existing `home:control` scope before entering
this path. Implementing this protocol does not approve a live trial. The first
real operation still requires the parent's concrete owner-review step.

## Preview and revalidation

A successful normal protected `rooms` read can atomically save a private
per-device preview under the account's existing current-UID0700 directory.
Regular current-UID0600 files, no-follow reads/locks, bounded JSON, atomic rename
and directory sync retain the session's protection boundary. The record holds
only private target/map/preview values and an opaque SHA256 change fingerprint;
there are no credentials, raw maps, keys, storage hosts or object URLs. Filenames
use a device hash. The fingerprint includes authenticated owner/device/model,
the saved-frame and descriptor hashes, selected saved-room/name metadata and
concrete previews. Current frame IDs, robot pose and signed URLs are excluded.
This detects changes; it is not a vendor-integrity or exactly-once claim.

`cleaning_readiness.verified_for_control=true` means an implemented narrow writer,
valid current-map proof, fresh valid planner result and successfully saved
server-held preview are available. It does not grant OAuth scope or owner
approval and does not claim firmware acceptance. Map-level
`verified_for_control` stays false; `Preview.firmware_acceptance` stays
`unverified`.

Control acquires the per-device lock and loads the existing preview first. The
preview must be at most five minutes old. It then rebuilds all authenticated
ownership/map/state/settings facts through the existing reader and pure planner,
with its thirty-second observation bound. This fresh control read explicitly
cannot save or replace the expected preview. A changed fingerprint, map, room,
settings, ownership, missing genuine settings, unsupported model or busy state
returns `not_sent`; there is no floor/mode/settings/whole-home fallback.

## One physical send

Before transmission, the helper atomically records a dispatch marker. All new
physical starts on that device are suppressed for sixty seconds. The marker is
retained after transport errors, process cancellation or ambiguous replies.
This engineering cooldown is not an exactly-once guarantee.

Only a fresh opaque cleanplan draft supplies the logical START_CUSTOM payload:
service4/action1, status18, PIID10 with exactly one
`selects[[segment,1,observedFan,observedWater,1]]` row. The fixed owned shard,
request identity and existing signing transport wrap it. No preparatory write or
other physical action exists. The adapter invokes control once and never enters
its token-refresh/retry loop afterward; process/JSON failures become a normal
structured `unknown` outcome. Credential refresh is permitted only before
control invocation, during the fresh device-catalog read.

The device acknowledgement must contain numeric result code0 in an object or
exactly-one-object list. Returned identities, when present, must match the fixed
request. Duplicate/ill-typed/ambiguous replies are unknown, not acceptance.
Explicit nonzero device result is `rejected`. Cloud/HTTP success alone is not
device acceptance. No raw response or logical payload enters stdout.

## Bounded start evidence

The helper's overall control deadline is28s, inside the adapter30s deadline;
requests keep the existing6s cap. At least8s must remain before physical send.
After a minimum2s delay, one fixed status6+isolated-paused read can establish
status18/task3/error0/paused0 and mode-matching working state1/7/12. This proves
a segment-task kind, not the room identity.

Only when that running evidence exists may one optional fixed full-I map
solicitation run. It reuses the existing exact-model active-map, descriptor and
saved-linkage proof and the same aggregate4GET/7MiB budget. Only fresh outer raw
`Frame.Selection` (`sa`) with exactly the requested segment can prove room
selection. No optimistic, carried-over or nested selection is used. No repeated
cleaning command, map switch, mode change, stop or resume is possible.

Outcomes distinguish `not_sent`, `accepted`, `rejected`, `unknown`,
`segment_task_running_room_unconfirmed`, `started`, and
`observed_running_reply_unknown`. `started` requires valid acceptance plus both
running-task and exact raw-room/map evidence. A lost reply with matching observed
activity retains its causal uncertainty. No outcome claims cleaning completion.

Tests use synthetic TLS/vendor responses and fake helper executables only. They
cover one-send payloads, marker-before-send/cooldown, snapshot change/expiry,
no snapshot promotion, safe acknowledgement shapes, optional raw selection,
post-send auth failures, process timeout/redaction/no retry, protected POSIX
state/locks/symlinks and strict result/eligibility DTOs. No live trial occurred.
