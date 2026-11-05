# Fixed current-map read/refresh

Only the exact authenticated `dreame.vacuum.r2422` target can enter this phase,
after a selected saved map has decoded with a trusted model/host profile and
descriptor snapshot. The internal request is fixed: method action, service6,
action1, input property2 with compact text
`{"req_type":1,"frame_type":"I","force_type":1}`. It requests a map upload;
there is no cleaning, movement, map switching or generic action interface.

Each rooms operation sends at most one action. Typed modern outputs1/3 supply
inline/current-reference candidates; legacy13 remains explicitly unsupported.
The source-preferred reference branch splits its comma key only here. Saved
rismobj references retain their separate unchanged contract. Dual outputs are
never combined, and contradictory decoded identities/format are rejected.

An action-coupled candidate is fresh evidence. Otherwise at most3 fixed6.3-only
RPC polls, spaced at least2s apart, can find a changed successful typed baseline
reference. Unknown or unchanged baseline data is insufficient. No history,
host-time, docked, outer-header-ID or first-floor fallback is used. Per-request,
24s operation and shared4GET/7MiB limits remain in force.

Active metadata verification requires a decoded full-I current frame with
explicit savedstatus2, valid markers, no temporary/nested-temporary/new/restored
or empty flags, and its private SavedMapID equal to the selected saved map.
The fixed6.3+6.8 descriptor read must then match the original descriptor snapshot.
This is device-declared active-map evidence, not independent physical location.

All post-action failures return unverified diagnostic data rather than a
retryable session fault, preventing automatic token-refresh replay of an
ambiguous action. No automatic action retry exists. Schema4 current_map contains
only fixed outcomes/freshness classes, counts and flags. Hostnames are removed
from schema4 saved_object output; raw IDs/refs/keys/frames/geometry never enter
the new diagnostics. Owned room result data retains its existing private scope.

Duplicate frame failures may additionally carry an optional duplicate_category:
list, frame_metadata, frame_rooms, frame_version, frame_binding or segment_alias.
These are fixed typed decoder categories, never actual JSON names or paths.
Only ignored frame branches may use last-wins parsing; consumed proof/room/
version/binding fields and the saved-map list remain duplicate-strict.

A successfully decoded current frame may also carry optional `selection` with
only `present`, `valid`, and `count` (0..63). These are copied from the decoder's
raw outer `sa` observation; no IDs, names or requested-room match are exposed.
Absent/invalid selection has count0; present+valid+count0 is a genuine empty
selection. This diagnostic neither promotes map/room proof nor changes any
action, polling, control, retry or blob budget.

Even complete active evidence keeps map-level verified_for_control=false. The
optional subsequent readiness preview is described in CLEANING-READINESS.md;
the separate protected writer is described in SINGLE-ROOM-CONTROL.md. Format logic remains in the frozen savedmap module,
adapted from Tasshack commitf5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585 under MIT.
