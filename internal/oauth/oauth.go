// Package oauth talks to Anthropic's OAuth token-refresh and usage endpoints
// using the stored subscription credential (never an API key — API keys force
// per-request billing and disable subscription mode).
//
// Endpoints (reverse-engineered from Claude Code v2.1.x):
//
//	refresh: POST https://platform.claude.com/v1/oauth/token
//	usage:   GET  https://api.anthropic.com/api/oauth/usage
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// ClientID is Claude Code's public OAuth client id.
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	tokenEndpoint = "https://platform.claude.com/v1/oauth/token"
	usageEndpoint = "https://api.anthropic.com/api/oauth/usage"

	// betaHeader is required for the usage endpoint.
	betaHeader = "oauth-2025-04-20"
)

// UserAgent matches the Claude Code CLI's own User-Agent format
// (`claude-cli/<version> (external)`, from the binary's Io() builder) so the
// OAuth endpoints treat our polling like the official client. The daemon stamps
// the detected claude version via SetUserAgentVersion.
var UserAgent = "claude-cli/2.1.166 (external)"

// SetUserAgentVersion sets UserAgent to claude-cli/<version> (external).
func SetUserAgentVersion(version string) {
	if version != "" {
		UserAgent = "claude-cli/" + version + " (external)"
	}
}

// Client is a thin OAuth client. The zero value is not usable; use New.
type Client struct {
	http      *http.Client
	tokenURL  string
	usageURL  string
	refreshSF singleflight.Group // de-dupes concurrent refreshes per key
}

// New returns a Client with sane timeouts.
func New() *Client {
	return &Client{
		http:     &http.Client{Timeout: 15 * time.Second},
		tokenURL: tokenEndpoint,
		usageURL: usageEndpoint,
	}
}

// TokenResponse is the refresh endpoint's reply.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"` // may be empty if not rotated
	ExpiresIn    int64  `json:"expires_in"`    // seconds
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// Expiry converts expires_in into an absolute time, from now.
func (t *TokenResponse) Expiry(now time.Time) time.Time {
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// RefreshError carries the HTTP status so callers can distinguish a revoked
// token (4xx -> re-login needed) from a transient failure (5xx/network).
type RefreshError struct {
	Status int
	Body   string
}

func (e *RefreshError) Error() string {
	return fmt.Sprintf("oauth refresh failed: HTTP %d: %s", e.Status, e.Body)
}

// Revoked reports whether the error indicates the refresh token is no longer
// valid (invalid_grant / 400 / 401), meaning the account must be re-logged-in.
func (e *RefreshError) Revoked() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusUnauthorized
}

// Refresh exchanges a refresh token for a fresh access token. Concurrent calls
// sharing flightKey collapse to one in-flight request (single-flight), so the
// daemon never races itself into a refresh-token rotation loop. Pass the
// account id (or any stable per-account key) as flightKey.
func (c *Client) Refresh(ctx context.Context, flightKey, refreshToken string) (*TokenResponse, error) {
	v, err, _ := c.refreshSF.Do(flightKey, func() (any, error) {
		return c.refresh(ctx, refreshToken)
	})
	if err != nil {
		return nil, err
	}
	return v.(*TokenResponse), nil
}

func (c *Client) refresh(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     ClientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, &RefreshError{Status: resp.StatusCode, Body: truncate(string(raw), 300)}
	}
	var tr TokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	return &tr, nil
}

// Window is one usage window (5-hour, 7-day, or 7-day-opus).
type Window struct {
	// Utilization is a fraction in [0,1] (e.g. 0.7 == 70%), as the API reports it.
	Utilization float64
	// ResetsAt is when this window resets. Zero if absent.
	ResetsAt time.Time
	// Present reports whether the API included this window at all.
	Present bool
}

// Used returns utilization as a 0..100 percentage for scoring/display.
func (w Window) Used() float64 { return w.Utilization * 100 }

// Remaining returns 100 - Used, clamped to [0,100].
func (w Window) Remaining() float64 {
	r := 100 - w.Used()
	if r < 0 {
		return 0
	}
	if r > 100 {
		return 100
	}
	return r
}

// Usage is the parsed /api/oauth/usage response.
type Usage struct {
	FiveHour     Window
	SevenDay     Window
	SevenDayOpus Window
}

// rawWindow matches the API JSON: utilization is a fraction in [0,1] and
// resets_at is a Unix epoch in seconds (the binary does utilization*100 and
// new Date(resets_at*1000).toISOString() on the raw response).
type rawWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *float64 `json:"resets_at"` // Unix epoch SECONDS
}

func (rw *rawWindow) toWindow() Window {
	if rw == nil {
		return Window{}
	}
	w := Window{Present: true}
	if rw.Utilization != nil {
		w.Utilization = *rw.Utilization
	}
	if rw.ResetsAt != nil {
		w.ResetsAt = time.Unix(int64(*rw.ResetsAt), 0)
	}
	return w
}

type rawUsage struct {
	FiveHour     *rawWindow `json:"five_hour"`
	SevenDay     *rawWindow `json:"seven_day"`
	SevenDayOpus *rawWindow `json:"seven_day_opus"`
}

// UsageError carries the HTTP status from a failed usage fetch.
type UsageError struct {
	Status int
	Body   string
}

func (e *UsageError) Error() string {
	return fmt.Sprintf("oauth usage failed: HTTP %d: %s", e.Status, e.Body)
}

// RateLimited reports whether the usage fetch itself was rate-limited (429).
func (e *UsageError) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

// Unauthorized reports whether the access token was rejected (401) — the
// caller should refresh and retry.
func (e *UsageError) Unauthorized() bool { return e.Status == http.StatusUnauthorized }

// Usage fetches the current usage windows using a bearer access token.
func (c *Client) Usage(ctx context.Context, accessToken string) (*Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", betaHeader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth usage request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, &UsageError{Status: resp.StatusCode, Body: truncate(string(raw), 300)}
	}
	var ru rawUsage
	if err := json.Unmarshal(raw, &ru); err != nil {
		return nil, fmt.Errorf("decode usage response: %w", err)
	}
	return &Usage{
		FiveHour:     ru.FiveHour.toWindow(),
		SevenDay:     ru.SevenDay.toWindow(),
		SevenDayOpus: ru.SevenDayOpus.toWindow(),
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
