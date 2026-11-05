# Source provenance and licensing

## Station client

`station/` is based on [hu553in/yandex-station-chatgpt-voice](https://github.com/hu553in/yandex-station-chatgpt-voice), starting from upstream commit `0739f08`, with local Station adaptations and tests. Preserve upstream attribution. No license file or declared SPDX license was present in that source snapshot; this repository does not label that code MIT or grant rights on behalf of its upstream authors.

The upstream project cites [dyhhhhhh/chatgpt-web-voice](https://github.com/dyhhhhhh/chatgpt-web-voice) as a protocol reference. Root access is a separate prerequisite; see [hu553in/yandex-station-root](https://github.com/hu553in/yandex-station-root). Firmware images, vendor libraries, wake models and signing material are not bundled.

## Dreame bridge

The source and notices under `home-control/tools/dreame-bridge/` retain their own provenance and license in `NOTICE.md` and `LICENSE.upstream`. Those terms apply to the identified upstream-derived code rather than the entire monorepo.

## Dependencies and support tools

Go dependencies remain declared in each module's `go.mod` and `go.sum`. They are not vendored; their respective licenses continue to apply. Support tools identify external requirements in their own documentation. No blanket license is assigned to all files in this repository.
