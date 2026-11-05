The constructor accepts trusted absolute ReaderPath, ProvisionerPath and
ProfilePath plus an optional per-child Timeout (default and maximum 30s).
Executable/profile files must be regular, without group/other write bits.
This adapter supports Linux only; other operating systems fail closed.

Authenticate accepts empty Region (or auto) for one EU login followed by a
successful authenticated nonempty catalog at the same origin, before saving
the session. It proves a working connection, not a registered account country.
There is no password cycling or second candidate. Locale/domain fields do not
route the account. A zero-device automatic account returns
account_connection_unverified. Explicit by/eu/ru/us/cn/sg/kr remains supported
for internal/admin backward compatibility, without automatic catalog proof.
Login JSON is bounded stdin, with no inherited application environment. The
profile stays in memory. The existing provisioner atomically writes a current
UID-owned 0600 session within the backend's current UID-owned 0700 account
directory. Passwords are not saved; Go cannot guarantee zeroing string copies
or runtime heap memory. No credentials or vendor errors are logged.

Automatic sessions store server_region=eu and routing_proof with the fixed
authenticated_nonempty_catalog method and UTC RFC3339 verified_at. Loaded
proofs are validated; legacy explicit sessions may omit proof. Provision output
adds only resolved_server_region and routing_verified alongside existing safe
booleans. The adapter requires both eu and verified=true for automatic login;
old helper responses cannot accidentally satisfy it. The proof survives token
refresh as historical evidence; each read still checks a fresh device catalog.

Each Execute fetches a fresh device catalog and proves device membership
before status/rooms/clean_rooms. Explicit expiry permits exactly one same-account
refresh followed by a fresh catalog. There is no password retry. Output has
fixed allowlisted fields and is capped at 32KiB; stderr is discarded and capped
at 64KiB. Execution has a 90s overall deadline and kills the child process group
on timeout/cancellation. All returned errors use shared fixed provider codes.

clean_rooms supports exactly one canonical segment and signed16 map ID through
the separate protected --control helper mode. It requires a recent server-held
preview and fresh ownership/map/state/settings/planner validation. No automatic
control-helper retry/refresh occurs after invocation; ambiguous outcomes return
structured unknown data. Map metadata alone is insufficient, and no whole-home
fallback exists. The backend's home:control scope and concrete first-test owner
approval remain mandatory. See SINGLE-ROOM-CONTROL.md for the full contract.

rooms may return maps=[] with a strict diagnostic object (schema1 legacy or
schema2 with required saved_object). Counts0..20/enums/booleans cover history6.1
and fixed read properties6.3/6.8. Schema2 may make one fixed signed getDownloadUrl
request from an unambiguous authenticated6.8 reference. Its only free-text
result is a private normalized HTTPS DNS443 hostname, capped at253 bytes;
never publish/log this private runtime field. No URL path/query/signature,
raw map/reference/key/name/ID is returned. Unknown/missing fields, unsafe
locator metadata and inconsistent counts are rejected. Empty diagnostics
do not prove saved rooms are absent; active-map/control verification stays
false. No returned-URL DNS/connect/GET, object download or decryption is
implemented in this phase. A hostname is not proof of public resolution or
vendor ownership and cannot enable a fetch automatically. With explicitly
approved exact map_storage_hosts and source-derived map_model_ivs plus
map_model_versions, schema3 enables bounded saved-map list/frame GETs. The
trusted reader receives a generated account-local current-UID0600 copy of the
alreadyvalidated deployment profile on rooms, removed on success/error/timeout.
The immutable deployment profile is never passed directly to the helper;
private session/profile ownership checks stay unchanged. No session migration
or caller path is allowed. All returned room maps
remain active/control-unverified, selected by decoded curr_id match only.
See tools/dreame-bridge/SAVED-MAP-READS.md for transport and budget details.

Schema4 may prove active-map metadata through the one fixed full-I refresh and
unchanged descriptor/linkage checks; control remains unverified. After that proof,
optional cleaning_readiness schema1 returns bounded observed state/settings and
safe single-room DRAFT previews only. Exact nested keys, quality/outcome enums,
numeric bounds, owned segments and preview agreement are validated. Unsupported
is permitted only for source-known absent drainage; generic errors remain invalid
or missing. There is no logical action payload, cleaning dispatch, new scope,
or per-room-settings fallback in the read path. A successfully saved private
preview makes readiness.verified_for_control true for the separate writer;
map-level flags remain false and firmware acceptance remains unverified.
See tools/dreame-bridge/CLEANING-READINESS.md and SINGLE-ROOM-CONTROL.md.

Optional schema4 current_map.selection is a strict three-key object with raw
outer-frame present/valid booleans and count0..63. Nulls, unknown keys, IDs,
non-full-frame observations and inconsistent flags/counts are rejected. It is
diagnostic data only and makes no inference about which room is running.
