# Project rules

- Treat this repository as public regardless of its GitHub visibility.
- Fetch existing remotes before starting a task. Create worktrees only when requested.
- Never commit credentials, runtime state, device/account identifiers, maps, personal diagnostics or machine-specific configuration. Use synthetic fixtures and portable templates.
- Keep scratch work in ignored `temp/` directories. Do not add local working snapshots to Git.
- Preserve source attribution and third-party notices. Do not apply a blanket license to upstream code.
- Use Conventional Commits and never force-push without an explicit request.
- Device, cloud-account and physical-appliance tests are separate from offline checks. Do not start a microphone or issue a cleaning command as a test without authorization.
- Run focused tests for changes, then relevant module checks. Permission-sensitive Linux tests require a POSIX filesystem.
- `station/` and `home-control/` are independent Go modules; `home-control/tools/dreame-bridge/` is a nested module. Keep experimental support tools separate from production code.
- The live installation and its private configuration are not part of this checkout. A repository update is not authorization to deploy.
