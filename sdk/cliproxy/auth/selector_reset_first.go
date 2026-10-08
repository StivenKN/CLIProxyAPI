package auth

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetFirstSelector spends the credential whose 5-hour window resets soonest,
// so quota that is about to reset gets used instead of expiring unused. When
// that window is exhausted it moves to the next soonest reset. Credentials with
// no active window come next, and exhausted ones last.
//
// It reads Claude's unified rate-limit watermarks from Auth.Quota.Signals,
// which response headers and the Claude usage poller keep current. Credentials
// without signals keep a deterministic ID order, like fill-first.
type ResetFirstSelector struct {
	// now is overridable in tests.
	now func() time.Time
}

const (
	claudeSignal5hUtilization = "Anthropic-Ratelimit-Unified-5h-Utilization"
	claudeSignal5hReset       = "Anthropic-Ratelimit-Unified-5h-Reset"
	claudeSignal5hStatus      = "Anthropic-Ratelimit-Unified-5h-Status"
	claudeSignal7dUtilization = "Anthropic-Ratelimit-Unified-7d-Utilization"
	claudeSignal7dReset       = "Anthropic-Ratelimit-Unified-7d-Reset"
	claudeSignal7dStatus      = "Anthropic-Ratelimit-Unified-7d-Status"
)

// Ranks order credentials before their reset times are compared.
const (
	resetRankActiveWindow = iota
	resetRankIdle
	resetRankExhausted
)

type quotaWindow struct {
	utilization float64 // fraction used, 0..1
	resetAt     time.Time
	rejected    bool
}

// active reports a window that has started and not yet reset.
func (w quotaWindow) active(now time.Time) bool {
	return w.resetAt.After(now)
}

func (w quotaWindow) exhausted(now time.Time) bool {
	return w.active(now) && (w.rejected || w.utilization >= 1)
}

type resetCandidate struct {
	auth     *Auth
	rank     int
	resetAt  time.Time
	weeklyAt time.Time
}

// Pick returns the highest-priority available credential with the soonest reset.
func (s *ResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	candidates := make([]resetCandidate, 0, len(available))
	for _, auth := range available {
		candidates = append(candidates, rankForReset(auth, now))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if !a.resetAt.Equal(b.resetAt) {
			return earlierKnown(a.resetAt, b.resetAt)
		}
		if !a.weeklyAt.Equal(b.weeklyAt) {
			return earlierKnown(a.weeklyAt, b.weeklyAt)
		}
		return a.auth.ID < b.auth.ID
	})
	return candidates[0].auth, nil
}

func rankForReset(auth *Auth, now time.Time) resetCandidate {
	fiveHour := parseQuotaWindow(auth.Quota.Signals, claudeSignal5hUtilization, claudeSignal5hReset, claudeSignal5hStatus)
	weekly := parseQuotaWindow(auth.Quota.Signals, claudeSignal7dUtilization, claudeSignal7dReset, claudeSignal7dStatus)
	candidate := resetCandidate{auth: auth}
	if weekly.active(now) {
		candidate.weeklyAt = weekly.resetAt
	}
	switch {
	case weekly.exhausted(now) || fiveHour.exhausted(now):
		candidate.rank = resetRankExhausted
		candidate.resetAt = fiveHour.resetAt
		if weekly.exhausted(now) && weekly.resetAt.After(candidate.resetAt) {
			candidate.resetAt = weekly.resetAt
		}
	case fiveHour.active(now):
		candidate.rank = resetRankActiveWindow
		candidate.resetAt = fiveHour.resetAt
	default:
		candidate.rank = resetRankIdle
	}
	return candidate
}

// earlierKnown orders known times ascending and unknown (zero) times last.
func earlierKnown(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	if b.IsZero() {
		return true
	}
	return a.Before(b)
}

func parseQuotaWindow(signals map[string]string, utilizationKey, resetKey, statusKey string) quotaWindow {
	var window quotaWindow
	if len(signals) == 0 {
		return window
	}
	if raw := strings.TrimSpace(signals[utilizationKey]); raw != "" {
		if value, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
			window.utilization = value
		}
	}
	if raw := strings.TrimSpace(signals[resetKey]); raw != "" {
		if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 {
			window.resetAt = time.Unix(seconds, 0)
		}
	}
	window.rejected = strings.EqualFold(strings.TrimSpace(signals[statusKey]), "rejected")
	return window
}

// ObserveQuotaHeaders records an out-of-band quota snapshot, such as a usage
// poll rendered as rate-limit headers, the same way response headers are
// recorded. It only touches observation data, which is never persisted.
func (m *Manager) ObserveQuotaHeaders(ctx context.Context, authID string, headers http.Header) bool {
	if m == nil || strings.TrimSpace(authID) == "" {
		return false
	}
	release, errLock := m.lockAuthMutationContext(ctx, authID)
	if errLock != nil {
		return false
	}
	defer release()
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[authID]
	if auth == nil {
		return false
	}
	return auth.Quota.ObserveResponseHeadersForProvider(auth.Provider, headers, time.Now())
}
