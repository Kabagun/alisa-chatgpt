# Provenance and export policy

This is a source-only, packaging-time extraction from the project's `temp/native-runtime/` experiments and the two project-owned scoped provisioning Python files. Each copied file is listed in `EXPORT_MANIFEST.json`. No Git history or private working-directory instructions are included. This file makes no blanket license grant.

The KWS C wrapper describes the device's native ABI and dynamically loads vendor libraries. It contains no vendor library, firmware, trained model, certificate, or key. Historical copies are retained in both probe directories to avoid changing their experimental interfaces. Consult the repository's top-level third-party notices for the Station client's upstream origin and licensing status; these files do not relicense upstream code.

The scoped provisioning adapter imports a separately supplied `scripts/equasar_server.py` from [krevetkqqq/lite-kit](https://github.com/krevetkqqq/lite-kit), examined locally at commit `b9f54696d394b2b5ff8ba32bd5d3cc4257b64891`. That project contains an MIT license. Its code and dependencies are not vendored by this support export; obtain it separately and preserve its license when using or redistributing it. The adapter's synthetic tests do not include upstream source.

The untrained Kolya YAML example references public Piper voices and an external microwakeword trainer. These models, datasets, trainer sources, and their licenses must be obtained and reviewed separately. The bootstrap references public Go, uv, and Zig distribution metadata; their tools and licenses are not bundled.

Excluded categories:

- Credentials, browser sessions/cookies/tokens, DPAPI packs, SSH keys/known-host files, actual runtime configuration, home/account/device/map/room state, logs, request/response records, live acceptance evidence, private URLs/IDs, and private source instructions.
- Downloads, caches, vendor libraries, firmware, model/audio files, compiled binaries, install/update payloads, rollback snapshots, and scripts bound to private stage identities or artifact hashes.
- Removed or failed microphone gain and speaker volume changes, their fixtures, and their deployment/rollback workflows.
- Obsolete stage/deploy/binding scripts, one-off network/router/firewall changes, duplicate historical client snapshots, and reports based on private observations.
- Browser-auth research with no independent executable source; tool-probe context/model-default/synthetic-latency deployment experiments; feedback's copied indicator/effects upstream code and result binders; account relinking; Dreame/home-control state and helpers covered by the separate modern home-control export.

Public vendor service domains and fixed device OS paths remain where they define the code's protocol/ABI. Test identities, reserved documentation addresses, synthetic secrets, and malformed input test vectors remain deliberately synthetic. The hosting project ID is unset, the former installation-specific activation phrase is removed, and scoped tests take an external kit path from the environment.
