package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRefreshRequestAndResponse(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "new-at", RefreshToken: "new-rt", ExpiresIn: 3600,
		})
	}))
	defer srv.Close()

	c := New()
	c.tokenURL = srv.URL
	tr, err := c.Refresh(context.Background(), "k", "old-rt")
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %q", gotBody["grant_type"])
	}
	if gotBody["client_id"] != ClientID {
		t.Errorf("client_id = %q, want %q", gotBody["client_id"], ClientID)
	}
	if gotBody["refresh_token"] != "old-rt" {
		t.Errorf("refresh_token = %q", gotBody["refresh_token"])
	}
	if tr.AccessToken != "new-at" || tr.RefreshToken != "new-rt" {
		t.Errorf("token response = %+v", tr)
	}
	if exp := tr.Expiry(time.Unix(0, 0)); exp != time.Unix(3600, 0) {
		t.Errorf("expiry = %v", exp)
	}
}

func TestRefreshRevoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	c := New()
	c.tokenURL = srv.URL
	_, err := c.Refresh(context.Background(), "k", "rt")
	var re *RefreshError
	if !errors.As(err, &re) || !re.Revoked() {
		t.Fatalf("expected revoked RefreshError, got %v", err)
	}
}

func TestUsageHeadersAndParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer abc" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != betaHeader {
			t.Errorf("anthropic-beta = %q, want %q", got, betaHeader)
		}
		io.WriteString(w, `{
			"five_hour":{"utilization":0.4,"resets_at":1700000000},
			"seven_day":{"utilization":0.1,"resets_at":1700600000},
			"seven_day_opus":{"utilization":0.9,"resets_at":1700600000}
		}`)
	}))
	defer srv.Close()
	c := New()
	c.usageURL = srv.URL
	u, err := c.Usage(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if u.FiveHour.Used() != 40 {
		t.Errorf("five_hour used = %.1f, want 40", u.FiveHour.Used())
	}
	if u.FiveHour.Remaining() != 60 {
		t.Errorf("five_hour remaining = %.1f, want 60", u.FiveHour.Remaining())
	}
	if !u.SevenDayOpus.Present || u.SevenDayOpus.Used() != 90 {
		t.Errorf("opus window = %+v", u.SevenDayOpus)
	}
	if u.FiveHour.ResetsAt.Unix() != 1700000000 {
		t.Errorf("resets_at = %v", u.FiveHour.ResetsAt)
	}
}

func TestUsageUserAgentSent(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		io.WriteString(w, `{"five_hour":{"utilization":0.5,"resets_at":1}}`)
	}))
	defer srv.Close()
	c := New()
	c.usageURL = srv.URL
	if _, err := c.Usage(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ua, "claude-cli/") {
		t.Errorf("User-Agent = %q, want claude-cli/... form", ua)
	}
}

func TestUsageRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := New()
	c.usageURL = srv.URL
	_, err := c.Usage(context.Background(), "x")
	var ue *UsageError
	if !errors.As(err, &ue) || !ue.RateLimited() {
		t.Fatalf("expected rate-limited UsageError, got %v", err)
	}
}

func TestEndpointsAreClaudeDefaults(t *testing.T) {
	c := New()
	if !strings.Contains(c.tokenURL, "platform.claude.com/v1/oauth/token") {
		t.Errorf("token endpoint = %q", c.tokenURL)
	}
	if !strings.Contains(c.usageURL, "api.anthropic.com/api/oauth/usage") {
		t.Errorf("usage endpoint = %q", c.usageURL)
	}
}
