package service

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// BrewLabel is the launchd label Homebrew assigns the cc-pool service.
const BrewLabel = "homebrew.mxcl." + FormulaName

// brewServices runs `brew services <action> cc-pool`, streaming brew's
// output to the user.
func brewServices(action string) error {
	cmd := exec.Command("brew", "services", action, FormulaName)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// BrewStart starts the daemon via brew services (installs the user agent).
func BrewStart() error { return brewServices("start") }

// BrewKickstart forces launchd to (re)exec the brew-managed daemon now.
// `brew services start` only bootstraps the job; on a stop/start bootout race it
// can leave the job loaded-but-never-running (RunAtLoad fires only at
// bootstrap), so we kick it explicitly — the same `kickstart -k` the
// self-managed Install path uses.
func BrewKickstart() error {
	target := domainTarget() + "/" + BrewLabel
	if out, err := launchctl("kickstart", "-k", target); err != nil {
		return fmt.Errorf("launchctl kickstart %s: %v: %s", target, err, out)
	}
	return nil
}

// BrewStop stops and unloads the brew-managed agent.
func BrewStop() error { return brewServices("stop") }

// BrewInfo returns `brew services info cc-pool` output for status display.
func BrewInfo() (string, error) {
	out, err := exec.Command("brew", "services", "info", FormulaName).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
