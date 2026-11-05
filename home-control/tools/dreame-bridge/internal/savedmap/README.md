# Bounded saved-map reader

This package has no network, filesystem, logging, or device-control operations.
`ParseList` reads the downloaded list JSON. A caller fetches each `rismobj` through
the interim object branch unchanged and then calls `DecodeFrame`. The decoded
frame ID, rather than record order, display name, or an invented record ID, is
compared with `List.CurrentID`. References, keys, names, and frame IDs remain
private application data and must not enter diagnostics.

`DecodeFrame` requires an explicit source-backed version hint (0, 1, 2, or 3), a
runtime IV for encrypted frames, and an optional separately supplied key. It
returns only actual pixel-derived, visible room segment IDs and their metadata.
Room `UniqueID` is separate metadata. `Saved` describes frame status;
`SavedMapID` describes a source-supported current-to-saved linkage. Neither
establishes which floor is currently active or authorizes any control action.
The caller owns selection, snapshot invalidation, aggregate fetch budgets, and
all active-map verification.

`Frame.Format` adds bounded format evidence for a future current-frame reader;
it does not enable or decide current binding. Its exported type is
`FormatMetadata`:

| Field | Source interpretation |
| --- | --- |
| `SavedMapStatus SavedMapStatus` | The validated `ris` value, or `SavedMapStatusUnspecified` (`-1`) when absent. Constants `SavedMapStatus0`, `SavedMapStatus1`, and `SavedMapStatus2` preserve the source's numbers without assigning additional meanings. |
| `SavedMapStatusPresent bool` | Distinguishes an absent `ris` default from an explicitly supplied value, including explicit `-1`. |
| `Temporary bool` | `suw` equals integer 5 or 6 and `fsm` is absent in an accepted frame; also becomes true when decoded nested `rism` is temporary, matching the upstream propagation. |
| `TemporaryFromNested bool` | At least one decoded nested `rism` supplied temporary-map evidence. This can be true alongside an independently temporary outer frame. |
| `New bool` | The outer frame explicitly supplies integer `risp == 0`. Nested new-map flags are not substituted for the outer value. |
| `Restored bool` | The outer frame declares `rpur == 1`, before upstream restoration/display adjustments. |
| `Empty bool` | The source's full-I `empty_map` pixel classification for the outer frame, before nested/display adjustments. It is independent of visible mapped-room count: a wall-only map can be nonempty with zero rooms. |
| `MarkersValid bool` | The newly interpreted `suw` / `risp` fields have supported shapes throughout the outer frame and decoded `rism` chain. Previously ignored unsupported fields preserve decode success but set this flag false; false `Temporary` / `New` values then do not establish absence. |

For `suw` and `risp`, absence and JSON null have the source predicate's known-false
semantics. Native integral JSON values fitting signed 64-bit are supported;
other types, floating-point notation, and larger values produce uncertainty
without new decode errors or coercion. This deliberately keeps prior saved-room
decoding behavior. Previously enforced `fsm` / `ris` / `rpur` validation remains
unchanged, including rejection of `fsm: null`. Upstream can suppress temporary
status with an explicitly present `fsm: 0`; presence matters independently of its
truthiness. `MarkersValid` propagation is conservative uncertainty tracking;
it is not an additional vendor map-status field.

The outer `Restored` and `Empty` evidence remains set when nested saved-map
content is available. The reader does not reproduce the upstream display
fallback that clears a restored flag or fills current-map geometry from saved
content. A future caller can reject temporary/new/restored/empty or uncertain
frames and require explicit `ris == 2`, but must independently establish
freshness, target identity, consistent snapshot selection, and saved-ID equality.
No such binding decision or control capability is implemented here.

