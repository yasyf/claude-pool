# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `clp add` detects when a login is the same subscription already in the pool
  (by accountUuid) and, on a TTY, asks before adding it again; non-interactive
  runs warn and proceed so automation is never blocked.

### Changed
- Trimmed the add flow to what the user can act on: dropped the seeding,
  overlay, and cleanup chatter and the redundant "closed claude" line. "Add
  another account?" now defaults to yes after the first account, and the closing
  line just reports the pool total.

## [0.5.0] - 2026-06-08

### Removed
- Credential adoption. `clp add` no longer copies plain claude's current login
  into a pool account. Adopting meant refreshing plain claude's single-use
  OAuth refresh token, which rotation invalidated — silently logging plain
  claude out. Every account now logs in with its own `claude /login`, so plain
  claude's token is never read or spent. The `keychain.ReadCanonical` /
  `CanonicalExists` accessors and the `pool.CanonicalReader` seam are gone; no
  code path can name the canonical Keychain item anymore.

### Fixed
- Usage decode crashed when the API returned `resets_at` as a string. The field
  now accepts a JSON number, a numeric string, or an RFC3339 timestamp.

### Changed
- Reworked the CLI output: a shared styled output layer with consistent,
  flush-left formatting and a single huh form theme. Human flows (`add`,
  `status`, `select`, `run`) no longer leak internal ids, Keychain service
  names, paths, or overlay kind; those stay in the inspection commands
  (`clp list`, `clp doctor`). Copy across every command rewritten for clarity.

## [0.4.0] - 2026-06-06

### Added
- `clp add` — pool a Claude Max/Pro subscription (interactive `claude /login`),
  each account in its own `~/.cc-pool/accounts/acct-NN` config dir with its own
  independent OAuth grant and Keychain credential. Auto-initializes the pool
  and starts the background daemon on first run; loops to onboard several
  subscriptions in one go. Plain `claude` on `~/.claude` is never part of the
  pool and can never be logged out by it.
- New account dirs are seeded from `~/.claude.json` (top-level `oauthAccount`
  stripped; `claude /login` writes the account's own), so pooled sessions
  inherit settings, MCP servers, and per-project tool approvals instead of
  hitting the first-run onboarding wizard.
- `clp select` — print the emptiest account's config dir, scored from live
  5-hour / 7-day usage with imminent-reset credit, low-headroom barrier, and
  burn-rate terms; cwd-keyed session stickiness for prompt-cache continuity.
- `clp run` / `clp status` / `clp list` / `clp env` / `clp doctor` /
  `clp remove` — session lifecycle, live usage table, account inspection,
  drift repair, and account removal.
- Shared overlay so every account presents all of `~/.claude`: symlink
  provider (default, pure Go) and optional fuse-t live mirror (`-tags fuse`).
  Per-account private entries: `daemon/`, `ide/`, `backups/`, and
  `.claude.json`.
- User LaunchAgent daemon (`clp service`, `brew services`): usage polling with
  backoff, idle-account token refresh, score caching, fuse mount lifecycle.
  `clp add`/`clp init` start it automatically and restart it after upgrades.
- `clp init` — optional explicit pool setup (state dir + daemon); `clp add`
  does the same automatically.
- Homebrew formula with `clp` symlink (prebuilt universal binary; fuse build
  picked automatically when fuse-t is present); CI and release workflows.
- License: PolyForm Noncommercial 1.0.0.

[Unreleased]: https://github.com/yasyf/cc-pool/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/yasyf/cc-pool/compare/v0.3.0...v0.4.0
