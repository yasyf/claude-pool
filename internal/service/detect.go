package service

import (
	"os"
	"path/filepath"
	"strings"
)

// FormulaName is the Homebrew formula / brew-services name.
const FormulaName = "cc-pool"

// IsBrewManaged reports whether this binary was installed via Homebrew, in
// which case the daemon should be managed with `brew services` rather than the
// self-rolled launchctl path. It inspects the executable path only (no shelling
// out), so it is cheap enough for any code path.
func IsBrewManaged() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if pathIsBrewManaged(exe) {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return pathIsBrewManaged(resolved)
	}
	return false
}

// pathIsBrewManaged reports whether an executable path indicates a Homebrew
// install of this formula (Cellar/opt/bin under a brew prefix).
func pathIsBrewManaged(p string) bool {
	if strings.Contains(p, "/Cellar/"+FormulaName+"/") {
		return true
	}
	for _, prefix := range brewPrefixes() {
		if strings.HasPrefix(p, prefix+"/opt/"+FormulaName+"/") ||
			p == filepath.Join(prefix, "bin", FormulaName) {
			return true
		}
	}
	return false
}

// brewPrefixes returns candidate Homebrew prefixes (HOMEBREW_PREFIX if set,
// else the standard arm64 and Intel locations).
func brewPrefixes() []string {
	if v := os.Getenv("HOMEBREW_PREFIX"); v != "" {
		return []string{v}
	}
	return []string{"/opt/homebrew", "/usr/local"}
}
