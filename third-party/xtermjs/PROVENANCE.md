# Forks: @xterm/xterm 6.0.0 + @xterm/addon-fit 0.11.0 (MIT)

These are **first-party forks** of the xterm.js npm artifacts, developed
in this repository's git history (no separate fork repository) — the
virtweb commits touching this directory *are* the forks' history. The
shipped files are the published UMD builds; the full upstream source
tree `xterm.js-6.0.0/` in the repo root is kept as the reviewable source
of truth. Edits are allowed but must be minimal and confined, and
recorded in the fork delta below.

## @xterm/xterm — 6.0.0 (MIT)

- Tarball: `https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz`
- sha256: `908e66e04af6c8dc6b00dd3b54de088e2e81e5ed866284fd6c2fb3c2d1c7a3f6`
- Fetched: 2026-10-03
- Files kept: `lib/xterm.js` (UMD build, all named exports land on
  `globalThis`, incl. `Terminal`), `css/xterm.css`,
  `typings/xterm.d.ts`, `LICENSE`, `README.md`, `package.json`
- Cross-reference: full upstream source tree `xterm.js-6.0.0/` in the
  repo root (same version; the reviewable source of truth)

## @xterm/addon-fit — 0.11.0 (MIT)

- Tarball: `https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-0.11.0.tgz`
- sha256: `26003b4517a132b64e4ff228fd88a5fda3fff5e606c76093f6dcff772e9ecec0`
- Fetched: 2026-10-03 (latest release at fetch time; no peer pins)
- Files kept (under `addon-fit/`): `lib/addon-fit.js` (UMD; note the
  constructor is reached as `globalThis.FitAddon.FitAddon` — this build
  assigns the module namespace to `FitAddon`, unlike core xterm which
  spreads its exports onto `globalThis`), `typings/addon-fit.d.ts`,
  `LICENSE`, `README.md`, `package.json`

## Fork delta (keep changes minimal and confined)

*(none — byte-identical to the pinned npm artifacts as of 2026-10-03)*

Record any change here: file, what, why, and the virtweb commit.

## Re-basing on upstream

1. Re-fetch the new tarball(s), verify the sha256, refresh the source
   tree at repo root `xterm.js-6.0.0/`.
2. Copy the same file set back; re-apply any fork delta listed above;
   run the e2e suite (console specs included).
3. Update versions, hashes, and delta in this file.

## Audit notes (2026-10-03, virtweb security review)

Audited as first-party code against the source tree at repo root
`xterm.js-6.0.0/` (shipped UMD pinned by sha256 in this file). Trust
boundary: a hostile guest console feeds us untrusted terminal bytes
(escape sequences, OSC data); xterm is the parser/renderer. Findings:

**Safe (verified):**
- No DOM XSS sinks in `src/` (no `innerHTML`/`document.write`/
  `eval`); the renderer builds the DOM from text nodes/elements only.
- **OSC 8 hyperlinks are sanitized by default** (`browser/OscLinkProvider.ts`):
  unless `linkHandler.allowNonHttpProtocols` is set, only `http:` /
  `https:` URIs are offered as links (invalid URIs are dropped), and
  the default handler asks for `confirm()` and opens in a new window
  with `opener = null`. virtweb's `console.js` sets no `linkHandler`,
  so the strict default applies.
- No `document.title` writes from terminal data (OSC 0/2 only update
  xterm's internal title, which virtweb does not surface).
- OSC 52 (clipboard write) is **not implemented** in 6.0.0 — terminal
  data cannot touch the user's clipboard.

**Accepted residuals:**
- A hostile console can still paint arbitrary text/graphics and
  trigger the OSC-8 `confirm()` prompt (phishing-style); that is
  inherent to rendering an untrusted terminal and the prompt + strict
  protocol filter are the mitigations.
