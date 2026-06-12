//go:build !fuse

package pool

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-pool/internal/overlay"
)

// CanHostFuse is pinned against the build tag itself — not against
// overlay.FuseBuilt(), which would be tautological. The fuse-build
// counterpart lives in overlay_fuse_test.go.
func TestCanHostFusePureBuild(t *testing.T) {
	if CanHostFuse() {
		t.Fatal("CanHostFuse() = true in a pure (non-fuse) build, want false")
	}
}

// TestDetectOverlayKindPureBuild pins the pure-build short-circuit: the
// verdict is symlink with a short actionable reason, decided without touching
// the holder socket — no spawn, no probe, and no adoption of a leftover
// holder this binary could never respawn (the same policy
// SetDefaultOverlayKind enforces). A regression that probes anyway surfaces
// as a different reason (or a fuse verdict) here.
func TestDetectOverlayKindPureBuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	kind, reason := DetectOverlayKind()
	if kind != overlay.KindSymlink {
		t.Fatalf("kind = %q, want symlink in a pure build", kind)
	}
	if !strings.Contains(reason, "cannot host fuse mounts") {
		t.Fatalf("reason = %q, want it to say this build cannot host fuse mounts", reason)
	}
}
