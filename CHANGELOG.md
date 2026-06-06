# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
