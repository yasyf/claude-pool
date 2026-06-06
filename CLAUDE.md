# Claude Rules for cc-pool

@AGENTS.md

## Claude-Specific Rules

- **Always use `AskUserQuestion` for clarifying questions** (per AGENTS.md "Ask Before Assuming") — concrete options beat inline prose questions; batch related questions into one call.
- **Track non-trivial work with tasks** (`TaskCreate`/`TaskUpdate`): pending → in_progress → completed. Create tasks before starting; update as you discover work.
- **Verify every change** with `go vet ./... && go test ./...` before claiming it done. For binary-affecting changes, also `CGO_ENABLED=0 go build ./cmd/cc-pool` and `go build -tags fuse ./...`.
- **Never run the daemon, `launchctl`, or `security` mutations against the user's real state** during development unless explicitly asked — tests must not touch `~/.claude`, `~/.cc-pool`, or the Keychain.
