package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/store"
	"github.com/yasyf/cc-pool/internal/version"
)

// reportCall is one captured doctor report line.
type reportCall struct {
	label   string
	healthy bool
	detail  string
}

// captureReports returns a report func and the slice it appends to.
func captureReports() (func(string, bool, string), *[]reportCall) {
	var calls []reportCall
	return func(label string, healthy bool, detail string) {
		calls = append(calls, reportCall{label, healthy, detail})
	}, &calls
}

// TestReportHolder pins the doctor's mount-holder checks over every
// HolderStatus shape: unreachable-with-fuse-rows fails with the respawn hint,
// unreachable-without-rows says nothing, an orphan holder (no fuse rows, no
// daemon) is noted, skew is a note not a failure, a healthy current holder is
// a plain pass, and the daemon's cached TCC/spawn failures each fail loudly.
func TestReportHolder(t *testing.T) {
	cur := version.String()
	cases := map[string]struct {
		facts    holderFacts
		fuseRows int
		want     []reportCall // label + healthy must match; detail is a substring
		none     bool
	}{
		"unreachable with fuse rows fails with the respawn hint": {
			facts:    holderFacts{reachable: false, daemonUp: true},
			fuseRows: 2,
			want: []reportCall{
				{"mount holder", false, "not running with 2 fuse accounts; the daemon respawns it automatically"},
			},
		},
		"unreachable with no fuse rows says nothing": {
			facts:    holderFacts{reachable: false, daemonUp: true},
			fuseRows: 0,
			none:     true,
		},
		"orphan holder with no fuse rows and no daemon is noted": {
			facts:    holderFacts{reachable: true, version: cur, daemonUp: false},
			fuseRows: 0,
			want: []reportCall{
				{"mount holder", true, "orphan (" + cur + ") running with no fuse accounts; `ccp service uninstall` stops it"},
			},
		},
		"version skew is a note, not a failure": {
			facts:    holderFacts{reachable: true, version: "0.0.1-old", daemonUp: true},
			fuseRows: 1,
			want: []reportCall{
				{"mount holder", true, "0.0.1-old (version skew; the daemon replaces it when the pool is idle)"},
			},
		},
		"healthy current holder passes plainly": {
			facts:    holderFacts{reachable: true, version: cur, daemonUp: true},
			fuseRows: 1,
			want:     []reportCall{{"mount holder", true, cur}},
		},
		"cached TCC block fails": {
			facts: holderFacts{
				reachable: true, version: cur, daemonUp: true,
				cached: &daemon.HolderStatus{TCCError: "grant Network Volumes access"},
			},
			fuseRows: 1,
			want: []reportCall{
				{"mount holder", true, cur},
				{"mount holder TCC", false, "grant Network Volumes access"},
			},
		},
		"cached spawn failure fails": {
			facts: holderFacts{
				reachable: false, daemonUp: true,
				cached: &daemon.HolderStatus{SpawnError: "spawn mount holder: exec format error"},
			},
			fuseRows: 1,
			want: []reportCall{
				{"mount holder", false, "the daemon respawns it automatically"},
				{"mount holder spawn", false, "exec format error"},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			report, calls := captureReports()
			reportHolder(tc.facts, tc.fuseRows, report)
			if tc.none {
				if len(*calls) != 0 {
					t.Fatalf("want no reports, got %+v", *calls)
				}
				return
			}
			if len(*calls) != len(tc.want) {
				t.Fatalf("got %d reports %+v, want %d", len(*calls), *calls, len(tc.want))
			}
			for i, want := range tc.want {
				got := (*calls)[i]
				if got.label != want.label || got.healthy != want.healthy || !strings.Contains(got.detail, want.detail) {
					t.Errorf("report[%d] = %+v, want label %q healthy %v detail containing %q", i, got, want.label, want.healthy, want.detail)
				}
			}
		})
	}
}

// TestReportCarcasses pins the dead-mount check: only a fuse row whose dir is
// a mountpoint with ~/.claude not visible through it is flagged; live mounts,
// unmounted dirs, and symlink rows are silent.
func TestReportCarcasses(t *testing.T) {
	accts := []store.Account{
		{ID: 1, ConfigDir: "/p/acct-01", OverlayKind: string(overlay.KindFuse)},    // carcass
		{ID: 2, ConfigDir: "/p/acct-02", OverlayKind: string(overlay.KindFuse)},    // live
		{ID: 3, ConfigDir: "/p/acct-03", OverlayKind: string(overlay.KindFuse)},    // unmounted
		{ID: 4, ConfigDir: "/p/acct-04", OverlayKind: string(overlay.KindSymlink)}, // wrong kind
	}
	swapVar(t, &dirMounted, func(dir string) bool {
		return dir != "/p/acct-03"
	})
	swapVar(t, &mountAliveAt, func(_, dir string) bool {
		return dir == "/p/acct-02"
	})

	report, calls := captureReports()
	reportCarcasses(accts, report)

	if len(*calls) != 1 {
		t.Fatalf("got %d reports %+v, want exactly the carcass", len(*calls), *calls)
	}
	got := (*calls)[0]
	if got.label != "acct-01 mount" || got.healthy || !strings.Contains(got.detail, "dead mount (carcass)") {
		t.Errorf("report = %+v, want acct-01 mount flagged as a dead mount (carcass)", got)
	}
}

// TestCountFuse pins the fuse-row counter both holder checks key on.
func TestCountFuse(t *testing.T) {
	accts := []store.Account{
		{ID: 1, OverlayKind: string(overlay.KindFuse)},
		{ID: 2, OverlayKind: string(overlay.KindSymlink)},
		{ID: 3, OverlayKind: ""}, // legacy rows default to symlink
		{ID: 4, OverlayKind: string(overlay.KindFuse)},
	}
	if got := countFuse(accts); got != 2 {
		t.Errorf("countFuse = %d, want 2", got)
	}
	if got := countFuse(nil); got != 0 {
		t.Errorf("countFuse(nil) = %d, want 0", got)
	}
}
