# cc-pool Development Guide

Full style guide: [STYLEGUIDE.md](STYLEGUIDE.md)

## Project Basics

cc-pool (`clp`) pools several Claude Max/Pro subscriptions and launches each Claude Code session on the emptiest account. Go, macOS-only, single binary.

- **Build**: `CGO_ENABLED=0 go build ./cmd/cc-pool` (pure-Go default; `-tags fuse` needs cgo + fuse-t)
- **Test**: `go test ./...` — must pass with no network, no Keychain, no daemon
- **Vet**: `go vet ./...` before every commit

## Repository Structure

```
cc-pool/
├── cmd/cc-pool/        # main: CLI entrypoint (installs as cc-pool, clp symlink)
├── internal/
│   ├── cli/            # clp subcommands (init, add, select, run, status, doctor, …)
│   ├── daemon/         # background poller: usage polling, idle token refresh, socket protocol
│   ├── keychain/       # macOS Keychain access for Claude Code credentials
│   ├── oauth/          # Claude OAuth refresh + /api/oauth/usage client
│   ├── overlay/        # shared ~/.claude overlay providers (symlink, fuse-t mirror)
│   ├── pool/           # account dirs, paths, pool manager
│   ├── procscan/       # detect live claude sessions per config dir
│   ├── score/          # account scoring (5h/7d headroom, reset credit, burn rate)
│   ├── service/        # LaunchAgent install / brew services delegation
│   ├── store/          # SQLite state (no secrets — Keychain only)
│   └── version/        # build metadata injected via -ldflags
├── launchd/            # LaunchAgent plist template
├── Formula/            # Homebrew formula
├── docs/               # verification notes
├── AGENTS.md           # This file — shared conventions
└── STYLEGUIDE.md       # Full style guide
```

Two filesystem trees, never confused:

- `~/.claude` — canonical Claude Code config dir. **NEVER moved, modified structurally, or registered as a pool account.** It is plain `claude`'s home and the shared overlay base; plain `claude` must keep working untouched.
- `~/.cc-pool/` — cc-pool's own state (sqlite db, daemon socket, logs) plus `accounts/acct-NN` pool config dirs (ids start at 1).

Safety rules baked into the architecture — do not regress them:

1. **The pool NEVER writes or deletes the canonical unsuffixed Keychain item (`Claude Code-credentials`) and never mutates plain claude's OAuth state.** One read-only exception: `clp add` adoption may READ the canonical item via `keychain.ReadCanonical`/`CanonicalExists` — exposed to the pool only through the write-less `pool.CanonicalReader` seam, so mutation is impossible to express — to copy the user's current login into a pool account's own suffixed item, which is then immediately refreshed onto its own chain. Every pool account — including the user's main subscription — still has its own config dir, its own refresh-token chain, and its own suffixed Keychain item. `keychain.ServiceName` always emits a suffixed name; outside the canonical accessors, no code path can even name the canonical item.
2. **No secrets in SQLite** — the macOS Keychain is the sole secret store.
3. **Account dir strings are hashed for Keychain service names** — the path string `clp` emits and the string hashed must stay byte-identical. No realpath/normalization divergence.

Known follow-up (documented, test-pinned in `TestConcurrentPrepareAddIndexRace`): two concurrent `clp add`s can be handed the same account index because no row exists until FinalizeAdd; fixing it needs a pending-row reservation.

## Ask Before Assuming

When a request is ambiguous — unclear scope, multiple plausible interpretations, undefined edge cases — stop and ask. Propose 2–4 concrete options, or list the assumptions you'd otherwise make. One wrong implementation costs more than ten clarifying exchanges.

## Code Review Response (Plan Re-Entry)

When the user reviews your code and re-enters plan mode (inline diff comments, a numbered list of issues, or other review-shaped feedback):

1. **Draft a new plan**, not a code change — re-entry means "align on what you'll do next."
2. **Inline every comment verbatim** with an anchor (`#N`, file:line). Never paraphrase; the user must see each comment reproduced exactly.
3. **Cluster into themes when >5 comments**, and extrapolate each rule to other call sites with the same problem.
4. **Map every comment** in a final table: `# | file:line | verbatim | cluster`. No comment silently dropped.
5. **Don't implement before approval.**

If the user responds to a plan with questions, answer conversationally and surface choices via AskUserQuestion — don't bake answers into the plan before they choose.

## Parallelize Independent Work

Independent investigations dispatch concurrently — one message, multiple subagent calls. A lone ad-hoc lookup gets one subagent; substantive multi-step work gets a structured fan-out with verification.

## Code Search

`semble` is wired up via `.mcp.json` (project-scoped MCP server, runs via `uvx` — nothing to install). It's the default tool for any "find code by intent or symbol" question:

1. **"How do we do X?" / "Where is the code that does Y?"** → `semble.search("...")`
2. **"Where is `Foo` defined?"** → `semble.search("Foo")` (or `search("type Foo")` for a relevance boost)
3. **"Show me other code like this"** → `semble.find_related` on a prior hit
4. **Cross-repo lookup** → pass an `https://...git` URL as `repo`

`repo` defaults to the current project root for local searches. Semble is purely semantic — it ranks by meaning, not substring, so it won't find literal strings that don't appear in nearby code.

Reach for the **LSP** when the answer must be *exhaustive* or *structural*: `findReferences`/`incomingCalls` for "who calls X", `goToImplementation` for "what implements interface I", `hover` for types.

Reach for **`Grep`** only for material neither tool indexes: literal *content* of strings/comments (error messages, env-var names, Keychain service names, TODOs) and non-source files (plists, JSON, logs). File-pattern questions go through `Glob`.

## Style Rules (summary — see STYLEGUIDE.md)

- **Fail fast, fail loud.** No silent fallbacks, sentinels, or defensive coding. No back-compat shims — delete dead code.
- **Errors**: wrap once per layer with `%w`; sentinels + `errors.Is/As`; never log-and-return; fallible call adjacent to its `if err != nil`.
- **Comments**: godoc on every exported symbol; inside bodies only TODOs, non-obvious workarounds/invariants, disabled code.
- **Flat over nested**: early returns; nesting >3 is a smell.
- **Concurrency**: `ctx` first param; every goroutine has a defined exit; locks never held across I/O.
- **Tests must catch bugs**: strong assertions, table-driven with named cases, mock externals only, negative tests required, never degrade a test to make it pass.
- **Leave it better**: fix guide violations in code you're touching; stay in scope otherwise.

## Mechanical Linting

`gofmt` and `go vet` own formatting and mechanical issues. Don't hand-flag them in review; only fix issues requiring judgment — logic, architecture, edge cases.
