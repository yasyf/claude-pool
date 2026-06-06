package pool

import (
	"context"
	"errors"
	"testing"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/store"
)

// TestRefreshRefusesAcctZero pins the #1 safety invariant: acct-00's single-use
// refresh token is owned by plain `claude`, so refresh must never POST it. The
// guard sits in refresh (the chokepoint both EnsureFreshToken and fetchUsage's
// 401 retry funnel through) and returns before any OAuth call, so this needs no
// network or Keychain. Revert the guard and this test must fail.
func TestRefreshRefusesAcctZero(t *testing.T) {
	m := &Manager{}
	cred := &keychain.Credential{}
	cred.ClaudeAiOauth.RefreshToken = "rt-must-not-be-used"

	_, err := m.refresh(context.Background(), store.Account{ID: 0, IsZero: true}, cred)
	if !errors.Is(err, ErrRefuseAcctZeroRefresh) {
		t.Fatalf("refresh(acct-00) = %v, want ErrRefuseAcctZeroRefresh", err)
	}
}
