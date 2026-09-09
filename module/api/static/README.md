# Embedded Console Assets

The first-party console scripts are split by ownership:

- `console-core.js`: session-safe requests, permissions, modal and navigation guards.
- `console-i18n.js`: English/Chinese UI selection; only the locale preference is stored in local storage.
- `console-media.js`: stream inventory, playback, publishing, and media statistics.
- `console-management.js`: cluster, SIP/GB28181 labs, recordings, security, and view polling.
- `console-config.js`: full-document drafts, common controls, redacted comparison with schema reload impact, history, and revision-aware writes. The common server-name control is read-only because runtime application rejects name changes.
- `console-lists.js`: bounded list navigation, audit filtering, and authenticated export.
- `console-init.js`: UI event binding and startup.

The Go `embed` filesystem serves these assets. No Node.js installation or asset
build is required to build or run LiveForge.

## YAML Parser

`yaml.min.js` bundles [yaml 2.9.0](https://www.npmjs.com/package/yaml/v/2.9.0)
from [eemeli/yaml](https://github.com/eemeli/yaml), licensed under ISC; see
`yaml.LICENSE`. The common settings form edits its `Document` AST, retaining
comments and fields outside the typed server configuration. The parser also
supports bounded alias expansion for the redacted semantic comparison.

The npm package integrity is
`sha512-2AvhNX3mb8zd6Zy7INTtSpl1F15HW6Wnqj0srWlkKLcpYl/gMIMJiyuGq2KeI2YFxUPjdlB+3Lc10seMLtL4cA==`.
The generated bundle SHA-256 is
`61681504d0a0f320c404f5ba78105627bba6a1231f137d2738f0b79fa9fe04e1`.

To reproduce from the repository root using Node.js/npm:

```sh
npm install --prefix /tmp/liveforge-console-bundle --no-audit --no-fund yaml@2.9.0 esbuild@0.28.2
/tmp/liveforge-console-bundle/node_modules/.bin/esbuild /tmp/liveforge-console-bundle/node_modules/yaml/browser/index.js --bundle --minify --format=iife --global-name=LiveForgeYAML --outfile=module/api/static/yaml.min.js --banner:js='/* yaml 2.9.0, ISC license; see yaml.LICENSE. Bundled with esbuild 0.28.2. */'
```

The existing `mpegts.min.js`, `hls.min.js`, and `dash.all.min.js` player libraries
are independent of the configuration editor and remain in their existing
bundled versions.
