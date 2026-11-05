# Checks and requirements

Use existing tools; this export installs nothing. Run from the repository root unless stated otherwise.

```sh
python3 -m unittest discover -s support/runtime/tests -p 'test_*.py' -v
pwsh -NoProfile -File support/auth/probe-web-session.ps1 -SelfCheck
node --test support/diagnostics/cloud-probe/tests/*.test.mjs
```

The auth self-check requires Windows PowerShell 7 with Windows DPAPI. It uses a synthetic HTTP handler, creates encrypted synthetic packs beside its script, and removes them afterward. The supervisor tests require a POSIX `sh`; they mock only ownership metadata and never operate on real system services.

For the Go probes, use the Go version declared in each `go.mod`. The HTTPS preflight uses only the Go standard library. KWS tests require Linux, CGO, a C compiler, and `libdl`; the IPC probe also requires the pinned modules from `go.mod`/`go.sum`. These tests use synthetic fixtures and mocked detector/IPC dependencies, with no real microphone or cloud requests.

```sh
cd support/diagnostics/column-agent
go test -mod=readonly ./...
go vet -mod=readonly ./...
cd ../kws-offline-probe
CGO_ENABLED=1 go test -mod=readonly ./...
CGO_ENABLED=1 go vet -mod=readonly ./...
cd ../kws-probe
CGO_ENABLED=1 go test -mod=readonly ./...
CGO_ENABLED=1 go vet -mod=readonly ./...
```

Run KWS permission checks on a native Linux filesystem that models ownership, links, and modes. On Windows-mounted filesystems use a project-local private mount namespace/tmpfs scratch directory, or run a Linux CI checkout. Do not weaken permission tests to accommodate filesystem emulation.

Native probe execution needs separately obtained compatible vendor libraries/models on an explicitly authorized device. Those proprietary dependencies are not distributed. `build.sh` scripts compile source only and accept an already configured `CC`/Go toolchain; they never connect to a device. `CC` may point to a compatible ARM cross-compiler. The ABI probe accepts `STATION_NATIVE_DIR` pointing to the repository's Station native wrapper directory.

Provisioning tests have a separate external dependency, described in `provisioning/README.md`. Source syntax checks alone do not establish hardware or upstream service compatibility.
