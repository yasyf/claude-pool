package mountd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// startRawHolder serves scripted raw lines over a short /tmp unix socket. It
// speaks the wire by hand — raw bytes, never the real Server — so the
// client's encoding and decoding are pinned independently of the server
// implementation. respond nil means stall: read the request, then hold the
// connection open without ever replying. respond returning "" means hang up:
// close the connection without replying, like a holder crashing mid-request.
func startRawHolder(t *testing.T, respond func(reqLine string) string) (socket string, requests func() []string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ccp-mountd-cl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = filepath.Join(dir, "m.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	stall := make(chan struct{})
	t.Cleanup(func() {
		close(stall)
		ln.Close()
	})
	var mu sync.Mutex
	var lines []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				mu.Lock()
				lines = append(lines, strings.TrimSuffix(line, "\n"))
				mu.Unlock()
				if respond == nil {
					<-stall
					return
				}
				reply := respond(line)
				if reply == "" {
					return // hang up without replying: a holder crash mid-request
				}
				_, _ = io.WriteString(conn, reply+"\n")
			}(conn)
		}
	}()
	requests = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
	return socket, requests
}

// TestClientRoundTrips pins each method's exact request bytes and its decoding
// of a canned response. The wantReq literals are frozen proto-1 wire artifacts
// (see protocol_test.go).
func TestClientRoundTrips(t *testing.T) {
	tests := []struct {
		name    string
		resp    string
		wantReq string
		invoke  func(t *testing.T, c *Client)
	}{
		{
			name:    "health returns the holder version",
			resp:    `{"proto":1,"ok":true,"version":"v9.8.7 (abc1234)"}`,
			wantReq: `{"proto":1,"op":"health"}`,
			invoke: func(t *testing.T, c *Client) {
				v, err := c.Health()
				if err != nil || v != "v9.8.7 (abc1234)" {
					t.Fatalf("Health = %q, %v; want the holder's version", v, err)
				}
			},
		},
		{
			name:    "health surfaces a classless error verbatim",
			resp:    `{"proto":1,"ok":false,"error":"kaboom"}`,
			wantReq: `{"proto":1,"op":"health"}`,
			invoke: func(t *testing.T, c *Client) {
				if _, err := c.Health(); err == nil || err.Error() != "kaboom" {
					t.Fatalf("Health err = %v, want the holder's message verbatim", err)
				}
			},
		},
		{
			name:    "probe true",
			resp:    `{"proto":1,"ok":true,"fuse_ok":true}`,
			wantReq: `{"proto":1,"op":"probe"}`,
			invoke: func(t *testing.T, c *Client) {
				ok, err := c.Probe()
				if err != nil || !ok {
					t.Fatalf("Probe = %v, %v; want true", ok, err)
				}
			},
		},
		{
			name:    "probe false arrives as an omitted field",
			resp:    `{"proto":1,"ok":true}`,
			wantReq: `{"proto":1,"op":"probe"}`,
			invoke: func(t *testing.T, c *Client) {
				ok, err := c.Probe()
				if err != nil || ok {
					t.Fatalf("Probe = %v, %v; want false", ok, err)
				}
			},
		},
		{
			name:    "mount sends base and dir",
			resp:    `{"proto":1,"ok":true}`,
			wantReq: `{"proto":1,"op":"mount","base":"/pool/base","dir":"/pool/acct-01"}`,
			invoke: func(t *testing.T, c *Client) {
				if err := c.Mount("/pool/base", "/pool/acct-01"); err != nil {
					t.Fatalf("Mount: %v", err)
				}
			},
		},
		{
			name:    "unmount sends base and dir",
			resp:    `{"proto":1,"ok":true}`,
			wantReq: `{"proto":1,"op":"unmount","base":"/pool/base","dir":"/pool/acct-01"}`,
			invoke: func(t *testing.T, c *Client) {
				if err := c.Unmount("/pool/base", "/pool/acct-01"); err != nil {
					t.Fatalf("Unmount: %v", err)
				}
			},
		},
		{
			name:    "list decodes mounts",
			resp:    `{"proto":1,"ok":true,"mounts":[{"dir":"/pool/acct-01","base":"/pool/base","live":true},{"dir":"/pool/acct-02","base":"/pool/base","live":false}]}`,
			wantReq: `{"proto":1,"op":"list"}`,
			invoke: func(t *testing.T, c *Client) {
				mounts, err := c.List()
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				want := []MountInfo{
					{Dir: "/pool/acct-01", Base: "/pool/base", Live: true},
					{Dir: "/pool/acct-02", Base: "/pool/base", Live: false},
				}
				if !reflect.DeepEqual(mounts, want) {
					t.Fatalf("List = %+v, want %+v", mounts, want)
				}
			},
		},
		{
			name:    "list with no mounts is empty",
			resp:    `{"proto":1,"ok":true}`,
			wantReq: `{"proto":1,"op":"list"}`,
			invoke: func(t *testing.T, c *Client) {
				mounts, err := c.List()
				if err != nil || len(mounts) != 0 {
					t.Fatalf("List = %+v, %v; want empty", mounts, err)
				}
			},
		},
		{
			name:    "shutdown clean sweep returns no failed dirs",
			resp:    `{"proto":1,"ok":true}`,
			wantReq: `{"proto":1,"op":"shutdown"}`,
			invoke: func(t *testing.T, c *Client) {
				failed, err := c.Shutdown()
				if err != nil || len(failed) != 0 {
					t.Fatalf("Shutdown = %+v, %v; want clean", failed, err)
				}
			},
		},
		{
			name:    "shutdown reports the dirs that failed to come down",
			resp:    `{"proto":1,"ok":true,"mounts":[{"dir":"/pool/acct-01","base":"/pool/base","live":true}]}`,
			wantReq: `{"proto":1,"op":"shutdown"}`,
			invoke: func(t *testing.T, c *Client) {
				failed, err := c.Shutdown()
				if err != nil {
					t.Fatalf("Shutdown: %v", err)
				}
				want := []MountInfo{{Dir: "/pool/acct-01", Base: "/pool/base", Live: true}}
				if !reflect.DeepEqual(failed, want) {
					t.Fatalf("Shutdown failed dirs = %+v, want %+v", failed, want)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			socket, requests := startRawHolder(t, func(string) string { return tc.resp })
			tc.invoke(t, NewClient(socket))
			got := requests()
			if len(got) != 1 {
				t.Fatalf("holder saw %d requests, want exactly 1: %q", len(got), got)
			}
			if got[0] != tc.wantReq {
				t.Fatalf("request line = %s\nwant         %s", got[0], tc.wantReq)
			}
		})
	}
}

func TestClientErrClassMapping(t *testing.T) {
	const guidance = "fuse mount did not come up: /pool/acct-01 (grant Network Volumes access once in System Settings ▸ Privacy, then retry; symlink is used until then)"
	sentinels := []error{ErrTCCDenied, ErrMountFailed, ErrUnmountWedged, ErrForeignMount, ErrBusy, ErrBaseMismatch, ErrUnknownClass}
	tests := []struct {
		name  string
		class string
		want  error // nil: no sentinel may match
	}{
		{"tcc", ClassTCC, ErrTCCDenied},
		{"mount-failed", ClassMountFailed, ErrMountFailed},
		{"wedged", ClassWedged, ErrUnmountWedged},
		{"foreign-mount", ClassForeignMount, ErrForeignMount},
		{"busy", ClassBusy, ErrBusy},
		{"base-mismatch", ClassBaseMismatch, ErrBaseMismatch},
		// Forward skew: a newer holder's class this client predates must map
		// to ErrUnknownClass — which drivers route to retry, never to
		// conversion — and never to a wrong sentinel or a plain (and thus
		// convertible) error.
		{"unknown class from a newer holder", "quota-exceeded", ErrUnknownClass},
		{"no class at all", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := fmt.Sprintf(`{"proto":1,"ok":false,"error":%q,"err_class":%q}`, guidance, tc.class)
			socket, _ := startRawHolder(t, func(string) string { return resp })

			err := NewClient(socket).Mount("/pool/base", "/pool/acct-01")
			if err == nil {
				t.Fatal("ok=false must surface an error")
			}
			for _, s := range sentinels {
				if got, want := errors.Is(err, s), s == tc.want; got != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v (err: %v)", s, got, want, err)
				}
			}
			if errors.Is(err, ErrHolderUnavailable) {
				t.Errorf("a holder reply must never read as holder-unavailable: %v", err)
			}
			if !strings.Contains(err.Error(), guidance) {
				t.Errorf("holder guidance lost from the error message: %q", err)
			}
			if tc.want != nil && errors.Is(tc.want, ErrUnknownClass) && !strings.Contains(err.Error(), tc.class) {
				t.Errorf("unrecognized class %q lost from the error message: %q", tc.class, err)
			}
		})
	}
}

