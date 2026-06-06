package service

import (
	"os"
	"os/exec"
	"strings"
)

// brewServices runs `brew services <action> cc-pool`, streaming brew's
// output to the user.
func brewServices(action string) error {
	cmd := exec.Command("brew", "services", action, FormulaName)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// BrewStart starts the daemon via brew services (installs the user agent).
func BrewStart() error { return brewServices("start") }

// BrewStop stops and unloads the brew-managed agent.
func BrewStop() error { return brewServices("stop") }

// BrewInfo returns `brew services info cc-pool` output for status display.
func BrewInfo() (string, error) {
	out, err := exec.Command("brew", "services", "info", FormulaName).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
