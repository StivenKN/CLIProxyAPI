package auth

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetFirstSelector spends the credential whose weekly quota resets soonest,
// so quota that is about to reset gets used instead of expiring unused. For
// Claude that is the Fable weekly limit, for Codex the weekly (secondary)
// window. Credentials whose weekly window hasn't started come next, and
// exhausted ones (5-hour, weekly or Fable) last, so routing moves on to the
// next soonest reset as each credential runs out.
//
// It reads Claude's unified rate-limit watermarks and Codex's primary/secondary
// watermarks from Auth.Quota.Signals, which response headers, the Claude usage
// poller, and window priming keep current, plus the Fable windows the usage
// poller records with ObserveClaudeFableWeekly. Ties fall back to the sooner
// 5-hour reset, then to ID order, like fill-first.
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

	// Codex reports its 5-hour window as "primary" and its weekly window as
	// "secondary": X-Codex-Primary-Used-Percent, X-Codex-Primary-Reset-At, ...
	codexSignalPrimary   = "X-Codex-Primary-"
	codexSignalSecondary = "X-Codex-Secondary-"
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

// claudeFableWeekly holds each Claude credential's Fable weekly window by auth
// ID. Only the usage endpoint reports it, and every response replaces
// Quota.Signals wholesale, so it is kept here instead of in the signals.
var claudeFableWeekly = struct {
	sync.RWMutex
	windows map[string]quotaWindow
}{windows: make(map[string]quotaWindow)}

// ObserveClaudeFableWeekly records a Claude credential's Fable weekly window:
// the fraction used (0..1) and when it resets.
func ObserveClaudeFableWeekly(authID string, utilization float64, resetAt time.Time) {
	claudeFableWeekly.Lock()
	defer claudeFableWeekly.Unlock()
	claudeFableWeekly.windows[authID] = quotaWindow{utilization: utilization, resetAt: resetAt}
}

type resetCandidate struct {
	auth       *Auth
	rank       int
	resetAt    time.Time
	fiveHourAt time.Time
}

// Pick returns the available credential whose weekly quota resets soonest.
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
		if !a.fiveHourAt.Equal(b.fiveHourAt) {
			return earlierKnown(a.fiveHourAt, b.fiveHourAt)
		}
		return a.auth.ID < b.auth.ID
	})
	return candidates[0].auth, nil
}

// rankForReset ranks a credential by its target weekly window (Fable for
// Claude, the weekly window for Codex). Exhausted credentials sort by when
// their last exhausted window resets.
func rankForReset(auth *Auth, now time.Time) resetCandidate {
	fiveHour, weekly := quotaWindows(auth)
	target := weekly
	windows := []quotaWindow{fiveHour, weekly}
	if !strings.EqualFold(auth.Provider, "codex") {
		claudeFableWeekly.RLock()
		target = claudeFableWeekly.windows[auth.ID]
		claudeFableWeekly.RUnlock()
		windows = append(windows, target)
	}
	candidate := resetCandidate{auth: auth, rank: resetRankIdle}
	if fiveHour.active(now) {
		candidate.fiveHourAt = fiveHour.resetAt
	}
	for _, window := range windows {
		if window.exhausted(now) {
			candidate.rank = resetRankExhausted
			if window.resetAt.After(candidate.resetAt) {
				candidate.resetAt = window.resetAt
			}
		}
	}
	if candidate.rank != resetRankExhausted && target.active(now) {
		candidate.rank = resetRankActiveWindow
		candidate.resetAt = target.resetAt
	}
	return candidate
}

// QuotaWindowIdle reports whether a credential's 5-hour window has not started
// (or shows no usage yet) while its weekly window still has room, so a small
// request would start the 5-hour clock.
func QuotaWindowIdle(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	fiveHour, weekly := quotaWindows(auth)
	if weekly.exhausted(now) {
		return false
	}
	return !fiveHour.active(now) || fiveHour.utilization == 0
}

// quotaWindows reads a credential's 5-hour and weekly windows from its quota signals.
func quotaWindows(auth *Auth) (fiveHour, weekly quotaWindow) {
	signals := auth.Quota.Signals
	if strings.EqualFold(auth.Provider, "codex") {
		observedAt := auth.Quota.ObservedAt
		return parseCodexQuotaWindow(signals, codexSignalPrimary, observedAt),
			parseCodexQuotaWindow(signals, codexSignalSecondary, observedAt)
	}
	return parseQuotaWindow(signals, claudeSignal5hUtilization, claudeSignal5hReset, claudeSignal5hStatus),
		parseQuotaWindow(signals, claudeSignal7dUtilization, claudeSignal7dReset, claudeSignal7dStatus)
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

// parseCodexQuotaWindow reads one Codex window. Used percent is 0-100, and the
// reset is either an absolute Unix time or seconds after the observation.
func parseCodexQuotaWindow(signals map[string]string, prefix string, observedAt time.Time) quotaWindow {
	var window quotaWindow
	if len(signals) == 0 {
		return window
	}
	if raw := strings.TrimSpace(signals[prefix+"Used-Percent"]); raw != "" {
		if value, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
			window.utilization = value / 100
		}
	}
	if raw := strings.TrimSpace(signals[prefix+"Reset-At"]); raw != "" {
		if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 {
			window.resetAt = time.Unix(seconds, 0)
		}
	} else if raw := strings.TrimSpace(signals[prefix+"Reset-After-Seconds"]); raw != "" && !observedAt.IsZero() {
		if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 {
			window.resetAt = observedAt.Add(time.Duration(seconds) * time.Second)
		}
	}
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
