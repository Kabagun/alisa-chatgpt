# Build and deployment inputs

Both Go modules require Go 1.27.3. They use only the standard library and need
no downloaded Go modules. Install the Go toolchain separately, then run from
this directory on Linux:

```sh
go version
mkdir -p temp
# Use a real Linux filesystem for test fixtures (not a Windows/DrvFs mount).
# TMPDIR and GOTMPDIR must both point to an existing private local directory.
export TMPDIR="$(mktemp -d)"
export GOTMPDIR="$TMPDIR"
go test ./internal/providers/dreame ./internal/oauth ./internal/store
(cd tools/dreame-bridge && go test ./internal/cloud ./internal/savedmap ./internal/cleanplan)
go test -race ./...
go vet ./...
(cd tools/dreame-bridge && go test -race ./... && go vet ./...)
sh build.sh arm64
# Use: sh build.sh amd64 for an AMD64 Linux server.
```

The top-level script builds `bin/home-control`, `bin/dreame-reader` and
`bin/dreame-provision`. It disables CGO and VCS build metadata and removes
filesystem paths/build IDs from the binaries. For repeatable binary hashes,
use the same Go release, source tree, target architecture and build environment.
The helper-only `tools/dreame-bridge/build.sh` remains available; its reader
output is named `dreame-bridge`, so the full service bundle should use the
top-level script. Generated binaries and build manifests are not source files.

For a deployment, create `public-config.json` from `public-config.example.json`
with your own HTTPS origin. Provision `vendor-profile.json` separately using the
operator-supplied application profile; the deliberately invalid public profile
example documents its schema. Neither file belongs in Git. Provide `HOST`
(`127.0.0.1`), `PORT` and an absolute private `APP_DATA_DIR` through your runtime.
The `run` launcher expects the three built executables in `bin/` and starts the
service from this directory. Keep state outside the source checkout and set
executable permissions on the launcher and binaries after copying to Linux.

The source export excludes local sessions, OAuth/provider account state,
passwords, tokens, signing profiles, map payloads, logs, temporary captures,
proprietary packages and generated binaries. Real device access requires your
own deployment inputs and a separate explicit connection flow. Unit tests use
synthetic fixtures and local HTTP/TLS servers; they do not contact an account.

The nested bridge retains `NOTICE.md`, `LICENSE.upstream` and the source-level
MIT attribution for adapted map parsing. No proprietary firmware or vendor
application package is distributed here. No project-wide license is inferred
from the upstream MIT notice; it applies to the attributed upstream material.
