# Read-only map diagnostics, schema 2

The existing `rooms` operation now includes `diagnostics` when reading the
authenticated device's map metadata. This does not add a public RPC, selectable
property, URL, map update or robot action. It is model-independent and does not
hardcode an owner's device code or household data.

The fixed reads are the existing cloud history `6.1` (up to 20 records), then
one `get_properties` RPC selecting only OBJECT_NAME `6.3` and MAP_LIST `6.8`.
Both use the session's fixed origin and the owned catalog's validated RPC path.
When the trusted6.8 descriptor contains an unambiguous object_name/obj_name,
one additional signed POST to `/dreame-user-iot/iotfile/getDownloadUrl` requests
URL metadata using only did, actual catalog model, intact filename and session
server region. No caller supplies these fields. The MD5 is not sent. A comma
in this filename stays intact; this is not the getOss1dDownloadUrl operation.
Each request retains existing TLS, timeout, redirect and response-size checks.
Authentication errors remain errors so the adapter's one-refresh behavior is
preserved. The signed-URL response is capped at64KiB and the URL at8192 bytes.
No DNS lookup, connection, redirect, GET, download or decryption is performed
against the returned URL.

Pinned evidence at `f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585`:

- [Property mappings](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/types.py#L1673).
- [Map-list object_name/obj_name and md5 handling](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/device.py#L936).
- [Saved-map object retrieval](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/map.py#L1644).
- [Comma key suffix and frame-header decoding](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/map.py#L4202).
- [Native getDownloadUrl request and data URL string](https://github.com/Tasshack/dreame-vacuum/blob/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585/custom_components/dreame_vacuum/dreame/protocol.py#L1359).

Top-level map output stays `source=cloud_history_6.1_metadata_only`,
`active_map_verified=false`. Each decoded map remains metadata-only with
`active_map=false` and `verified_for_control=false`. If no inline map can be
decoded, `maps=[]` is returned with diagnostics. This means metadata is
unavailable through this reader; it does not prove the home has no saved rooms.

Diagnostics contain only:

- `schema=2` and a fixed `reason` enum. The adapter also accepts the exact old
  schema1 shape; it does not accept arbitrary additions to either version.
- `history.outcome`, a record count, six `value_shapes` counters, and counts of
  candidates, encrypted-suffix hints, full/incremental/other/invalid frames,
  decode failures and decoded maps. Every count is 0..20. Counts are checked
  against each other and against returned maps by the adapter.
- `properties.outcome` and two property summaries (`object_name`, `map_list`).
  Each summary has fixed `state` and `value_shape` enums plus
  `object_reference_present` and `md5_present` booleans.
- `saved_object` has fixed outcome/scheme/host/port classes and
  `fetch_performed=false`. An optional normalized hostname is private
  authenticated tool data, present only for HTTPS, syntactically safe DNS,
  default or explicit443, and no userinfo/fragment. It is capped at253 bytes.
  Treat the hostname as private runtime data; do not log or publish the full
  saved_object result. Formatting its Go type redacts the metadata.

No raw property value, object name, filename, signed URL/query, map key/IV,
frame, map/room ID, room name, pixel data or vendor error text is present in
the diagnostics. The only origin detail allowed is the private hostname above;
it is not a signed URL and does not include its path/query/signature. Existing
successfully decoded room metadata remains private
account result data, with the same unverified flags as before.

The reason enum is prioritized as follows:

| Reason | Interpretation |
| --- | --- |
| inline_metadata_available | At least one inline metadata map decoded; active map is still unverified. |
| encrypted_inline_unsupported | At least one comma-suffix encryption hint was observed; no key was used. |
| saved_object_required | An object-reference field is present; fetching is still unsupported. |
| incremental_only | Incremental frames were observed without a full frame. |
| history_unavailable | The history read or response shape could not be used. |
| no_map_history | This bounded history query returned zero records, without an observed object reference. |
| history_shape_unsupported | Records exist but none has the expected JSON-array-string first frame. |
| inline_decode_unsupported | Candidate frames exist but no supported complete room metadata decoded. |

Outcome enums distinguish successful reads, unsupported/missing properties,
unavailable history owner metadata, unavailable routing/request, invalid
responses and response limits. They do not disclose raw upstream error codes.
Unknown/missing diagnostic fields, unknown enums, oversized or inconsistent
counts, and control-verification promotion are rejected by the adapter.

Saved-object outcome enums are not_referenced, reference_invalid,
request_unavailable, response_invalid, response_limit, url_rejected and
metadata_available. Scheme/host/port enums contain no raw input. IP literals,
reserved/invalid DNS forms, nonHTTPS URLs, unexpected ports, userinfo and any
fragment are rejected without returning a hostname. DNS syntax validation is
not evidence of public resolution or vendor ownership. No storage-host
allowlist is inferred; root must privately approve an exact host and a future
fetch boundary before any blob GET is implemented.

The next authorized live read needs only this diagnostic object. Further map
work depends on the observed format and a reviewed storage-host boundary;
diagnostics alone do not establish an active floor or a room-cleaning binding.
