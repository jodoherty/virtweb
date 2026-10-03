# Vendored: xterm.js 6.0.0 + addon-fit 0.11.0

These files are the **published npm artifacts, unmodified**. Do not edit
anything under this directory. To replace the libraries: delete this
directory, re-fetch the tarballs below, re-verify the sha256, and copy the
same file set back. The implementation never touches the npm registry —
the tarballs are a one-time vendor fetch (curl), recorded here.

## @xterm/xterm — 6.0.0 (MIT)

- Tarball: `https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz`
- sha256: `908e66e04af6c8dc6b00dd3b54de088e2e81e5ed866284fd6c2fb3c2d1c7a3f6`
- Fetched: 2026-10-03
- Files kept: `lib/xterm.js` (UMD build, all named exports land on
  `globalThis`, incl. `Terminal`), `lib/xterm.js.map`, `css/xterm.css`,
  `typings/xterm.d.ts`, `LICENSE`, `README.md`, `package.json`
- Cross-reference: full upstream source tree `xterm.js-6.0.0/` in the repo
  root (same version; kept as the reviewable source of truth)

## @xterm/addon-fit — 0.11.0 (MIT)

- Tarball: `https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-0.11.0.tgz`
- sha256: `26003b4517a132b64e4ff228fd88a5fda3fff5e606c76093f6dcff772e9ecec0`
- Fetched: 2026-10-03 (latest release at fetch time; no peer pins)
- Files kept (under `addon-fit/`): `lib/addon-fit.js` (UMD; note the
  constructor is reached as `globalThis.FitAddon.FitAddon` — this build
  assigns the module namespace to `FitAddon`, unlike core xterm which
  spreads its exports onto `globalThis`), `lib/addon-fit.js.map`,
  `typings/addon-fit.d.ts`, `LICENSE`, `README.md`, `package.json`
