# cc-pool Notification Center widget

A macOS WidgetKit widget that shows `ccp status` at a glance: per-account 5h/7d
usage bars, live-session counts, and flags (stale / rate-limited / exhausted /
overage), in the same order as the CLI's table.

## How it gets data

The daemon writes an atomic snapshot to `~/.cc-pool/status.json` after every
completed poll (~3 min) — same schema as `ccp status --json`. The sandboxed
widget extension reads that file via a read-only sandbox exception for
`~/.cc-pool/`; it never touches the socket, the database, or the Keychain.

The host app (`CCPoolStatus.app`) is a Dock-less agent whose only jobs are to
register the widget with the system and — while running — watch `~/.cc-pool`
and reload the widget when the snapshot changes. Without the app running, the
widget still works on WidgetKit's lazier schedule. Add the app to
System Settings → Login Items for always-fresh updates.

## Install

```sh
ccp widget
```

That installs the prebuilt app from the `cc-pool-status` Homebrew cask
(passing `--no-quarantine` — the app is ad-hoc signed, and Gatekeeper blocks a
quarantined copy), launches it once so macOS discovers the widget, and prints
the enable steps:

Open Notification Center (click the menu-bar clock), scroll down →
**Edit Widgets** → search "cc-pool" → add the small or medium widget. Desktop
widgets work too: right-click the desktop → Edit Widgets.

If the widget doesn't appear in the gallery: `killall NotificationCenter
chronod`, relaunch the app, and re-open the gallery.

To remove it: `brew uninstall --cask cc-pool-status`.

## Build from source (development)

Requires full Xcode (CommandLineTools alone cannot build app targets) and
[XcodeGen](https://github.com/yonaskolb/XcodeGen) (`brew install xcodegen`).

```sh
cd widget
xcodegen generate
xcodebuild -project CCPoolStatus.xcodeproj -scheme CCPoolStatus \
  -configuration Release -derivedDataPath build build
ditto build/Build/Products/Release/CCPoolStatus.app ~/Applications/CCPoolStatus.app
open ~/Applications/CCPoolStatus.app
```

`project.yml` is the source of truth; the `.xcodeproj` and everything under
`Generated/` are emitted by xcodegen and gitignored. To work in the Xcode UI:
`xcodegen generate && open CCPoolStatus.xcodeproj`.

## Signing

Builds are ad-hoc signed (`CODE_SIGN_IDENTITY=-`) — fine for an app that is
never quarantined (the cask installs with `--no-quarantine`; local builds
never get the quarantine bit). If chronod refuses to load the ad-hoc-signed
widget, build with a free personal team instead:

```sh
xcodebuild -project CCPoolStatus.xcodeproj -scheme CCPoolStatus \
  -configuration Release -derivedDataPath build build \
  CODE_SIGN_STYLE=Automatic DEVELOPMENT_TEAM=<YOUR_TEAM_ID> -allowProvisioningUpdates
```

## Troubleshooting

- `codesign -d --entitlements - ~/Applications/CCPoolStatus.app/Contents/PlugIns/CCPoolStatusWidget.appex`
  must show `com.apple.security.app-sandbox` plus the
  `temporary-exception.files.home-relative-path.read-only` entry for `/.cc-pool/`.
- `log stream --predicate 'process == "chronod" OR process CONTAINS "CCPoolStatusWidget"'`
  shows widget discovery/launch errors.
- "daemon not running" in the widget → `ccp service install` (or
  `brew services start cc-pool`); "status unreadable" → version skew between
  the snapshot and the widget — rebuild the widget or update cc-pool, and check
  `ccp status --json`.
