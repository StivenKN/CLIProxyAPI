package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	claudeUsageFirstPoll = 15 * time.Second
	// The loop wakes every claudeUsageTick to prime idle windows, but polls a
	// credential only every claudeUsagePollEvery: Fable weekly resets move once a
	// week, and the usage endpoint has a strict per-credential rate limit
	// (shared with the management panel). Failures retry after
	// claudeUsageRetryAfter.
	claudeUsageTick       = 30 * time.Minute
	claudeUsagePollEvery  = 6 * time.Hour
	claudeUsageRetryAfter = 30 * time.Minute
	// A poll only overwrites a credential's 5-hour and weekly signals when they
	// are older than claudeUsageFreshFor; credentials serving traffic stay
	// fresher from response headers.
	claudeUsageFreshFor = 30 * time.Minute
	// claudeUsageRequestTimeout bounds a background probe so one stalled
	// request cannot stop later polls. No client request waits on it.
	claudeUsageRequestTimeout = 30 * time.Second
)

// claudeUsageWindow is one window of the /api/oauth/usage response.
// Utilization is a percentage (0-100).
type claudeUsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    *string `json:"resets_at"`
}

// claudeUsageLimit is one entry of the response's limits list. The Fable weekly
// limit is kind "weekly_scoped" scoped to the "Fable" model. Percent is 0-100.
type claudeUsageLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	IsActive bool     `json:"is_active"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type claudeUsageResponse struct {
	FiveHour *claudeUsageWindow `json:"five_hour"`
	SevenDay *claudeUsageWindow `json:"seven_day"`
	Limits   []claudeUsageLimit `json:"limits"`
}

// runClaudeUsagePoller keeps the reset-first routing strategy informed. While
// that strategy is active it polls the usage of each Claude OAuth credential
// every claudeUsagePollEvery, recording its Fable weekly window and, for
// credentials not serving traffic, its 5-hour and weekly windows. It then
// primes idle 5-hour windows of Claude and Codex credentials (see
// primeQuotaWindows).
func (s *Service) runClaudeUsagePoller(ctx context.Context) {
	timer := time.NewTimer(claudeUsageFirstPoll)
	defer timer.Stop()
	// nextPollAt holds when each credential is due for a poll, and primedUntil
	// the hold after a prime. Only this goroutine touches them.
	nextPollAt := make(map[string]time.Time)
	primedUntil := make(map[string]time.Time)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if s.resetFirstRoutingActive() {
				s.pollClaudeUsage(ctx, nextPollAt)
				s.primeQuotaWindows(ctx, primedUntil)
			}
			timer.Reset(claudeUsageTick)
		}
	}
}

func (s *Service) resetFirstRoutingActive() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg != nil && normalizedRoutingRuntimeState(s.cfg).strategy == "reset-first"
}

func (s *Service) pollClaudeUsage(ctx context.Context, nextPollAt map[string]time.Time) {
	if s.coreManager == nil {
		return
	}
	for _, auth := range s.coreManager.List() {
		now := time.Now()
		if !isPollableClaudeAuth(auth) || now.Before(nextPollAt[auth.ID]) {
			continue
		}
		usage, errFetch := s.fetchClaudeUsage(ctx, auth)
		if errFetch != nil {
			nextPollAt[auth.ID] = now.Add(claudeUsageRetryAfter)
			log.WithField("auth_id", auth.ID).Debugf("claude usage poll failed, retrying in %s: %v", claudeUsageRetryAfter, errFetch)
			continue
		}
		nextPollAt[auth.ID] = now.Add(claudeUsagePollEvery)
		if utilization, resetAt, ok := claudeFableWeekly(usage); ok {
			coreauth.ObserveClaudeFableWeekly(auth.ID, utilization, resetAt)
		}
		if observedAt := auth.Quota.ObservedAt; observedAt.IsZero() || now.Sub(observedAt) >= claudeUsageFreshFor {
			s.coreManager.ObserveQuotaHeaders(ctx, auth.ID, claudeUsageHeaders(usage))
		}
	}
}

// claudeFableWeekly returns the Fable weekly limit's used fraction and reset
// time, preferring the active entry when several match. ok is false when the
// response has no Fable limit.
func claudeFableWeekly(usage claudeUsageResponse) (utilization float64, resetAt time.Time, ok bool) {
	var fable *claudeUsageLimit
	for i := range usage.Limits {
		limit := &usage.Limits[i]
		if limit.Kind != "weekly_scoped" || limit.Percent == nil || limit.Scope == nil || limit.Scope.Model == nil ||
			!strings.HasPrefix(strings.ToLower(strings.TrimSpace(limit.Scope.Model.DisplayName)), "fable") {
			continue
		}
		if fable == nil || (limit.IsActive && !fable.IsActive) {
			fable = limit
		}
	}
	if fable == nil {
		return 0, time.Time{}, false
	}
	if fable.ResetsAt != nil {
		resetAt, _ = time.Parse(time.RFC3339, strings.TrimSpace(*fable.ResetsAt))
	}
	return *fable.Percent / 100, resetAt, true
}

func isPollableClaudeAuth(auth *coreauth.Auth) bool {
	return auth != nil &&
		strings.EqualFold(auth.Provider, "claude") &&
		auth.AuthKind() == coreauth.AuthKindOAuth &&
		!auth.Disabled &&
		auth.Status != coreauth.StatusDisabled &&
		claudeAccessToken(auth) != ""
}

func claudeAccessToken(auth *coreauth.Auth) string {
	token, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(token)
}

// fetchClaudeUsage reads a credential's usage windows.
func (s *Service) fetchClaudeUsage(ctx context.Context, auth *coreauth.Auth) (claudeUsageResponse, error) {
	var usage claudeUsageResponse
	client := &http.Client{}
	transport, _, errProxy := proxyutil.BuildHTTPTransport(s.antigravityModelFetchProxyURL(auth))
	if errProxy != nil {
		return usage, fmt.Errorf("build transport: %w", errProxy)
	}
	if transport != nil {
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}

	reqCtx, cancel := context.WithTimeout(ctx, claudeUsageRequestTimeout)
	defer cancel()
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, claudeUsageURL, nil)
	if errReq != nil {
		return usage, errReq
	}
	req.Header.Set("Authorization", "Bearer "+claudeAccessToken(auth))
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := client.Do(req)
	if errDo != nil {
		return usage, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("claude usage poll: close body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return usage, errRead
	}
	if resp.StatusCode != http.StatusOK {
		return usage, fmt.Errorf("status %d", resp.StatusCode)
	}
	if errDecode := json.Unmarshal(body, &usage); errDecode != nil {
		return usage, fmt.Errorf("decode usage: %w", errDecode)
	}
	return usage, nil
}

// claudeUsageHeaders renders the 5-hour and weekly windows as the unified
// rate-limit headers Claude returns on messages responses.
func claudeUsageHeaders(usage claudeUsageResponse) http.Header {
	headers := http.Header{}
	setClaudeUsageWindowHeaders(headers, "5h", usage.FiveHour)
	setClaudeUsageWindowHeaders(headers, "7d", usage.SevenDay)
	return headers
}

func setClaudeUsageWindowHeaders(headers http.Header, window string, usage *claudeUsageWindow) {
	if usage == nil || math.IsNaN(usage.Utilization) || math.IsInf(usage.Utilization, 0) {
		return
	}
	prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
	fraction := usage.Utilization / 100
	headers.Set(prefix+"Utilization", strconv.FormatFloat(fraction, 'f', -1, 64))
	status := "allowed"
	if fraction >= 1 {
		status = "rejected"
	}
	headers.Set(prefix+"Status", status)
	if usage.ResetsAt == nil {
		return
	}
	if resetAt, errParse := time.Parse(time.RFC3339, strings.TrimSpace(*usage.ResetsAt)); errParse == nil {
		headers.Set(prefix+"Reset", strconv.FormatInt(resetAt.Unix(), 10))
	}
}
