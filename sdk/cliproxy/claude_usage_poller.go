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
	claudeUsageURL          = "https://api.anthropic.com/api/oauth/usage"
	claudeUsagePollInterval = 5 * time.Minute
	claudeUsageFirstPoll    = 15 * time.Second
	// The usage endpoint has a strict per-credential rate limit (shared with the
	// management panel), so a credential is polled only when its quota snapshot
	// is older than claudeUsageFreshFor. Credentials serving traffic stay fresh
	// from response headers and are never polled. Failures wait
	// claudeUsageRetryAfter before the next attempt.
	claudeUsageFreshFor   = 30 * time.Minute
	claudeUsageRetryAfter = 30 * time.Minute
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

type claudeUsageResponse struct {
	FiveHour *claudeUsageWindow `json:"five_hour"`
	SevenDay *claudeUsageWindow `json:"seven_day"`
}

// runClaudeUsagePoller keeps the reset-first routing strategy informed. While
// that strategy is active it polls the usage of Claude OAuth credentials with
// stale quota data, so credentials that have not served a request since
// startup still rank by their real reset times.
func (s *Service) runClaudeUsagePoller(ctx context.Context) {
	timer := time.NewTimer(claudeUsageFirstPoll)
	defer timer.Stop()
	// retryAt holds per-credential backoff after a failed poll. Only this
	// goroutine touches it.
	retryAt := make(map[string]time.Time)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if s.resetFirstRoutingActive() {
				s.pollClaudeUsage(ctx, retryAt)
			}
			timer.Reset(claudeUsagePollInterval)
		}
	}
}

func (s *Service) resetFirstRoutingActive() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg != nil && normalizedRoutingRuntimeState(s.cfg).strategy == "reset-first"
}

func (s *Service) pollClaudeUsage(ctx context.Context, retryAt map[string]time.Time) {
	if s.coreManager == nil {
		return
	}
	for _, auth := range s.coreManager.List() {
		now := time.Now()
		if !shouldPollClaudeUsage(auth, now, retryAt[auth.ID]) {
			continue
		}
		headers, errFetch := s.fetchClaudeUsageHeaders(ctx, auth)
		if errFetch != nil {
			retryAt[auth.ID] = now.Add(claudeUsageRetryAfter)
			log.WithField("auth_id", auth.ID).Debugf("claude usage poll failed, retrying in %s: %v", claudeUsageRetryAfter, errFetch)
			continue
		}
		delete(retryAt, auth.ID)
		s.coreManager.ObserveQuotaHeaders(ctx, auth.ID, headers)
	}
}

// shouldPollClaudeUsage reports whether a credential needs a usage poll: it is
// a usable Claude OAuth credential, is not backing off, and has no quota
// snapshot newer than claudeUsageFreshFor.
func shouldPollClaudeUsage(auth *coreauth.Auth, now, retryAt time.Time) bool {
	if !isPollableClaudeAuth(auth) || now.Before(retryAt) {
		return false
	}
	observedAt := auth.Quota.ObservedAt
	return observedAt.IsZero() || now.Sub(observedAt) >= claudeUsageFreshFor
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

// fetchClaudeUsageHeaders reads a credential's usage and renders it as the
// unified rate-limit headers Claude returns on messages responses.
func (s *Service) fetchClaudeUsageHeaders(ctx context.Context, auth *coreauth.Auth) (http.Header, error) {
	client := &http.Client{}
	transport, _, errProxy := proxyutil.BuildHTTPTransport(s.antigravityModelFetchProxyURL(auth))
	if errProxy != nil {
		return nil, fmt.Errorf("build transport: %w", errProxy)
	}
	if transport != nil {
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}

	reqCtx, cancel := context.WithTimeout(ctx, claudeUsageRequestTimeout)
	defer cancel()
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, claudeUsageURL, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Authorization", "Bearer "+claudeAccessToken(auth))
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("claude usage poll: close body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var usage claudeUsageResponse
	if errDecode := json.Unmarshal(body, &usage); errDecode != nil {
		return nil, fmt.Errorf("decode usage: %w", errDecode)
	}
	return claudeUsageHeaders(usage), nil
}

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
