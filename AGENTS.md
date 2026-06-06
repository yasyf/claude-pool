# cc-pool Development Guide

Full style guide: [STYLEGUIDE.md](STYLEGUIDE.md)

## Project Basics

cc-pool (`clp`) pools several Claude Max/Pro subscriptions and launches each Claude Code session on the emptiest account. Go, macOS-only, single binary.

- **Build**: `CGO_ENABLED=0 go build ./cmd/cc-pool` (pure-Go default; `-tags fuse` needs cgo + fuse-t)
- **Test**: `go test ./...` — must pass with no network, no Keychain, no daemon
- **Vet**: `go vet ./...` before every commit

Two filesystem trees, never confused:

- `~/.claude` — canonical Claude Code config dir. **NEVER moved or modified structurally.** It is acct-00 and the shared base; plain `claude` must keep working untouched.
- `~/.cc-pool/` — cc-pool's own state (sqlite db, daemon socket, logs) plus `accounts/acct-NN` pool config dirs.

Safety rules baked into the architecture — do not regress them:

1. **The daemon NEVER POST-refreshes acct-00's OAuth token** (shared single-use refresh token with plain `claude`; refreshing it races and logs the user out).
2. **No secrets in SQLite** — the macOS Keychain is the sole secret store.
3. **Account dir strings are hashed for Keychain service names** — the path string `clp` emits and the string hashed must stay byte-identical. No realpath/normalization divergence.

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
