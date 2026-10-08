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

	soon := resetFirstAuth("d-soon", claudeWindowSignals("5h", 0.03, now.Add(2*time.Hour)))
	later := resetFirstAuth("c-later", claudeWindowSignals("5h", 0, now.Add(4*time.Hour)))
	idle := resetFirstAuth("a-idle", nil)
	expired := resetFirstAuth("b-expired-window", claudeWindowSignals("5h", 0.9, now.Add(-time.Minute)))
	exhausted5h := resetFirstAuth("0-exhausted-5h", claudeWindowSignals("5h", 1, now.Add(time.Hour)))
	exhaustedWeekly := resetFirstAuth("1-exhausted-weekly", mergeSignals(
		claudeWindowSignals("5h", 0.1, now.Add(30*time.Minute)),
		claudeWindowSignals("7d", 1, now.Add(48*time.Hour)),
	))

	tests := []struct {
		name  string
		auths []*Auth
		want  string
	}{
		{"soonest 5h reset wins", []*Auth{idle, later, soon}, "d-soon"},
		{"active window beats idle", []*Auth{idle, later}, "c-later"},
		{"expired window counts as idle, ID breaks ties", []*Auth{expired, idle}, "a-idle"},
		{"exhausted 5h goes last", []*Auth{exhausted5h, idle}, "a-idle"},
		{"exhausted weekly goes last despite soon 5h reset", []*Auth{exhaustedWeekly, later}, "c-later"},
		{"all exhausted picks earliest recovery", []*Auth{exhaustedWeekly, exhausted5h}, "0-exhausted-5h"},
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

func TestResetFirstSelectorWeeklyResetBreaksTies(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	selector := &ResetFirstSelector{now: func() time.Time { return now }}
	reset := now.Add(time.Hour)

	noWeekly := resetFirstAuth("a", claudeWindowSignals("5h", 0.5, reset))
	weekly := resetFirstAuth("b", mergeSignals(
		claudeWindowSignals("5h", 0.5, reset),
		claudeWindowSignals("7d", 0.4, now.Add(72*time.Hour)),
	))

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{noWeekly, weekly})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %s, want the credential with a weekly reset", got.ID)
	}
}
