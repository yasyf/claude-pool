package keychain

import (
	"os"
	"path/filepath"
	"testing"
)

// TestServiceNameGoldenVectors pins the derivation against an independent
// oracle (values computed via `shasum -a 256` of the NFC path, first 8 hex).
func TestServiceNameGoldenVectors(t *testing.T) {
	cases := map[string]string{
		"/Users/yasyf/.claude.pool/acct-01": "Claude Code-credentials-ed0d2df9",
		"/Users/yasyf/.claude":              "Claude Code-credentials-c25ff9d8",
	}
	for dir, want := range cases {
		if got := ServiceName(dir); got != want {
			t.Errorf("ServiceName(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestDefaultServiceName(t *testing.T) {
	if got := DefaultServiceName(); got != "Claude Code-credentials" {
		t.Errorf("DefaultServiceName() = %q", got)
	}
}

func TestAccountLabelFromEnv(t *testing.T) {
	t.Setenv("USER", "valid.user-1")
	if got := AccountLabel(); got != "valid.user-1" {
		t.Errorf("AccountLabel() = %q, want valid.user-1", got)
	}
	t.Setenv("USER", "bad user!")
	if got := AccountLabel(); got != fallbackAccount {
		t.Errorf("AccountLabel() for invalid = %q, want %q", got, fallbackAccount)
	}
}

// TestSecurityRoundTrip drives the real wrapper against a fake `security`
// binary that emulates add/find/delete, proving the exact argv contract and the
// -X hex round-trip.
func TestSecurityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "items")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeSecurity(t, dir, storeDir)

	old := securityBin
	securityBin = fake
	t.Cleanup(func() { securityBin = old })

	const svc = "Claude Code-credentials-deadbeef"
	const acct = "tester"

	if _, err := Read(svc, acct); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound before write, got %v", err)
	}

	cred := &Credential{}
	cred.ClaudeAiOauth.AccessToken = "at-123"
	cred.ClaudeAiOauth.RefreshToken = "rt-456"
	cred.ClaudeAiOauth.ExpiresAt = 1700000000000
	cred.ClaudeAiOauth.SubscriptionType = "max"
	cred.ClaudeAiOauth.Scopes = []string{"user:inference"}

	if err := Write(svc, acct, cred); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !Exists(svc, acct) {
		t.Fatal("Exists should be true after Write")
	}
	got, err := Read(svc, acct)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.ClaudeAiOauth.AccessToken != "at-123" || got.ClaudeAiOauth.RefreshToken != "rt-456" {
		t.Fatalf("round-trip mismatch: %+v", got.ClaudeAiOauth)
	}
	if got.ClaudeAiOauth.SubscriptionType != "max" {
		t.Fatalf("subscriptionType not preserved: %q", got.ClaudeAiOauth.SubscriptionType)
	}
	if acctGot, err := DiscoverAccount(svc); err != nil || acctGot != acct {
		t.Fatalf("DiscoverAccount = %q, %v; want %q", acctGot, err, acct)
	}
	if err := Delete(svc, acct); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if Exists(svc, acct) {
		t.Fatal("Exists should be false after Delete")
	}
	if err := Delete(svc, acct); err != nil {
		t.Fatalf("Delete of missing should be nil, got %v", err)
	}
}

// writeFakeSecurity writes a shell script emulating /usr/bin/security using a
// directory of files keyed by service+account.
func writeFakeSecurity(t *testing.T, dir, storeDir string) string {
	t.Helper()
	script := `#!/bin/bash
set -e
STORE="` + storeDir + `"
op="$1"; shift
acct=""; svc=""; pw=""; want_pw=0
while [ $# -gt 0 ]; do
  case "$1" in
    -a) acct="$2"; shift 2;;
    -s) svc="$2"; shift 2;;
    -X) pw="$2"; shift 2;;
    -w) want_pw=1; shift;;
    -U) shift;;
    *) shift;;
  esac
done
key=$(printf '%s' "${svc}::${acct}" | tr '/ :' '___')
keysvc=$(printf '%s' "${svc}" | tr '/ :' '___')
case "$op" in
  add-generic-password)
    printf '%s' "$pw" > "$STORE/$key.hex"
    printf '%s' "$acct" > "$STORE/$key.acct"
    printf '%s' "$svc" > "$STORE/$keysvc.lastacct.acct" 2>/dev/null || true
    # also index by service for -s-only lookups
    printf '%s' "$acct" > "$STORE/svc_$keysvc.acct"
    exit 0;;
  find-generic-password)
    f=""
    if [ -n "$acct" ]; then f="$STORE/$key"; else
      # service-only: find any acct for this service
      a=$(cat "$STORE/svc_$keysvc.acct" 2>/dev/null || true)
      f="$STORE/$(printf '%s' "${svc}::${a}" | tr '/ :' '___')"
    fi
    if [ ! -f "$f.hex" ]; then
      echo "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain." >&2
      exit 44
    fi
    if [ "$want_pw" = "1" ]; then
      # decode hex to raw bytes
      cat "$f.hex" | xxd -r -p
      exit 0
    fi
    a=$(cat "$f.acct")
    echo "keychain: \"login.keychain-db\""
    echo "    \"acct\"<blob>=\"$a\""
    echo "    \"svce\"<blob>=\"$svc\""
    exit 0;;
  delete-generic-password)
    if [ ! -f "$STORE/$key.hex" ]; then
      echo "security: The specified item could not be found in the keychain." >&2
      exit 44
    fi
    rm -f "$STORE/$key.hex" "$STORE/$key.acct" "$STORE/svc_$keysvc.acct"
    exit 0;;
esac
exit 1
`
	path := filepath.Join(dir, "security")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
