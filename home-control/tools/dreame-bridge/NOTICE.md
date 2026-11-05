This directory contains the stdlib-only Go implementation of the pinned
Dreamehome protocol, adapted from public upstream protocol evidence:
https://github.com/Tasshack/dreame-vacuum/tree/f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585
The upstream MIT notice is retained in LICENSE.upstream. The Go implementation
is original project code; it does not vendor or execute the Python component.

Build with Go 1.27.3: `sh build.sh arm64` (VPS) or `sh build.sh amd64`.
No external Go dependencies, CGO, listener, arbitrary RPC or shell execution.
The nested module keeps the CLI binaries independent of the web service.
Run `go test ./...` here on a POSIX filesystem; session tests require real
0700/0600 permissions and current UID ownership. Windows/DrvFs permissions
cannot establish those guarantees.

The profile example deliberately fails validation. Supply a deployment-only
profile file through operator configuration; never put actual profile values,
passwords, issued tokens, sessions or account data in this source directory.

Reader operations are devices/status/rooms only. Rooms currently decode inline
complete, unencrypted history metadata; active-map and control verification
remain false. Signed map objects, model-specific encryption and saved/current
map reconciliation remain unsupported. Room control must remain unavailable
until actual authenticated model/map capabilities can be verified. Backend
region can be omitted (or set to auto) for one EU login attempt followed by an
authenticated, parsed, nonempty device catalog at the same origin. Only then
is the session saved with a timestamped working-origin proof. This is a bounded
connection strategy, not discovery of the registered country; locale/domain
fields do not choose a route. A zero-device automatic account is refused.
Explicit by/eu/ru/us/cn/sg/kr modes remain for internal/admin compatibility.
Neither mode retries credentials at another origin. Regional cloud routing is
pinned evidence, not a promise of current service availability.

The rooms reader also returns safe schema2 diagnostics for history6.1 and the
fixed OBJECT_NAME6.3/MAP_LIST6.8 property reads. See MAP-DIAGNOSTICS.md. Missing
inline metadata does not establish that no saved rooms exist. No object URL,
map payload, key, name or ID is included in diagnostics, and there is no map
download/decryption or robot action. One fixed signed getDownloadUrl read can
return private origin metadata (HTTPS DNS443 hostname only). No returned URL
DNS lookup, connection or GET is made; no path/query/signature is returned.

Optional protected map extensions now enable exact-host bounded saved-map
reads (diagnostics schema3). No host is inferred or granted automatically.
The source-only pure savedmap package retains its own MIT attribution. See
SAVED-MAP-READS.md; active/control verification stays false and there are no
robot actions or cleaning writes.
