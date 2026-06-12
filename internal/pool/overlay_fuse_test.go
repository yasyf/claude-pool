//go:build fuse && cgo && darwin

package pool

import "testing"

// The pure-build counterpart lives in overlay_pure_test.go.
func TestCanHostFuseFuseBuild(t *testing.T) {
	if !CanHostFuse() {
		t.Fatal("CanHostFuse() = false in a fuse build, want true")
	}
}