func TestClientHolderUnavailable(t *testing.T) {
	socket := filepath.Join(shortSockDir(t), "never-bound.sock")
	c := NewClient(socket)
	if c.Available() {
		t.Fatal("Available = true for a socket that was never bound")
	}
	methods := []struct {
		name string
		call func() error
	}{
		{"health", func() error { _, err := c.Health(); return err }},
		{"probe", func() error { _, err := c.Probe(); return err }},
		{"mount", func() error { return c.Mount("/pool/base", "/pool/acct-01") }},
		{"unmount", func() error { return c.Unmount("/pool/base", "/pool/acct-01") }},
		{"list", func() error { _, err := c.List(); return err }},
		{"shutdown", func() error { _, err := c.Shutdown(); return err }},
	}
	for _, tc := range methods {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, ErrHolderUnavailable) {
				t.Fatalf("err = %v, want errors.Is ErrHolderUnavailable", err)
			}
		})
	}
}

// TestClientHolderDiedMidRequest: a holder that accepts, reads the request,
// then hangs up without replying — the shape of a holder crashing inside
// Setup, which is exactly when a fuse-t fault would kill it — leaves the op's
// outcome unknown and must read as ErrHolderUnavailable, never as a classless
// op-level failure a driver could mistake for mount-failed.
func TestClientHolderDiedMidRequest(t *testing.T) {
	socket, requests := startRawHolder(t, func(string) string { return "" })
	err := NewClient(socket).Mount("/pool/base", "/pool/acct-01")
	if !errors.Is(err, ErrHolderUnavailable) {
		t.Fatalf("err = %v, want errors.Is ErrHolderUnavailable", err)
	}
	for _, s := range []error{ErrTCCDenied, ErrMountFailed, ErrUnmountWedged, ErrForeignMount, ErrBusy, ErrBaseMismatch} {
		if errors.Is(err, s) {
			t.Fatalf("outcome-unknown must not carry an op-level class: errors.Is(err, %v) on %v", s, err)
		}
	}
	if got := requests(); len(got) != 1 {
		t.Fatalf("holder saw %d requests, want 1", len(got))
	}
}

