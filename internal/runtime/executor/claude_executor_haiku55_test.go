package executor

import "testing"

func TestClaudeRequestSupportsEffortByHaikuGeneration(t *testing.T) {
	cases := map[string]bool{
		`{"model":"claude-haiku-5-5"}`:          true,
		`{"model":"claude-haiku-4-5-20251001"}`: false,
	}
	for body, want := range cases {
		if got := claudeRequestSupportsEffort([]byte(body), nil); got != want {
			t.Errorf("claudeRequestSupportsEffort(%s) = %v, want %v", body, got, want)
		}
	}
}
