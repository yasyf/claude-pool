# cc-pool

[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/cc-pool/ci.yml?branch=main&label=CI)](https://github.com/yasyf/cc-pool/actions/workflows/ci.yml)
[![License: PolyForm Noncommercial](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](https://github.com/yasyf/cc-pool/blob/main/LICENSE)

**Predictive, start-of-session load-balancing across multiple Claude subscriptions — for macOS.**

If you have several Claude Max/Pro subscriptions, `cc-pool` launches each
session on the **emptiest** account, so you stop hitting 5-hour and weekly
limits on one account while another sits idle:

```sh
CLAUDE_CONFIG_DIR=$(clp select) claude
```

Unlike reactive proxies or manual switchers, `clp select` predicts the best
account *before* the session starts, from live 5-hour / 7-day usage. Plain
`claude` keeps working, untouched.

---

## Install

```sh
brew tap yasyf/cc-pool https://github.com/yasyf/cc-pool
brew install yasyf/cc-pool/cc-pool
```

The binary installs as `cc-pool` with a `clp` symlink.

## Quickstart

```sh
clp add                                    # log in your first subscription (auto-inits the pool)
clp add                                    # ...and another (acct-02)
clp status                                 # live table: per-account 5h/7d remaining, score, sessions

CLAUDE_CONFIG_DIR=$(clp select) claude     # launch on the emptiest account
claude                                     # plain claude STILL works, fully decoupled (~/.claude)
```

Your main subscription joins the pool like any other account: `clp add` gives
it its own login in its own account dir. Plain `claude` stays fully decoupled —
the pool can never log it out.

Make it the default in your shell:

```sh
alias cl='CLAUDE_CONFIG_DIR=$(clp select) claude'
```

## How it works

### `~/.claude` is sacred

`~/.claude` is **never moved** and never registered as a pool account. It stays
the canonical config dir, so plain `claude` keeps working exactly as before, and
serves as the **shared base** every pooled account mirrors. The pool never
reads or writes plain claude's credential or state. Every account, including
your main subscription, joins with its own `claude /login`, so its token chain
is fully independent of plain claude's.

### One real config dir per account

Claude Code namespaces its Keychain credential **per config dir**: the default
`~/.claude` uses the item `Claude Code-credentials`; a custom `CLAUDE_CONFIG_DIR`
gets a suffixed item `Claude Code-credentials-<hash>`. cc-pool gives each
account a real, unique dir (`~/.cc-pool/accounts/acct-NN`) so each gets its own
Keychain item, its own independent OAuth grant (its own refresh-token chain),
and runs on its own **subscription** (never API billing). Each account dir is
also seeded with a copy of your `~/.claude.json` with the identity stripped (the
account's own login writes its identity), so pooled sessions inherit your
settings, MCP servers, and per-project tool approvals instead of running
first-run onboarding.

### Shared overlay

Each account dir presents **all of `~/.claude`** — your `projects/`, `skills/`,
`settings.json`, `history.jsonl`, etc. — with writes passing straight back, so
every session shares the same workspace. Two providers:

- **symlink** (default, zero-dependency): symlinks each top-level entry of
  `~/.claude` into the account dir. New top-level entries are picked up
  automatically at launch (`clp select`/`clp run`), by the daemon, and by
  `clp doctor` — no manual step needed.
- **fuse** (optional, live mirror): an in-process passthrough mirror mounted via
  [fuse-t](https://github.com/macos-fuse-t/fuse-t) (kext-less, mounted as you,
  no root). Auto-includes new entries with no re-sync. Requires a
  `-tags fuse` build and a one-time *Network Volumes* privacy grant.

A small set of per-account entries are **not** shared — `daemon/` and `ide/`
(Claude's PID-keyed supervisor and IDE lock/socket files, which would conflict
across concurrent sessions), `backups/` (rotating backups of each account's
`.claude.json`), and `.claude.json` itself (per-account identity). Each account
gets its own.

### Scoring

For a healthy account the score is exactly:

```
score = 0.70·(100−util_5h) + 0.25·(100−util_7d)
      − 2·active_sessions − 100·rate_limited − 20·stale_or_refresh_failed
```

Near limits it gets smarter (each term reduces to the above when not engaged):
an **imminent reset** is credited (a 90%-used window resetting in 10 min ranks
*up*, not down); a **low-headroom barrier** stops a nearly-exhausted 7-day window
from being masked by 5-hour headroom; and a **burn-rate** term downranks an
account being actively drained. `select` picks argmax. Usage comes from Claude's
own `/api/oauth/usage` endpoint.

### The daemon

`brew services start cc-pool` (Homebrew installs) or `clp service install`
(source builds) runs a **user LaunchAgent** (a root daemon couldn't read your
login Keychain). It polls usage every ~3 min with exponential backoff, refreshes
**idle** accounts' tokens before they expire (a checked-out session owns its own
refresh; the daemon re-reads and adopts whatever token it rotated to on
check-in), caches scores, and — with the fuse overlay — owns the mount
lifecycle. `clp add` and `clp init` start it automatically; if it isn't
running, `clp select` auto-spawns it (≤2s) or samples live.

No secrets are ever stored in cc-pool's database — the macOS Keychain is the
only secret store.

## Commands

| Command | What it does |
|---|---|
| `clp add` | Pool a subscription (interactive `claude /login`; auto-inits the pool and starts the daemon) |
| `clp init` | Prepare the pool state dir and start the daemon (optional — `clp add` does this automatically) |
| `clp select` | Print the emptiest account's config dir (stdout only) — the hot path |
| `clp run -- <args>` | Select an account and exec `claude`, owning the session lifecycle |
| `clp status [-w]` | Live table of usage / score / sessions |
| `clp list` | Static account list |
| `clp env [--account N]` | Print `export` lines to launch a specific account |
| `clp doctor [--fix]` | Re-validate Keychain items and overlays; repair drift |
| `clp remove <id>` | Remove an account from the pool |
| `clp service install\|uninstall\|status` | Manage the daemon (delegates to `brew services` on Homebrew installs) |

## Uninstall

```sh
clp service uninstall            # stop & remove the daemon, unmount fuse overlays
clp service uninstall --purge    # ...and remove all pool accounts/dirs/state
brew uninstall cc-pool
```

`~/.claude` and its credential are never touched.

## Platform

macOS only (developed against macOS 26.5, Claude Code 2.1.16x). The default
build is pure Go; the optional fuse overlay needs cgo + fuse-t.

## License

[PolyForm Noncommercial 1.0.0](https://polyformproject.org/licenses/noncommercial/1.0.0) © Yasyf Mohamedali — free for noncommercial use; see [LICENSE](https://github.com/yasyf/cc-pool/blob/main/LICENSE).
