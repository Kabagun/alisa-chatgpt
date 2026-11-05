# Home Control

A self-hosted MCP service with separate provider connections for each user. The
first provider is Dreamehome. The speaker and the user's computer are not part of
the device-control service's runtime.

## Implemented behavior

- OAuth authorization-code flow with PKCE, explicit scopes, rotating refresh
  tokens and isolated private account directories.
- Dreamehome credential entry through the HTTPS authorization form. Passwords
  are passed only to the provisioner through stdin and are not saved.
- Authenticated device discovery, device status and available inline map-room
  metadata. Every device operation verifies membership in a fresh account
  catalog.
- Provider-managed connection setup without a cloud-region field in the login
  form. Dreame currently makes one initial EU login attempt, then requires a
  successful authenticated nonempty device catalog on that same origin before
  saving the session. Token locale metadata is kept separate from this working-
  origin proof. This is not a universal account-country discovery protocol.
- Same-account token refresh on the verified origin. Failed setup does not cycle
  credentials through other regions. Explicit regional setup remains an internal
  compatibility option. Zero-device accounts cannot establish automatic setup.

Room cleaning is **not implemented or enabled yet**. `home_clean_rooms` returns
`room_control_unverified`. Current room metadata does not establish a verified
active floor, saved-map binding or safe model-specific cleaning request. The
service never substitutes a whole-home clean. Real account/model/map inspection
is required before completing that capability.

## Managed deployment

The `appsctl` declaration is `app.yaml`. The platform assigns `HOST`, `PORT` and
`APP_DATA_DIR`; `run` launches the bundled static Go server. Only the assigned
loopback host is accepted. The public OAuth origin comes from a generated
`public-config.json`; use `public-config.example.json` as a portable example.

A prepared deployment bundle contains:

```text
app.yaml
run
public-config.json
vendor-profile.json
bin/home-control
bin/dreame-reader
bin/dreame-provision
```

The real pinned upstream application profile is a build/deployment input, kept
outside source control. The durable helper source and invalid example profile
are under `tools/dreame-bridge`. Provider tokens, account state and OAuth state
belong only under the service's private `APP_DATA_DIR`; source updates must not
copy them.

Private state uses current-UID ownership and exact 0700/0600 permissions. Run one
server process per data directory. The platform's SSH deployment identity is
not an application user identity. Incoming identity headers do not authenticate
MCP callers.

## Verification

The project uses Go's standard library and no third-party Go modules. Test on a
real Linux filesystem because private-file tests need POSIX ownership/modes.
Go 1.27's `testing.T.TempDir` uses `GOTMPDIR`, so changing only `TMPDIR` does not
move those fixtures from a Windows-mounted filesystem.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false \
  -ldflags='-s -w -buildid=' -o bin/home-control ./cmd/home-control
```

`sh build.sh arm64` builds all three executables under `bin/`, including the
reader name expected by the service. See [BUILDING.md](BUILDING.md) for the full
portable workflow and deployment inputs. Build the two helper executables for
the same architecture. The deployment bundle can target
ARM64 or AMD64 Linux; select the architecture used by your server.
No vendor account request is part of the unit tests, and production has no fake
device mode.
