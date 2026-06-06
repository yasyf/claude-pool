package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yasyf/cc-pool/internal/overlay"
)

// SeedOutcome describes what seedClaudeJSON did for an account dir.
type SeedOutcome string

const (
	// SeedCopied: ~/.claude.json was copied in with oauthAccount stripped.
	SeedCopied SeedOutcome = "copied"
	// SeedNoSource: no ~/.claude.json exists; claude onboards fresh (correct —
	// there is nothing to inherit).
	SeedNoSource SeedOutcome = "no-source"
	// SeedKeptExisting: the account already holds logged-in state (a prior add
	// completed its login but was not finalized); it is left untouched.
	SeedKeptExisting SeedOutcome = "kept-existing"
)

// seedClaudeJSON seeds an account's private .claude.json from srcPath (plain
// claude's ~/.claude.json — its primary state file, which claude relocates to
// $CONFIG_DIR/.claude.json when CLAUDE_CONFIG_DIR is set). The copy is
// verbatim except the top-level "oauthAccount" key is stripped: it is the
// per-account identity, and `claude /login` writes the new account's own.
// Everything else (hasCompletedOnboarding, mcpServers, per-project state, …)
// carries over so a pooled session behaves like plain claude instead of
// running the first-run wizard.
//
// The file is written to the provider's private root (never through a fuse
// mount, which may not be up in a CLI process) via temp+rename, so a
// concurrently launched claude never sees a partial file. An existing
// destination is overwritten only when it is a pre-login stub (no
// oauthAccount); logged-in state is kept.
func seedClaudeJSON(prov overlay.Provider, accountDir, srcPath string) (SeedOutcome, error) {
	dst := filepath.Join(prov.PrivateRoot(accountDir), ".claude.json")

	if existing, err := os.ReadFile(dst); err == nil {
		var cur map[string]json.RawMessage
		if json.Unmarshal(existing, &cur) == nil {
			if _, ok := cur["oauthAccount"]; ok {
				return SeedKeptExisting, nil
			}
		}
		// Unparseable or a pre-login onboarding stub: overwrite below.
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read existing %s: %w", dst, err)
	}

	src, err := os.ReadFile(srcPath)
	if os.IsNotExist(err) {
		return SeedNoSource, nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", srcPath, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(src, &top); err != nil {
		return "", fmt.Errorf("parse %s: %w", srcPath, err)
	}
	delete(top, "oauthAccount")
	out, err := json.Marshal(top)
	if err != nil {
		return "", fmt.Errorf("encode seeded config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".claude.json.seed.*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", fmt.Errorf("install seeded config: %w", err)
	}
	return SeedCopied, nil
}
