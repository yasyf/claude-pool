# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-06-06

### Added
- `clp init` / `clp add` / `clp remove` — register `~/.claude` as acct-00 (never
  moved) and pool additional Claude Max/Pro subscriptions, each in its own
  `~/.cc-pool/accounts/acct-NN` config dir with its own Keychain credential.
- `clp select` — print the emptiest account's config dir, scored from live
  5-hour / 7-day usage with imminent-reset credit, low-headroom barrier, and
  burn-rate terms; cwd-keyed session stickiness for prompt-cache continuity.
- `clp run` / `clp status` / `clp list` / `clp env` / `clp doctor` — session
  lifecycle, live usage table, account inspection, and drift repair.
- Shared overlay so every account presents all of `~/.claude`: symlink provider
  (default, pure Go) and optional fuse-t live mirror (`-tags fuse`).
- User LaunchAgent daemon (`clp service`, `brew services`): usage polling with
  backoff, idle-account token refresh (never acct-00), score caching, fuse
  mount lifecycle.
- Homebrew formula with `clp` symlink; CI and release workflows.

[Unreleased]: https://github.com/yasyf/cc-pool/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/yasyf/cc-pool/releases/tag/v0.1.0
