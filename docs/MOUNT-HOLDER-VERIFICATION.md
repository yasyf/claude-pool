# Mount-holder end-to-end verification

A manual on-device checklist for the detached mount-holder architecture: fuse
mirrors live in a `cc-pool mount-holder` process (socket
`~/.cc-pool/mounts.sock`), the daemon supervises it, and daemon restarts and
upgrades never disturb mounts. Every step needs a real fuse setup — a `-tags
fuse` build, fuse-t installed, the "Network Volumes" grant, and at least one
fuse account (`ccp migrate`).

> **One-time transition note.** Upgrading from a pre-mount-holder release
> (v0.24.x and earlier) drops mounts ONCE: the OLD daemon hosts its mounts
> in-process and still unmounts them when the upgrade restarts it. Close
> fuse-account sessions for that single upgrade. Every restart and upgrade
> after this version is mount-safe.

## 1. Daemon restart under a live session

```sh
ccp run                                   # (human) keep this session open on a fuse account
mount | grep -c cc-pool                   # note the mount count
brew services restart cc-pool
mount | grep -c cc-pool                   # unchanged — mirrors never came down
# the session keeps working: type into it; no fs errors
tail ~/.cc-pool/daemon.log
```

Expect in `daemon.log`: a fresh `daemon <ver> started; socket=…` and per-account
`acct-NN adopted live mount`. Must NOT appear: `mount holder respawned`,
`replacing version-skewed mount holder`, or any unmount of the account dirs.

## 2. Holder crash → respawn + remount

```sh
kill -9 "$(pgrep -f 'mount-holder --socket')"
sleep 12                                  # one supervision tick is 10s
mount | grep -c cc-pool                   # mounts are back
tail ~/.cc-pool/daemon.log
```

Expect, within ~10s, in order:

```
mount holder unreachable
mount holder respawned
acct-NN cleared a dead mount before remounting   # carcass swept, per account
```

## 3. Version skew → deferred under a session, heals when idle

```sh
# Upgrade the binary (or reinstall a different build) and restart the daemon
# while a claude session is live on a fuse account.
brew upgrade cc-pool && brew services restart cc-pool
tail -f ~/.cc-pool/daemon.log
```

Expect while the session lives (logged once per reason, not per tick):

```
deferring replacement of version-skewed mount holder (<old>): N live session(s) on <dir>
```

`ccp status` shows the footer `mount holder <old> skewed; will be replaced when
idle`; `ccp service status` shows `version skew — will be replaced when idle`.
Close the session; within a tick expect:

```
replacing version-skewed mount holder (<old>) with <new>
mount holder replaced at <new>
```

…and the skew footer disappears from `ccp status`.

## 4. Uninstall gates

```sh
# (a) gate: with a live session on a fuse account
ccp service uninstall            # refuses: "live claude sessions are using pool
                                 #  accounts: acct-NN (pid …) — close them or pass --force"
# (b) --force overrides
ccp service uninstall --force    # proceeds: "Stopped the daemon." then
                                 # "Stopped the mount holder."; mount | grep cc-pool is empty
# (c) purge aborts on a survivor mount: wedge a mirror (e.g. hold a cwd inside
#     it from another shell so the unmount fails), then
ccp service uninstall --purge    # "refusing to purge: acct-NN is still a live
                                 #  mountpoint; unmount it first …"; ~/.cc-pool survives
```

## 5. TCC behavior across a binary swap

The macOS "Network Volumes" grant is per process path. The Homebrew install
execs the stable `opt` symlink precisely so upgrades keep the grant.

```sh
# (a) upgrade via brew (opt path stable): kill the holder so the new binary
#     respawns it, expect NO new TCC prompt and a clean remount (step 2 lines).
# (b) run a holder from a NEW path (e.g. a source build in /tmp): the first
#     mount sits on the one-time prompt; until granted expect
#     "acct-NN fuse mount blocked pending the macOS \"Network Volumes\" grant,
#      retrying next poll" in daemon.log, the `ccp status` footer
#     "mount holder: TCC blocked — …", and a failing `ccp doctor` "mount holder
#     TCC" check. Grant it; the next poll mounts and all three surfaces clear.
```
