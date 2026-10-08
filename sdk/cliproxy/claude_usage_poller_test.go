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

func TestShouldPollClaudeUsage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	claudeAuth := func(observedAt time.Time) *coreauth.Auth {
		return &coreauth.Auth{
			ID:         "claude-a.json",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth},
			Metadata:   map[string]any{"access_token": "token"},
			Quota:      coreauth.QuotaState{ObservedAt: observedAt},
		}
	}

	tests := []struct {
		name    string
		auth    *coreauth.Auth
		retryAt time.Time
		want    bool
	}{
		{"never observed", claudeAuth(time.Time{}), time.Time{}, true},
		{"fresh from traffic", claudeAuth(now.Add(-5 * time.Minute)), time.Time{}, false},
		{"stale snapshot", claudeAuth(now.Add(-claudeUsageFreshFor)), time.Time{}, true},
		{"backing off after failure", claudeAuth(time.Time{}), now.Add(time.Minute), false},
		{"backoff elapsed", claudeAuth(time.Time{}), now.Add(-time.Second), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPollClaudeUsage(tt.auth, now, tt.retryAt); got != tt.want {
				t.Fatalf("shouldPollClaudeUsage() = %v, want %v", got, tt.want)
			}
		})
	}
}
