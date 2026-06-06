# cc-pool

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
clp init                                   # register ~/.claude as acct-00 (does NOT move it)
clp add                                    # log in another subscription (acct-01)
clp add                                    # ...and another (acct-02)
clp status                                 # live table: per-account 5h/7d remaining, score, sessions

CLAUDE_CONFIG_DIR=$(clp select) claude     # launch on the emptiest account
claude                                     # plain claude STILL works (acct-00 / ~/.claude)
```

Make it the default in your shell:

```sh
alias cl='CLAUDE_CONFIG_DIR=$(clp select) claude'
```

## How it works

### `~/.claude` is sacred

`~/.claude` is **never moved**. It stays the canonical config dir, so plain
`claude` keeps working exactly as before. It doubles as **acct-00** and as the
**shared base** every pooled account mirrors.

### One real config dir per account

Claude Code namespaces its Keychain credential **per config dir**: the default
`~/.claude` uses the item `Claude Code-credentials`; a custom `CLAUDE_CONFIG_DIR`
gets a suffixed item `Claude Code-credentials-<hash>`. cc-pool gives each
account a real, unique dir (`~/.cc-pool/accounts/acct-NN`) so each gets its own
Keychain item and runs on its own **subscription** (never API billing).

> **acct-00 nuance.** Plain `claude` uses the *un-suffixed* default item.
> Because `CLAUDE_CONFIG_DIR=$(clp select)` *sets* the variable, selecting
> acct-00 would otherwise make Claude look for a *suffixed* item that doesn't
> exist. `clp init` therefore mirrors acct-00's credential into a suffixed item
> and the daemon keeps the two in lockstep, so acct-00 is selectable like any
> other account while plain `claude` stays untouched.

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

A small set of instance-local entries (`daemon/`, `ide/`) are **not** shared —
they hold Claude's own PID-keyed supervisor and IDE lock/socket files, which
would conflict across concurrent sessions. Each account gets its own.

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
check-in — and it never refreshes acct-00, whose token plain `claude` owns),
caches scores, and — with the fuse overlay — owns the mount lifecycle. If the
daemon isn't running, `clp select` auto-spawns it (≤2s) or samples live.

No secrets are ever stored in cc-pool's database — the macOS Keychain is the
only secret store.

## Commands

| Command | What it does |
|---|---|
| `clp init` | Register `~/.claude` as acct-00; set up the pool; offer to install the daemon |
| `clp add` | Pool another subscription (interactive `claude /login`) |
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

MIT © Yasyf Mohamedali
