# Read-only single-room DRAFT readiness

After the existing exact-r2422 current/saved-map proof succeeds, `rooms` may add
`cleaning_readiness` schema1. Active-map verification is not control permission:
map-level `verified_for_control` remains false. The separate protected control
protocol and readiness eligibility flag are described in SINGLE-ROOM-CONTROL.md.

Four fixed `get_properties` RPCs run inside the existing 24-second overall room
deadline, each with the existing six-second request cap:

1. The unchanged typed six-property status selector (state, error, battery,
   charging, status, task).
2. Exactly fan4.4/tag8, water4.5/tag9, mode4.23/tag27, tank4.6/tag10.
3. Customized4.26/tag30 alone.
4. Cleaning-paused4.17/tag21 alone.

No optional row can poison a required batch or erase map evidence. Missing/null
values remain `missing`; malformed values and generic property failures remain
`invalid`. Successful integers retain their observed value, including zero.
There is no boolean truthiness or guessed default. `unsupported` is used only
for the pinned exact model's known lack of drainage; no drainage RPC is added.
Read outcomes are only `ok`, `request_unavailable`, or `response_invalid`.
All errors after map refresh remain diagnostic data and cannot trigger an
authentication retry or replay of that refresh action.

The planner receives private authenticated catalog UID/device/model, one random
operation snapshot identifier, conservative local request-start timestamps,
the actual decoded current frame, and its proven selected-saved-map link.
Selected saved-frame rooms were already filtered to visible mapped pixels.
Their proven same saved-map linkage establishes current membership; current
outer pixels and names/categories are not substituted for segment identity.

The normal read path calls only the frozen `cleanplan.Build` and `Draft.Preview`
APIs. It never calls the logical-payload serializer or sends a cleaning action.
Each successful draft contains a selected segment ID and the fixed safe preview
(`DRAFT`, mode/fan/water, one pass/order1, battery, observed-settings source,
firmware acceptance `unverified`). No UID/device/map/snapshot IDs, raw maps,
objects, storage hosts, URLs, keys, or action payloads enter this DTO.

When customized cleaning is enabled, genuine per-room cleanset observations are
not yet available, so the result is `cleanplan_per_room_required` with no draft.
Other planner failures also leave the draft list empty while preserving safe
observed settings and their qualities. This is a review draft, not approval,
dispatch, or a firmware-acceptance claim. A protected server-held snapshot can
make readiness.verified_for_control true for the separate scope-gated writer;
firmware acceptance stays unverified. No new OAuth scope is introduced.

The provider adapter checks exact nested keys, fixed outcome/quality enums,
signed-32-bit observations, owned segment membership, preview ranges and
agreement with observed settings/resting state. Result size stays capped at
32KiB. Tests use only synthetic responses and existing local test servers.

Source baseline: Tasshack MIT commit
`f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585`,
[fixed property mappings](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/types.py#L1596-L1626)
and [room/settings construction](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/device.py#L4814-L4908).
