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

func TestResetFirstSelectorTeamPlansFirst(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	selector := &ResetFirstSelector{now: func() time.Time { return now }}
	withPlan := func(auth *Auth, plan string) *Auth {
		auth.Metadata = map[string]any{"plan_type": plan}
		return auth
	}

	personalSoon := withPlan(resetFirstAuth("a-personal", claudeWindowSignals("5h", 0.2, now.Add(time.Hour))), "max")
	teamLater := withPlan(resetFirstAuth("b-team-later", claudeWindowSignals("5h", 0.2, now.Add(4*time.Hour))), "team")
	teamSoon := withPlan(resetFirstAuth("c-team-soon", claudeWindowSignals("5h", 0.2, now.Add(2*time.Hour))), "team")
	teamIdle := withPlan(resetFirstAuth("d-team-idle", nil), "team")
	teamExhausted := withPlan(resetFirstAuth("e-team-exhausted", claudeWindowSignals("5h", 1, now.Add(time.Hour))), "team")

	tests := []struct {
		name  string
		auths []*Auth
		want  string
	}{
		{"team beats personal with sooner reset", []*Auth{personalSoon, teamLater}, "b-team-later"},
		{"soonest team reset wins", []*Auth{personalSoon, teamLater, teamSoon}, "c-team-soon"},
		{"idle team beats active personal", []*Auth{personalSoon, teamIdle}, "d-team-idle"},
		{"exhausted team goes after personal", []*Auth{teamExhausted, personalSoon}, "a-personal"},
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
	codexAuth := func(id, plan string, signals map[string]string) *Auth {
		return &Auth{
			ID: id, Provider: "codex", Status: StatusActive,
			Attributes: map[string]string{"plan_type": plan},
			Quota:      QuotaState{Signals: signals, ObservedAt: now},
		}
	}

	// Absolute and relative reset forms both count.
	soon := codexAuth("b-soon", "pro", map[string]string{
		"X-Codex-Primary-Used-Percent":        "40",
		"X-Codex-Primary-Reset-After-Seconds": "600",
	})
	later := codexAuth("a-later", "pro", map[string]string{
		"X-Codex-Primary-Used-Percent": "40",
		"X-Codex-Primary-Reset-At":     strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10),
	})
	weeklyExhausted := codexAuth("0-weekly-exhausted", "team", map[string]string{
		"X-Codex-Primary-Used-Percent":   "10",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(time.Minute).Unix(), 10),
		"X-Codex-Secondary-Used-Percent": "100",
		"X-Codex-Secondary-Reset-At":     strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10),
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
