# Archived plugin connection experiment

This is a source-only archival snapshot, separate from the main `home-control`
component. It preserves an unpublished cleanup/connection experiment. It is not
selected for deployment, and packaging it does not enable any live connection.

## Provenance and differences

The snapshot was copied from the local relative source directory
`smart-home-plugin/temp/plugin-deletion/cleanup-source`. Only `go.mod` and Go
source/tests from `cmd/` and `internal/` were allowlisted. No proof records,
configuration with live bindings, cookies, session/account state, binaries,
logs, screenshots or cached third-party code are included.

Compared with the main component's original source, 31 files are identical,
five are additional and three differ:

- Added `internal/app/cleanup_authorization.go` and four tests covering cleanup
  authorization, form proof, automatic activation and discovery descriptions.
- Changed `cmd/home-control/main.go` to accept optional deployment-only cleanup
  authorization settings.
- Changed `internal/app/app.go` to scope temporary cleanup identities to one
  exact registered client, redirect and expiry, with an optional form-proof
  fallback or automatic activation.
- Changed `internal/app/mcp.go` to mark cleanup discovery descriptions and
  reject device operations for cleanup identities.

The main root source does not contain these additional changes. Source
comparison establishes this difference; it does not independently establish
which version was deployed or the exact chronology of live experiments.

The experiment creates an inert cleanup identity without provider access. It
retains PKCE/flow binding and limits the optional configuration's expiry. The
code and tests are preserved for review, not as a production recommendation.
No real cleanup configuration or live client identifier is supplied here.

## Verification and build

This directory is a standalone standard-library Go module using Go 1.27.3.
On Linux, use a real POSIX filesystem for test fixtures and an existing private
local directory for both `TMPDIR` and `GOTMPDIR`:

```sh
go test -run Cleanup ./internal/app
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false \
  -ldflags='-s -w -buildid=' -o bin/home-control ./cmd/home-control
```

Tests use synthetic inputs and local mocks. The core compiles independently;
running the service additionally requires the main component's Dreame helper
binaries, an operator-supplied application profile and ordinary private runtime
configuration. Those deployment inputs are deliberately absent. The shared
helper source, notices and upstream MIT license remain in `home-control` and
are not duplicated in this archive. No project-wide license is inferred.
