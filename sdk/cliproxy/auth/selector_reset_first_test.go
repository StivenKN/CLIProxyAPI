package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func resetFirstAuth(id string, signals map[string]string) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Quota: QuotaState{Signals: signals}}
}

func claudeWindowSignals(window string, utilization float64, resetAt time.Time) map[string]string {
	prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
	return map[string]string{
		prefix + "Utilization": strconv.FormatFloat(utilization, 'f', -1, 64),
		prefix + "Reset":       strconv.FormatInt(resetAt.Unix(), 10),
		prefix + "Status":      "allowed",
	}
}

func mergeSignals(parts ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, part := range parts {
		for key, value := range part {
			merged[key] = value
		}
	}
	return merged
}

func TestResetFirstSelectorOrdering(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	selector := &ResetFirstSelector{now: func() time.Time { return now }}

	// The 5-hour windows point the other way, so only the weekly reset can order these.
	soon := resetFirstAuth("d-soon", mergeSignals(
		claudeWindowSignals("5h", 0.5, now.Add(4*time.Hour)),
		claudeWindowSignals("7d", 0.3, now.Add(24*time.Hour)),
	))
	later := resetFirstAuth("c-later", mergeSignals(
		claudeWindowSignals("5h", 0.5, now.Add(time.Hour)),
		claudeWindowSignals("7d", 0.1, now.Add(96*time.Hour)),
	))
	idle := resetFirstAuth("a-idle", nil)
	expired := resetFirstAuth("b-expired-weekly", claudeWindowSignals("7d", 0.9, now.Add(-time.Minute)))
	exhausted5h := resetFirstAuth("1-exhausted-5h", mergeSignals(
		claudeWindowSignals("5h", 1, now.Add(time.Hour)),
		claudeWindowSignals("7d", 0.2, now.Add(12*time.Hour)),
	))
	exhaustedSoon := resetFirstAuth("3-exhausted-soon", claudeWindowSignals("7d", 1, now.Add(2*time.Hour)))
	exhaustedLater := resetFirstAuth("2-exhausted-later", claudeWindowSignals("7d", 1, now.Add(48*time.Hour)))

	tests := []struct {
		name  string
		auths []*Auth
		want  string
	}{
		{"soonest weekly reset wins over sooner 5h reset", []*Auth{idle, later, soon}, "d-soon"},
		{"started weekly window beats idle", []*Auth{idle, later}, "c-later"},
		{"expired weekly window counts as idle, ID breaks ties", []*Auth{expired, idle}, "a-idle"},
		{"exhausted 5h is ignored", []*Auth{exhausted5h, soon}, "1-exhausted-5h"},
		{"exhausted weekly goes last", []*Auth{exhaustedSoon, later}, "c-later"},
		{"all exhausted picks earliest recovery", []*Auth{exhaustedLater, exhaustedSoon}, "3-exhausted-soon"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, tt.auths)
			if err != nil {
				t.Fatalf("Pick() error = %v", err)
			}
			if got.ID != tt.want {
				t.Fatalf("Pick() = %s, want %s", got.ID, tt.want)
			}
		})
	}
}

func TestResetFirstSelectorReadsCodexWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	selector := &ResetFirstSelector{now: func() time.Time { return now }}
	codexAuth := func(id string, signals map[string]string) *Auth {
		return &Auth{
			ID: id, Provider: "codex", Status: StatusActive,
			Quota: QuotaState{Signals: signals, ObservedAt: now},
		}
	}

	// Codex ranks by its weekly (secondary) window; absolute and relative
	// reset forms both count.
	soon := codexAuth("b-soon", map[string]string{
		"X-Codex-Primary-Used-Percent":          "40",
		"X-Codex-Primary-Reset-At":              strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10),
		"X-Codex-Secondary-Used-Percent":        "40",
		"X-Codex-Secondary-Reset-After-Seconds": "86400",
	})
	later := codexAuth("a-later", map[string]string{
		"X-Codex-Primary-Used-Percent":   "40",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		"X-Codex-Secondary-Used-Percent": "40",
		"X-Codex-Secondary-Reset-At":     strconv.FormatInt(now.Add(72*time.Hour).Unix(), 10),
	})
	weeklyExhausted := codexAuth("0-weekly-exhausted", map[string]string{
		"X-Codex-Primary-Used-Percent":   "10",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(time.Minute).Unix(), 10),
		"X-Codex-Secondary-Used-Percent": "100",
		"X-Codex-Secondary-Reset-At":     strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10),
	})

	got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{weeklyExhausted, later, soon})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b-soon" {
		t.Fatalf("Pick() = %s, want b-soon", got.ID)
	}
}

func TestQuotaWindowIdle(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name    string
		signals map[string]string
		want    bool
	}{
		{"no data", nil, true},
		{"expired window", claudeWindowSignals("5h", 0.5, now.Add(-time.Minute)), true},
		{"started window with no usage", claudeWindowSignals("5h", 0, now.Add(time.Hour)), true},
		{"active window", claudeWindowSignals("5h", 0.01, now.Add(time.Hour)), false},
		{"weekly exhausted", mergeSignals(
			claudeWindowSignals("5h", 0, now.Add(-time.Hour)),
			claudeWindowSignals("7d", 1, now.Add(24*time.Hour)),
		), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := QuotaWindowIdle(resetFirstAuth("a", tt.signals), now); got != tt.want {
				t.Fatalf("QuotaWindowIdle() = %v, want %v", got, tt.want)
			}
		})
	}
}
