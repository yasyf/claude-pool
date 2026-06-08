package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/pool"
	"golang.org/x/term"
)

const (
	// loginPollInterval is how often the suffixed Keychain item is probed
	// while a login is in flight.
	loginPollInterval = 500 * time.Millisecond
	// identityGrace bounds the pre-kill wait for claude to write oauthAccount
	// into the account's .claude.json. Kept short: claude may only write it at
	// exit, in which case the post-exit re-poll catches it.
	identityGrace = time.Second
	// identityPostExitGrace bounds the post-exit re-poll for exit-time writes.
	identityPostExitGrace = 2 * time.Second
	// killGrace is how long a SIGTERMed claude gets before SIGKILL.
	killGrace = 3 * time.Second
)

// awaitOutcome is how a watched login ended.
type awaitOutcome int

const (
	awaitCred     awaitOutcome = iota // the credential landed; claude still running
	awaitExited                       // the process exited first (user quit claude)
	awaitCanceled                     // the wait was aborted: context canceled or probe failure
)

// awaitLogin polls probe every interval until it reports done, the process
// exits, or ctx is canceled. probe errors abort the wait — a broken probe must
// not become a silent infinite retry loop. For awaitExited the process's exit
// error (possibly nil) is passed through.
func awaitLogin(ctx context.Context, procExit <-chan error, probe func() (bool, error), interval time.Duration) (awaitOutcome, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case err := <-procExit:
			return awaitExited, err
		case <-ctx.Done():
			return awaitCanceled, ctx.Err()
		case <-ticker.C:
			done, err := probe()
			if err != nil {
				return awaitCanceled, err
			}
			if done {
				return awaitCred, nil
			}
		}
	}
}

// discoverFunc matches keychain.DiscoverAccount; injectable for tests.
type discoverFunc func(service string) (string, error)

// newCredProbe returns a probe reporting the suffixed item's appearance. It is
// only used when no item exists at watch start (PrepareAdd purges stale ones):
// secret CHANGE is deliberately not a signal — claude rewrites a pre-existing
// item for non-login reasons (startup token refresh, dead-token clearing), so
// a pre-existing item disables watching entirely (see runWatchedLogin). Probe
// errors propagate; a broken keychain must not become a silent retry loop.
func newCredProbe(discover discoverFunc, service string) func() (bool, error) {
	return func() (bool, error) {
		_, err := discover(service)
		if errors.Is(err, keychain.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
}

// runWatchedLogin runs `claude /login` attached to the terminal and watches
// for the credential to land; when it does, it closes claude for the user. The
// child stays in our foreground process group (a background pgrp touching the
// tty would be stopped with SIGTTIN/SIGTTOU), so termination signals target
// c.Process directly and the terminal is only restored after the child has
// exited.
//
// When a credential already exists at start (the kept-existing reuse path),
// completion cannot be detected — claude rewrites the item at startup for
// non-login reasons — so the session runs unwatched and the user exits claude
// themselves, exactly the pre-watcher behavior.
func runWatchedLogin(ctx context.Context, cmd *cobra.Command, p *pool.PendingAdd) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("`claude` not found on PATH: %w", err)
	}
	watch := false
	switch _, err := keychain.DiscoverAccount(p.KeychainService); {
	case errors.Is(err, keychain.ErrNotFound):
		watch = true
	case err != nil:
		return fmt.Errorf("probe credential for %s: %w", p.ConfigDir, err)
	default:
		note(cmd.OutOrStdout(), "Found an existing login. Exit claude when done; it's reused unless you log in again.")
	}

	fd := int(os.Stdin.Fd())
	state, _ := term.GetState(fd) // nil on non-TTY; restore is nil-safe

	c := exec.Command(bin, "/login")
	c.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+p.ConfigDir)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return fmt.Errorf("start claude /login: %w", err)
	}
	procExit := make(chan error, 1)
	go func() { procExit <- c.Wait() }()

	if !watch {
		werr := <-procExit
		restoreTerminal(cmd.OutOrStdout(), fd, state)
		return werr
	}

	probe := newCredProbe(keychain.DiscoverAccount, p.KeychainService)
	outcome, werr := awaitLogin(ctx, procExit, probe, loginPollInterval)
	switch outcome {
	case awaitExited:
		// The user closed claude themselves; finalize proceeds as before.
		restoreTerminal(cmd.OutOrStdout(), fd, state)
		return werr
	case awaitCanceled:
		terminate(c, procExit)
		restoreTerminal(cmd.OutOrStdout(), fd, state)
		return werr
	}

	// Credential landed. Give claude a moment to write the account identity
	// (label prefill + duplicate-login detection source), then close it.
	waitForIdentity(ctx, p.OverlayKind, p.ConfigDir, identityGrace)
	terminate(c, procExit)
	restoreTerminal(cmd.OutOrStdout(), fd, state)
	if err := ctx.Err(); err != nil {
		// Canceled while closing claude: the login itself landed, but the add
		// must stop here, not march on into prompts and finalize.
		return err
	}
	if !waitForIdentity(ctx, p.OverlayKind, p.ConfigDir, identityPostExitGrace) {
		warn(cmd.ErrOrStderr(), "couldn't read this account's email, so its name won't prefill; if this add doesn't finish, retrying will discard this login")
	}
	success(cmd.OutOrStdout(), "Logged in. Closed claude.")
	return nil
}

