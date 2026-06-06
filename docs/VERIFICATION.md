# End-to-end verification

A manual checklist that maps 1:1 to the acceptance criteria. Steps marked
**(human)** need an interactive Claude login or a real second subscription and
cannot be automated. Run from a clean machine state where possible.

## 0. Build / install

```sh
# From source:
CGO_ENABLED=0 go build -o /usr/local/bin/cc-pool ./cmd/cc-pool
ln -sf /usr/local/bin/cc-pool /usr/local/bin/clp

# Or via the tap once released:
brew tap yasyf/cc-pool https://github.com/yasyf/cc-pool
brew install yasyf/cc-pool/cc-pool
```

## 1. Release installs and reports version
```sh
clp --version          # prints the version
cc-pool --version  # same binary, both names work
```

## 2. init keeps plain claude working; add a second account; status; daemon refresh
```sh
clp init               # registers acct-00; mirrors its credential; points at the daemon
claude                 # (human) plain claude STILL launches on ~/.claude, untouched
clp add                # (human) interactive: logs in an account, then loops ("Add another?")
                       #         so you can onboard several subscriptions in one run
clp status             # all accounts shown with live 5h/7d remaining + score
brew services start cc-pool   # brew install: enable the daemon (or `clp service install` on a source build)
clp service status     # shows "Homebrew (brew services)" or self-managed, + Daemon: running
```
Verify in `Keychain Access` that there is now `Claude Code-credentials`
(canonical) plus suffixed `Claude Code-credentials-<hash>` items — distinct per
account, and that daemon refreshes do not raise a prompt.

## 3. Drive one account up → select returns the other (stdout = path only)
```sh
# Burn 5h usage on acct-01 (human: run a heavy session), then:
clp select                       # prints ONLY a config dir on stdout
clp select 2>/dev/null           # stderr suppressed → still just the path
test -n "$(clp select 2>/dev/null)"   # non-empty path
# Expect the LESS-used account's dir.
```

## 4. Launch on the selected account → subscription, shared workspace, distinct keychain
```sh
CLAUDE_CONFIG_DIR=$(clp select) claude   # (human)
#   In-session /status shows the expected account on its SUBSCRIPTION (not "Claude API").
ls "$(clp select 2>/dev/null)/projects"  # shared projects identical to plain claude's
diff <(ls ~/.claude/skills) <(ls "$(clp env --account 1 2>/dev/null | sed -n 's/.*CLAUDE_CONFIG_DIR=//p' | tr -d "'")/skills")
```

## 5. Overlay parity (writes shared both ways)
```sh
# symlink provider (default):
ls -la ~/.cc-pool/accounts/acct-01            # top-level entries are symlinks into ~/.claude
#                                          except daemon/ and ide/ (private dirs)
echo hi > ~/.cc-pool/accounts/acct-01/projects/_clp_probe && \
  cat ~/.claude/projects/_clp_probe       # write through the overlay lands in ~/.claude
rm ~/.claude/projects/_clp_probe

# fuse provider (only with a -tags fuse build + fuse-t + Network Volumes grant):
mount | grep cc-pool                  # account dir is a fuse-t mirror mount
```

## 6. End session → token adopted, checkout released; uninstall leaves ~/.claude intact
```sh
# After a `clp run` session exits, the daemon re-reads any rotated token and
# closes the checkout:
clp run -- -p "hello"                     # (human) owns the PID; adopts rotated token on exit
clp status                                # active sessions for that account back to baseline

clp service uninstall                     # stops daemon, unmounts any fuse overlays
clp service uninstall --purge             # also removes pool accounts/dirs/state
brew uninstall cc-pool                # (if installed via brew)
test -d ~/.claude && claude --version     # (human) ~/.claude intact; plain claude still works
```

## Automated coverage (CI, no human needed)
- `go test ./...` — keychain argv contract (fake `security`), OAuth refresh/usage
  request+response shapes and unit normalization, sqlite store, scoring +
  tie-breaks, symlink overlay setup/sync/health/teardown, process-env parsing.
- `go build -tags fuse` — cgofuse + fuse-t compilation canary (the live mount
  test is `t.Skip`-ped without the interactive Network Volumes grant).
