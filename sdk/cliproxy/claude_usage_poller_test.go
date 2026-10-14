package cliproxy

import (
	"encoding/json"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeUsageHeadersMatchRateLimitHeaders(t *testing.T) {
	var usage claudeUsageResponse
	payload := `{"five_hour":{"utilization":3,"resets_at":"2026-10-07T21:39:00.123+00:00"},"seven_day":{"utilization":100,"resets_at":"2026-10-11T08:59:00Z"},"seven_day_opus":null}`
	if errDecode := json.Unmarshal([]byte(payload), &usage); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}

	headers := claudeUsageHeaders(usage)
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.03",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1791409140",
		"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
		"Anthropic-Ratelimit-Unified-7d-Utilization": "1",
		"Anthropic-Ratelimit-Unified-7d-Reset":       "1791709140",
		"Anthropic-Ratelimit-Unified-7d-Status":      "rejected",
	}
	for key, value := range want {
		if got := headers.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestClaudeUsageHeadersIdleWindowHasNoReset(t *testing.T) {
	headers := claudeUsageHeaders(claudeUsageResponse{FiveHour: &claudeUsageWindow{Utilization: 0}})
	if got := headers.Get("Anthropic-Ratelimit-Unified-5h-Reset"); got != "" {
		t.Fatalf("idle window should have no reset, got %q", got)
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-5h-Status"); got != "allowed" {
		t.Fatalf("status = %q, want allowed", got)
	}
}

func TestResetFirstRoutingSelector(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "reset-first"},
	})
	if state.strategy != "reset-first" {
		t.Fatalf("strategy = %q, want reset-first", state.strategy)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.ResetFirstSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.ResetFirstSelector", newRoutingSelector(state))
	}
}

func TestClaudeFableWeekly(t *testing.T) {
	// Trimmed from a real /api/oauth/usage response.
	payload := `{"limits":[
		{"kind":"session","percent":16,"resets_at":"2026-10-14T03:00:00.180025+00:00","scope":null,"is_active":true},
		{"kind":"weekly_all","percent":14,"resets_at":"2026-10-18T14:00:00.180049+00:00","scope":null,"is_active":false},
		{"kind":"weekly_scoped","percent":7,"resets_at":"2026-10-18T14:00:00.180244+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false}
	]}`
	var usage claudeUsageResponse
	if errDecode := json.Unmarshal([]byte(payload), &usage); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	utilization, resetAt, ok := claudeFableWeekly(usage)
	if !ok {
		t.Fatal("no Fable limit found")
	}
	if utilization != 0.07 {
		t.Errorf("utilization = %v, want 0.07", utilization)
	}
	if want := time.Date(2026, 10, 18, 14, 0, 0, 180244000, time.UTC); !resetAt.Equal(want) {
		t.Errorf("resetAt = %v, want %v", resetAt, want)
	}

	if _, _, ok := claudeFableWeekly(claudeUsageResponse{Limits: usage.Limits[:2]}); ok {
		t.Error("found a Fable limit in a response without one")
	}
}

func TestShouldPrimeQuotaWindow(t *testing.T) {
	now := time.Now()
	oauth := func(provider string) *coreauth.Auth {
		return &coreauth.Auth{
			ID: provider, Provider: provider, Status: coreauth.StatusActive,
			Metadata: map[string]any{"access_token": "token"},
		}
	}
	cooling := oauth("claude")
	cooling.Unavailable = true
	cooling.NextRetryAfter = now.Add(time.Minute)
	disabled := oauth("codex")
	disabled.Disabled = true

	tests := []struct {
		name      string
		auth      *coreauth.Auth
		holdUntil time.Time
		want      bool
	}{
		{"idle claude", oauth("claude"), time.Time{}, true},
		{"idle codex", oauth("codex"), time.Time{}, true},
		{"unsupported provider", oauth("gemini"), time.Time{}, false},
		{"held after prime", oauth("claude"), now.Add(time.Hour), false},
		{"cooling down", cooling, time.Time{}, false},
		{"disabled", disabled, time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPrimeQuotaWindow(tt.auth, now, tt.holdUntil); got != tt.want {
				t.Fatalf("shouldPrimeQuotaWindow() = %v, want %v", got, tt.want)
			}
		})
	}
}