// TestClientStalledHolderIsUnavailable: a holder that accepts but never
// replies must surface as ErrHolderUnavailable once the deadline blows — the
// caller cannot tell a wedged holder from a dead one and must not treat the
// timeout as an op-level failure. Exercised through do() with a short timeout
// (the mapping lives there; every method routes through it).
func TestClientStalledHolderIsUnavailable(t *testing.T) {
	socket, requests := startRawHolder(t, nil)
	c := NewClient(socket)
	if !c.Available() {
		t.Fatal("a stalled holder still accepts connections; Available must be true")
	}
	_, err := c.do(Request{Op: OpHealth}, 200*time.Millisecond)
	if !errors.Is(err, ErrHolderUnavailable) {
		t.Fatalf("stalled response err = %v, want errors.Is ErrHolderUnavailable", err)
	}
	if got := requests(); len(got) != 1 {
		t.Fatalf("holder saw %d requests, want 1", len(got))
	}
}

func TestClientWaitGone(t *testing.T) {
	t.Run("true once the socket is dead", func(t *testing.T) {
		socket := filepath.Join(shortSockDir(t), "gone.sock")
		start := time.Now()
		if !NewClient(socket).WaitGone(2 * time.Second) {
			t.Fatal("WaitGone = false for a dead socket")
		}
		if time.Since(start) > time.Second {
			t.Fatal("WaitGone on a dead socket should return on the first failed dial")
		}
	})
	t.Run("false while the holder lives", func(t *testing.T) {
		socket, _ := startRawHolder(t, func(string) string { return `{"proto":1,"ok":true}` })
		if NewClient(socket).WaitGone(400 * time.Millisecond) {
			t.Fatal("WaitGone = true while the socket still accepts")
		}
	})
}

// TestClientWaitGoneContext pins the ctx arm: a done ctx ends the wait on a
// live socket long before the timeout (a daemon shutdown must not stall ~70s
// behind a wedged holder), while kernel truth still wins — a dead socket
// reports gone even under a cancelled ctx.
func TestClientWaitGoneContext(t *testing.T) {
	t.Run("cancel aborts the wait on a live socket", func(t *testing.T) {
		socket, _ := startRawHolder(t, func(string) string { return `{"proto":1,"ok":true}` })
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		if NewClient(socket).WaitGoneContext(ctx, 30*time.Second) {
			t.Fatal("WaitGoneContext = true while the socket still accepts")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("cancelled WaitGoneContext took %v, want a prompt abort", elapsed)
		}
	})
	t.Run("dead socket reads gone even under a done ctx", func(t *testing.T) {
		socket := filepath.Join(shortSockDir(t), "gone.sock")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if !NewClient(socket).WaitGoneContext(ctx, 2*time.Second) {
			t.Fatal("WaitGoneContext = false for a dead socket; kernel truth must win over cancellation")
		}
	})
}