// terminate closes the child: SIGTERM, a grace period, then SIGKILL; always
// drains procExit so the Wait goroutine finishes.
func terminate(c *exec.Cmd, procExit <-chan error) {
	_ = c.Process.Signal(syscall.SIGTERM)
	select {
	case <-procExit:
		return
	case <-time.After(killGrace):
	}
	_ = c.Process.Kill()
	<-procExit
}

// restoreTerminal undoes whatever raw-mode state a (possibly SIGKILLed) claude
// left behind: termios via the saved state, then explicit escape sequences to
// leave the alternate screen, show the cursor, disable bracketed paste, mouse
// and focus-event reporting, pop the kitty keyboard protocol, and reset SGR.
// Escapes are only emitted on a real terminal.
func restoreTerminal(out io.Writer, fd int, state *term.State) {
	if state != nil {
		_ = term.Restore(fd, state)
	}
	if isTTY() {
		fmt.Fprint(out, "\x1b[?1049l\x1b[?25h\x1b[?2004l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1006l\x1b[<u\x1b[0m")
	}
}

// waitForIdentity polls for the account's .claude.json identity for up to
// grace, returning whether it appeared. Best-effort: the identity only feeds
// label prefill and duplicate-login detection.
func waitForIdentity(ctx context.Context, kind overlay.Kind, configDir string, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if _, err := pool.AccountIdentity(kind, configDir); err == nil {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitForCredential polls until the (suffixed) item for service appears,
// showing a one-line spinner. Used by the manual login path; unbounded — the
// user may take arbitrarily long in another terminal, and ^C cancels.
func waitForCredential(ctx context.Context, out io.Writer, service string) error {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	ticker := time.NewTicker(loginPollInterval)
	defer ticker.Stop()
	for i := 0; ; i++ {
		fmt.Fprintf(out, "\r%s %s", frames[i%len(frames)], dimStyle.Render("waiting for login… press ctrl-c to abort"))
		select {
		case <-ctx.Done():
			fmt.Fprint(out, "\r\x1b[K")
			return ctx.Err()
		case <-ticker.C:
			_, err := keychain.DiscoverAccount(service)
			if errors.Is(err, keychain.ErrNotFound) {
				continue
			}
			fmt.Fprint(out, "\r\x1b[K")
			if err != nil {
				return err
			}
			success(out, "Logged in.")
			return nil
		}
	}
}
