# Scoped provisioning adapter

`scoped-root-server.py` is an opt-in adapter around a separately supplied [lite-kit](https://github.com/krevetkqqq/lite-kit) checkout. It does not root or provision anything merely by being imported. Its default command validates inputs; serving requires `--serve`, an enabled local configuration, matching firmware metadata, and TLS inputs. Advertising firmware additionally requires `--offer-firmware`.

Copy `root-config.example.json` to ignored `root-config.local.json` and supply real values locally. The example is disabled and uses reserved documentation addresses and a synthetic device/build identity. Configure DNS/HTTP scoping for the intended device before any serving. The three public vendor DNS names are protocol identifiers, not user infrastructure.

External inputs under the kit directory are `scripts/equasar_server.py`, `certs/fullchain.pem`, `certs/quasar-all.key`, and a matching `firmware/yandexmicro/manifest.json` plus ZIP. This repository includes none of those files. Preserve upstream licensing and obtain firmware/certificates separately. Never publish real local configuration, firmware, certificate private keys, or live traffic.

Synthetic tests need an existing Python 3 and OpenSSL. They use an external upstream Python file, generate a one-day localhost certificate and synthetic firmware in ignored `temp/`, mock upstream fetches, and bind only loopback listeners:

```sh
EQUASAR_KIT_PATH=/path/to/independently-obtained/lite-kit python3 support/provisioning/test-scoped-root-server.py
```

Without the external checkout, the suite explicitly skips rather than claiming an integration pass. Run `python3 support/provisioning/scoped-root-server.py --help` to inspect CLI options without reading local runtime inputs.
