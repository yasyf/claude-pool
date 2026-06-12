package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/procscan"
	"github.com/yasyf/cc-pool/internal/store"
)

// defaultMigrateBudget bounds one migrate request's conversion work so the
// response always lands inside handle()'s extended conn deadline (140s) and
// the client's 150s timeout — the server, not a dead socket, must report the
// outcome. Each conversion can take ~16s worst case (8s mount wait, bounded
// rollback teardown), so a big pool can exceed any fixed deadline; accounts
// the budget cannot reach are reported busy and swept by a re-run, which the
// rollout already requires for session-busy accounts.
const defaultMigrateBudget = 120 * time.Second

// handleMigrate converts accounts between overlay providers. Only the daemon
// can do this: it hosts the in-process fuse mounts, and it alone can gate the
// conversion against its own select reservations atomically. Busy accounts
// (live sessions or reservations) are skipped and reported so the client can
// re-run later; per-account failures are rolled back by ConvertOverlay.
func (s *Server) handleMigrate(ctx context.Context, req Request) Response {
	to := overlay.Kind(req.To)
	if to != overlay.KindFuse && to != overlay.KindSymlink {
		return Response{OK: false, Error: fmt.Sprintf("unknown overlay kind %q", req.To)}
	}
	if to == overlay.KindFuse {
		if msg := s.fuseGate(); msg != "" {
			return Response{OK: false, Error: msg}
		}
	}

	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	if req.Account != nil {
		found := false
		for _, a := range accts {
			if a.ID == *req.Account {
				accts = []store.Account{a}
				found = true
				break
			}
		}
		if !found {
			return Response{OK: false, Error: fmt.Sprintf("account %d not found", *req.Account)}
		}
	}

	budget := s.migrateBudget
	if budget <= 0 {
		budget = defaultMigrateBudget
	}
	deadline := time.Now().Add(budget)

	results := make([]MigrationResult, 0, len(accts))
	converted := false
	for _, a := range accts {
		if ctx.Err() != nil {
			break
		}
		if time.Now().After(deadline) {
			results = append(results, MigrationResult{
				ID: a.ID, Label: a.Label, From: a.OverlayKind, To: string(to),
				Outcome: MigrationBusy, Detail: "migrate window elapsed; re-run `ccp migrate`",
			})
			continue
		}
		res := s.convertAccount(a, to, req.Force)
		converted = converted || res.Outcome == MigrationDone
		results = append(results, res)
	}

	resp := Response{OK: true, Migrations: results}
	if converted {
		// The new-account default follows the direction being migrated: the
		// first successful fuse conversion proves this machine mounts, and a
		// deliberate retreat to symlink should stop minting fuse accounts.
		if err := s.m.SetDefaultOverlayKind(to); err != nil {
			resp.OK = false
			resp.Error = fmt.Sprintf("accounts converted, but recording %s as the default for new accounts failed: %v", to, err)
		}
	}
	return resp
}

// fuseGate reports why this daemon cannot host fuse mirrors right now, or ""
// when it can. The probe runs daemon-side deliberately: TCC attributes a
// terminal-spawned CLI to the terminal, so only a probe from this process
// proves the daemon can mount — and a missing "Network Volumes" grant fails
// here, before any account is disturbed.
func (s *Server) fuseGate() string {
	if s.fuseGateFn != nil {
		return s.fuseGateFn()
	}
	if !overlay.FuseBuilt() {
		return "this daemon build has no fuse support; install fuse-t (brew install macos-fuse-t/cask/fuse-t), reinstall cc-pool, and restart the daemon"
	}
	if overlay.Detect() != overlay.KindFuse {
		return "fuse-t probe mount failed — grant cc-pool \"Network Volumes\" access in System Settings ▸ Privacy & Security (the failed attempt creates the toggle), then re-run `ccp migrate`"
	}
	return ""
}

// convertAccount runs one gated conversion, mapping it to a wire outcome.
// force skips the live-session gate only: the user vouches the sessions are
// idle and accepts that one writing mid-conversion may briefly error. The
// claim and reservation gates always hold — those mean another part of the
// daemon owns the dir right now.
func (s *Server) convertAccount(a store.Account, to overlay.Kind, force bool) MigrationResult {
	res := MigrationResult{ID: a.ID, Label: a.Label, From: a.OverlayKind, To: string(to)}
	if a.OverlayKind == string(to) {
		res.Outcome = MigrationAlready
		return res
	}
	if !s.beginConvert(a.ID) {
		res.Outcome = MigrationBusy
		res.Detail = "held by a pending select or a daemon poll; retry shortly"
		return res
	}
	defer s.endConvert(a.ID)

	// The caller's account list is a snapshot that ages while earlier
	// conversions run (seconds each); re-read the row now that the claim
	// makes it stable, so the kind decision and the conversion's writes are
	// against current state, not the snapshot.
	a, err := s.m.Store.GetAccount(a.ID)
	if err != nil {
		res.Outcome = MigrationFailed
		res.Detail = fmt.Sprintf("re-read account row: %v", err)
		return res
	}
	res.Label, res.From = a.Label, a.OverlayKind
	if a.OverlayKind == string(to) {
		res.Outcome = MigrationAlready
		return res
	}

	if !force {
		// Never convert blind: a failed scan means we cannot know whether a
		// live claude has this dir as its config dir.
		sessions, err := procscan.Scan()
		if err != nil {
			res.Outcome = MigrationFailed
			res.Detail = fmt.Sprintf("session scan: %v", err)
			return res
		}
		if n := procscan.CountByConfigDir(sessions, a.ConfigDir); n > 0 {
			res.Outcome = MigrationBusy
			res.Detail = fmt.Sprintf("%d live session(s)", n)
			return res
		}
	}

	if _, err := s.m.ConvertOverlay(a, to); err != nil {
		res.Outcome = MigrationFailed
		res.Detail = err.Error()
		return res
	}
	res.Outcome = MigrationDone
	s.log.Printf("acct-%02d overlay migrated %s -> %s", a.ID, res.From, res.To)
	return res
}
