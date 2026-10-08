package cliproxy

import (
	"encoding/json"
	"testing"

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