`Frame.Selection` is an optional, raw observation for a caller that is separately
confirming a newly dispatched room task. Its type is `SelectionMetadata` with
`Present bool`, `Valid bool`, and `SegmentIDs []int`. It reads only the outer
frame's `sa` rows, taking each row's first column as in the pinned
[raw selection extraction](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/map.py#L4471-L4472).
It returns no row geometry, remaining columns, renderer/editor overlays, or
optimistically copied state.

| Raw outer `sa` | Optional observation |
| --- | --- |
| Missing | `Present=false`, `Valid=false`, no IDs. |
| Empty array | `Present=true`, `Valid=true`, zero IDs; this is no running-room evidence. |
| At most 63 nonempty array rows, each with a unique native integer ID from 1 through 63 in its first column | `Present=true`, `Valid=true`, observed IDs in their original order. Extra columns are ignored, with the existing whole-frame resource bounds still applying. |
| Unsupported type/shape, string/boolean/floating-point ID, out-of-range ID, repeated segment ID, or more than 63 rows | `Present=true`, `Valid=false`, no partial IDs. |
| Repeated root `sa`, including identical values or equivalent escaped key spelling | `Present=true`, `Valid=false`, no IDs. This does not create a new frame-decode error. |

`MaxSelectedSegments` exposes the limit of 63. Unsupported selection only
invalidates this optional observation; existing rooms, format flags, map linkage,
cryptography, and frame success are preserved. Existing malformed JSON, byte,
depth, token, and decompression limits continue to fail as before. Selection
validity is separate from `Format.MarkersValid`.

Selection is never inherited from a nested saved `rism` or an earlier decode.
The IDs are not filtered against `Frame.Rooms`: the current frame's source pixel
branch can expose no rooms while raw `sa` still declares a segment. A different
reported segment remains available for a caller to detect a mismatch. These IDs
are private application data and must not be logged.

The caller still needs fresh post-dispatch, target-bound task/state observations,
unchanged approved mode and saved-map binding, and exactly matching selected-room
evidence. A valid nonempty raw selection alone does not prove physical startup,
freshness, command acceptance, or which initiator caused activity. This reader
does not implement a confirmation decision or a physical action.

Flag provenance: pinned [declarations and saved-map predicate](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/map.py#L4375-L4414),
[full-I pixel emptiness](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/map.py#L4561-L4685),
and [restoration and nested temporary propagation](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/map.py#L4772-L4809).

The list limit is 1 MiB / 32 records. Each frame is bounded to 8 MiB encoded,
32 MiB inflated, 16 MiB pixels, and 4 MiB JSON tail. Nested `rism` depth is at most
two; one decode operation also shares 16 MiB compressed / 48 MiB inflated limits
across nested frames. JSON has depth and token limits and rejects duplicate keys,
with the scoped frame policy below. Map-list JSON remains strict everywhere.
Errors contain fixed codes and optional fixed duplicate categories only.

The frame-tail parser permits duplicate fields only in branches this metadata
reader entirely ignores, matching the pinned Python reader's last-wins behavior
for those unused values. Duplicate consumed fields remain rejected even when
their values are identical. The protection inventory is path-aware:

| Frame location | Duplicate handling |
| --- | --- |
| Root `fsm`, `rpur`, `us`, `iscleanlog`, `ris`, `suw`, `risp` | Reject; metadata/proof fields and their descendants are strict. |
| Root `delsr` and `seg_inf` | Reject; hidden IDs and segment records affect returned rooms. |
| Root `cleanareaorder` | Reject; this field can change the decoded version. Its subtree is strict. |
| Root `rism` and `saveMapId` | Reject; these fields affect saved-map linkage. Their subtrees are strict. |
| Root `sa` | Repeats invalidate optional `Selection` only; the existing frame read still succeeds. |
| Segment keys directly inside `seg_inf` | Reject repeated keys; distinct spellings with the same numeric segment ID also remain rejected. |
| Room `name`, `type`, `index`, `roomID` | Reject repeated fields and duplicates within their relevant descendants. |
| Other root or room fields and their ignored branches | Permit duplicates; these values do not affect this reader's output. Familiar field names inside an ignored object remain ignored. |

Every occurrence, including ignored duplicates later replaced by `json.Unmarshal`,
still traverses UTF-8, JSON syntax, depth, and token validation. Existing byte,
inflation, and nesting limits remain in force. Each nested decoded `rism` uses
the same frame policy; `ParseList` keeps strict duplicate checks at every path.
Adding a new consumed frame field requires adding its duplicate protection to
the frame parser's field inventory.
Optional raw selection uses the separate invalidation policy above; it does not
weaken duplicate rejection for the existing consumed fields.

`Error.Code` and `Error.Error()` remain unchanged. Duplicate failures additionally
carry `Error.DuplicateCategory` (type `DuplicateCategory`), selected only from
`list`, `frame_metadata`, `frame_rooms`, `frame_version`, `frame_binding`, or
`segment_alias`. These categories contain no input key, path, value, identifier,
or map data. Other failures leave the optional category empty.

The implementation follows the pinned source's full `I` frame pixel branches,
signed 16-bit little-endian header fields, SHA-256 hex-text AES key derivation,
CBC followed directly by zlib, standard base64 UTF-8 room names, hidden segments,
and read-only `rism` / explicit-version-3 `saveMapId` binding. It retains the source
behavior in which URL alphabet replacement occurs before suffix-key extraction;
a separately supplied key is not transformed. Nested `rism` decoding inherits the
version but receives neither the outer IV nor its key. It never replaces outer
room metadata with a nested frame's rooms.

Source `saveMapId` / `cover` / `diff` / `curtain` markers do not promote the returned
partial version: the upstream local assignment occurs after `partial.version`
was set. Explicit `cleanareaorder` promotes version 1 to 2. Incremental, Wi-Fi, and
unknown frame types or version hints are unsupported. Parsing fails closed on
unknown interpreted metadata shapes, invalid dimensions, bad keys, invalid names,
and bounds violations. Unlike Python's permissive `decodebytes`, this reader
accepts only base64 alphabet characters and whitespace; it does not ignore
arbitrary punctuation. In particular a comma-bearing frame with a separately
supplied key is rejected rather than stripping another suffix or trying keys.

All tests use synthetic maps and cryptographic values. The independent CBC
fixture was generated with Python's standard-library zlib and the existing
OpenSSL 3.0.13, using a documented synthetic key and IV. No runtime map, real home
metadata, or vendor cryptographic constant is included.

Format logic and names adapted from [Tasshack/dreame-vacuum map.py](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/map.py#L4202-L4789)
and [types.py](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/types.py#L3772-L3876),
commit `f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585`.
Copyright (c) 2022 Tasshack; MIT license. The complete permission and warranty
notice is retained in [LICENSE.upstream](../../LICENSE.upstream).
