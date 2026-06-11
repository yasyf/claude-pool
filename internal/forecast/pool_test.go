package forecast

import (
	"math"
	"testing"
	"time"
)

func TestPoolOf(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		accts  []PoolAccount
		want   Pool
		wantOK bool
	}{
		"zero accounts": {nil, Pool{}, false},
		"never sampled pool": {
			[]PoolAccount{{}, {}}, Pool{}, false,
		},
		"all rate-limited is panic with zero remaining": {
			[]PoolAccount{
				{HasUsage: true, RateLimited: true, Remaining5h: 100},
				{HasUsage: true, RateLimited: true, Remaining5h: 100},
			},
			Pool{Mood: MoodPanic}, true,
		},
		"rate-limited account excluded from mean and burn": {
			// The RL account's fabricated remaining 100 and wild burn must
			// not leak into the rollup.
			[]PoolAccount{
				{HasUsage: true, Remaining5h: 80, Remaining7d: 60, BurnPerHour: 10},
				{HasUsage: true, RateLimited: true, Remaining5h: 100, BurnPerHour: 50},
			},
			Pool{Remaining5h: 80, Remaining7d: 60, BurnPerHour: 10,
				DryAt: now.Add(8 * time.Hour), Mood: MoodEasy}, true,
		},
		"stale accounts count toward remaining with zero burn": {
			[]PoolAccount{
				{HasUsage: true, Remaining5h: 50, Remaining7d: 40},
				{HasUsage: true, Remaining5h: 30, Remaining7d: 20},
			},
			Pool{Remaining5h: 40, Remaining7d: 30, Mood: MoodEasy}, true,
		},
		"dry-out projected from combined capacity and burn": {
			// 50 points at 25%/hr = 2h; remaining 50 is easy, bumped to
			// uneasy by the projected dry-out.
			[]PoolAccount{{HasUsage: true, Remaining5h: 50, BurnPerHour: 25}},
			Pool{Remaining5h: 50, BurnPerHour: 25,
				DryAt: now.Add(2 * time.Hour), Mood: MoodUneasy}, true,
		},
		"reset relief before dry-out suppresses it and the bump": {
			[]PoolAccount{{HasUsage: true, Remaining5h: 50, BurnPerHour: 25,
				Resets5h: now.Add(time.Hour)}},
			Pool{Remaining5h: 50, BurnPerHour: 25, Mood: MoodEasy}, true,
		},
		"past reset does not suppress dry-out": {
			[]PoolAccount{{HasUsage: true, Remaining5h: 50, BurnPerHour: 25,
				Resets5h: now.Add(-time.Hour)}},
			Pool{Remaining5h: 50, BurnPerHour: 25,
				DryAt: now.Add(2 * time.Hour), Mood: MoodUneasy}, true,
		},
		"earliest future reset across accounts is the relief": {
			// Combined: 90 points at 30%/hr = 3h dry; the 2h reset lands first.
			[]PoolAccount{
				{HasUsage: true, Remaining5h: 40, BurnPerHour: 20, Resets5h: now.Add(2 * time.Hour)},
				{HasUsage: true, Remaining5h: 50, BurnPerHour: 10, Resets5h: now.Add(4 * time.Hour)},
			},
			Pool{Remaining5h: 45, BurnPerHour: 30, Mood: MoodEasy}, true,
		},
		"remaining clamped before aggregation": {
			[]PoolAccount{
				{HasUsage: true, Remaining5h: -5, Remaining7d: 120},
				{HasUsage: true, Remaining5h: 100, Remaining7d: 100},
			},
			Pool{Remaining5h: 50, Remaining7d: 100, Mood: MoodEasy}, true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := PoolOf(tc.accts, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if math.Abs(got.Remaining5h-tc.want.Remaining5h) > 1e-9 {
				t.Errorf("Remaining5h = %v, want %v", got.Remaining5h, tc.want.Remaining5h)
			}
			if math.Abs(got.Remaining7d-tc.want.Remaining7d) > 1e-9 {
				t.Errorf("Remaining7d = %v, want %v", got.Remaining7d, tc.want.Remaining7d)
			}
			if math.Abs(got.BurnPerHour-tc.want.BurnPerHour) > 1e-9 {
				t.Errorf("BurnPerHour = %v, want %v", got.BurnPerHour, tc.want.BurnPerHour)
			}
			if !got.DryAt.Equal(tc.want.DryAt) {
				t.Errorf("DryAt = %v, want %v", got.DryAt, tc.want.DryAt)
			}
			if got.Mood != tc.want.Mood {
				t.Errorf("Mood = %q, want %q", got.Mood, tc.want.Mood)
			}
		})
	}
}

func TestMoodOf(t *testing.T) {
	cases := map[string]struct {
		usable    int
		remaining float64
		dry       bool
		want      Mood
	}{
		"no usable accounts is panic":  {0, 0, false, MoodPanic},
		"60 is chill":                  {1, 60, false, MoodChill},
		"just below 60 is easy":        {1, 59.9, false, MoodEasy},
		"40 is easy":                   {1, 40, false, MoodEasy},
		"just below 40 is uneasy":      {1, 39.9, false, MoodUneasy},
		"25 is uneasy":                 {1, 25, false, MoodUneasy},
		"just below 25 is worried":     {1, 24.9, false, MoodWorried},
		"10 is worried":                {1, 10, false, MoodWorried},
		"just below 10 is alarmed":     {1, 9.9, false, MoodAlarmed},
		"dry bumps chill to easy":      {1, 80, true, MoodEasy},
		"dry bumps easy to uneasy":     {1, 50, true, MoodUneasy},
		"dry bumps uneasy to worried":  {1, 30, true, MoodWorried},
		"dry bumps worried to alarmed": {1, 15, true, MoodAlarmed},
		"dry bumps alarmed to panic":   {1, 5, true, MoodPanic},
		"panic stays panic under bump": {0, 0, true, MoodPanic},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := moodOf(tc.usable, tc.remaining, tc.dry); got != tc.want {
				t.Errorf("moodOf(%d, %v, %v) = %q, want %q",
					tc.usable, tc.remaining, tc.dry, got, tc.want)
			}
		})
	}
}
