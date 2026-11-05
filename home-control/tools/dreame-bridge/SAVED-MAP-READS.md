# Saved-map reads: bounded, private, no control

Adapter.New reads and validates the fixed deployment Config.ProfilePath once.
That immutable deployment file may be platform-owned and runtime-readable by
ACL; it is not a private session file. For each rooms read, the adapter creates
an unpredictable0600 copy of the cached validated profile inside the account's
current-UID0700 directory. Only that generated path is supplied to
`--map-profile`. The CLI keeps its current-UID0600/O_NOFOLLOW checks unchanged.
The temporary copy is removed after success, failure or timeout. No caller,
environment variable or account session selects its path. No password/token
is copied; existing sessions are neither migrated nor rewritten.

The deployment profile recognizes these optional extensions:

- `map_storage_hosts`: up to4 exact normalized DNS names, without wildcards,
  URLs, ports, IP literals or private/reserved name forms.
- `map_model_ivs`: up to16 exact model keys, each with a UTF8 IV of16 bytes.
- `map_model_versions`: up to16 exact model keys with a source-derived default
  version0..3. A missing model version or IV refuses map decoding.

Actual hosts/IVs remain deployment-only private data. The source example has
empty extensions. AppProfile accepts the extensions for compatibility, but
authentication does not send them to the vendor or retain them in issued
sessions. Profile formatting is redacted; no map profile is logged.

The core property request remains exactly the proven6.3+6.8 pair. With map
extensions enabled,13.1 is queried separately in a fixed one-selector RPC,
cached only for that rooms operation. An unsupported or malformed optional
probe records its own quality and cannot erase the core map-list reference.
Successful nonnull13.1 (including0/false) overrides the version to0 according to
the pinned capability rule. Missing, unsupported, null or failed probes retain
the configured model default with explicit provenance; they do not prove
hardware absence.6.7 does not change version and is not required here. No
REQUEST_MAP/upload or cleaning action is sent.

After the existing signed getDownloadUrl lookup for the authenticated6.8
reference, the returned HTTPS DNS443 host must exactly match the approved
profile before any DNS lookup or GET. The separate blob client:

- Uses ordinary verified TLS1.2+ with SNI for that host, no proxy, redirects,
  cookies, account/app/tenant/auth headers or automatic compression.
- Checks every DNS answer. It rejects non-global/private/link-local/loopback,
  documentation, translation and other special-use blocks conservatively.
  It then dials a selected validated numeric IP directly, avoiding a second
  DNS lookup while preserving hostname verification. Each GET resolves anew.
- Uses6s request limits, bounded headers/body, no connection reuse or cache.
  Host/address/transport failures expose fixed codes only, without raw errors.

The address exclusions follow the
[IANA IPv4](https://www.iana.org/assignments/iana-ipv4-special-registry) and
[IPv6](https://www.iana.org/assignments/iana-ipv6-special-registry) special-use
registries. Some globally reachable protocol-specific exceptions are rejected
intentionally; storage objects should use ordinary public unicast addresses.

The whole helper rooms operation is bounded to24s. At most4 blob GET attempts
are permitted: one list (1MiB) and up to3 referenced frames (2MiB each), with
at most7MiB accepted payload in total. Signed locator reads retain the fixed
vendor origin and ordinary signing. Object references remain opaque, including
commas, and cannot choose a new vendor region or unapproved storage host.

The list parser caps32 entries. Inline `map` takes precedence; otherwise only
the source `server=1`/`rismobj` branch is fetched. Mapobj/name/order are not
fallback selectors. The pure savedmap decoder handles bounded full-I frames,
source AES/zlib handling, pixel-based visible segments and room enrichment.
Its independent limits are8MiB encoded,32MiB inflated,16MiB pixels,4MiB tail
and nesting depth2, with aggregate inflate/compressed bounds. The caller's
frame input limit is stricter. No full floorplan or raw blob is returned.

The caller returns only a decoded saved frame whose canonical MapID matches
the list's curr_id. It stops at that match, does not retain unrelated floors,
and never falls back to the first map. The result uses
`source=cloud_saved_map_metadata_only`, with optional private
`selected_saved_map_id` and one maps entry. Active-map and control verification
remain false. Curr_id and saved-frame status do not prove the robot's current
floor; descriptor MD5 is not claimed as an integrity check.

Diagnostics schema3 adds exact saved_maps outcome, version/probe provenance,
bounded list/frame counts, blob attempts/downloads/accepted-byte counts,
selected-match boolean and a fixed last-error enum. Accepted-byte counts cover
completed bounded payloads, not partial rejected wire transfers. The provider
rejects inconsistent counts, mismatched selection IDs, unknown fields/errors
and any control-verification promotion. Schema1/2 remain supported for legacy
diagnostic results. Names/IDs are private authenticated room result data;
URLs, signatures, keys, object references and pixels never enter diagnostics.

All phase3 tests use synthetic local TLS servers/fixtures. No owner credentials,
session, private profile, map, DNS host or live account call was used by this
implementation worker. No additional dependencies were installed.
