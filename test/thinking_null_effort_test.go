package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// TestThinkingNullOrEmptyEffortIsAbsent checks that a JSON null or empty
// reasoning effort/level is treated like an absent field instead of being
// rejected as an unsupported level.
func TestThinkingNullOrEmptyEffortIsAbsent(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	uid := fmt.Sprintf("thinking-null-effort-%d", time.Now().UnixNano())
	reg.RegisterClient(uid, "test", getTestModels())
	defer reg.UnregisterClient(uid)

	cases := []struct {
		name        string
		from        string
		to          string
		model       string
		input       string
		expectField string
		expectValue string
	}{
		{
			name:        "openai chat null effort to codex defaults to medium",
			from:        "openai",
			to:          "codex",
			model:       "level-model",
			input:       `{"model":"level-model","reasoning_effort":null,"messages":[{"role":"user","content":"hi"}]}`,
			expectField: "reasoning.effort",
			expectValue: "medium",
		},
		{
			name:        "openai chat empty effort to codex defaults to medium",
			from:        "openai",
			to:          "codex",
			model:       "level-model",
			input:       `{"model":"level-model","reasoning_effort":"","messages":[{"role":"user","content":"hi"}]}`,
			expectField: "reasoning.effort",
			expectValue: "medium",
		},
		{
			name:  "openai chat null effort to openai",
			from:  "openai",
			to:    "openai",
			model: "level-model",
			input: `{"model":"level-model","reasoning_effort":null,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:  "responses null effort to codex",
			from:  "openai-response",
			to:    "codex",
			model: "level-model",
			input: `{"model":"level-model","reasoning":{"effort":null},"input":"hi"}`,
		},
		{
			name:  "responses blank effort to codex",
			from:  "openai-response",
			to:    "codex",
			model: "level-model",
			input: `{"model":"level-model","reasoning":{"effort":"  "},"input":"hi"}`,
		},
		{
			name:  "gemini null thinkingLevel to gemini",
			from:  "gemini",
			to:    "gemini",
			model: "level-subset-model",
			input: `{"model":"level-subset-model","contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":null}}}`,
		},
		{
			name:        "gemini null thinkingLevel falls back to thinkingBudget",
			from:        "gemini",
			to:          "gemini",
			model:       "gemini-budget-model",
			input:       `{"model":"gemini-budget-model","contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":null,"thinkingBudget":4096}}}`,
			expectField: "generationConfig.thinkingConfig.thinkingBudget",
			expectValue: "4096",
		},
		{
			name:  "interactions null thinking_level to interactions",
			from:  "interactions",
			to:    "interactions",
			model: "level-model",
			input: `{"model":"level-model","input":"hi","generation_config":{"thinking_level":null}}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := sdktranslator.TranslateRequest(
				sdktranslator.FromString(tc.from),
				sdktranslator.FromString(tc.to),
				tc.model,
				[]byte(tc.input),
				true,
			)
			out, err := thinking.ApplyThinking(body, tc.model, tc.from, tc.to, tc.to)
			if err != nil {
				t.Fatalf("unexpected error: %v, body=%s", err, string(body))
			}
			if tc.expectField == "" {
				return
			}
			got := ojson.GetBytesNoCopy(out, tc.expectField)
			value := got.String()
			if got.Type == ojson.Number {
				value = fmt.Sprintf("%d", got.Int())
			}
			if value != tc.expectValue {
				t.Fatalf("%s = %q, want %q, body=%s", tc.expectField, value, tc.expectValue, string(out))
			}
		})
	}
}
