package cliproxy

import (
	"context"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	// quotaPrimeHold keeps a primed credential from being primed again for one
	// full window, even if its usage still reads 0% after the tiny request.
	quotaPrimeHold = 5 * time.Hour
	// quotaPrimeRetryAfter spaces out attempts after a failed prime.
	quotaPrimeRetryAfter = 30 * time.Minute
)

// quotaPrimeRequest is the smallest request that starts a provider's 5-hour window.
type quotaPrimeRequest struct {
	model   string
	format  sdktranslator.Format
	payload string
}

var quotaPrimeRequests = map[string]quotaPrimeRequest{
	"claude": {
		model:   "claude-haiku-4-5-20251001",
		format:  sdktranslator.FormatClaude,
		payload: `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`,
	},
	"codex": {
		model:   "gpt-5.6-luna",
		format:  sdktranslator.FormatOpenAIResponse,
		payload: `{"model":"gpt-5.6-luna","input":"hi","reasoning":{"effort":"low"}}`,
	},
}

// primeQuotaWindows sends a tiny request through every Claude and Codex OAuth
// credential whose 5-hour window is idle, so the window starts now and resets
// sooner. The response headers record the new window, so reset-first ranking
// sees it immediately. primedUntil holds per-credential hold and backoff times
// and is only touched by the poller goroutine.
func (s *Service) primeQuotaWindows(ctx context.Context, primedUntil map[string]time.Time) {
	if s.coreManager == nil {
		return
	}
	for _, auth := range s.coreManager.List() {
		now := time.Now()
		if !shouldPrimeQuotaWindow(auth, now, primedUntil[auth.ID]) {
			continue
		}
		if errPrime := s.primeQuotaWindow(ctx, auth); errPrime != nil {
			if ctx.Err() != nil {
				return
			}
			primedUntil[auth.ID] = now.Add(quotaPrimeRetryAfter)
			log.WithField("auth_id", auth.ID).Debugf("quota window prime failed, retrying in %s: %v", quotaPrimeRetryAfter, errPrime)
			continue
		}
		primedUntil[auth.ID] = now.Add(quotaPrimeHold)
		log.WithField("auth_id", auth.ID).Infof("primed %s 5-hour quota window", auth.Provider)
	}
}

// shouldPrimeQuotaWindow reports whether a credential is a usable Claude or
// Codex OAuth credential, outside its hold/backoff and any cooldown, with an
// idle 5-hour window.
func shouldPrimeQuotaWindow(auth *coreauth.Auth, now, holdUntil time.Time) bool {
	if auth == nil || now.Before(holdUntil) {
		return false
	}
	if _, ok := quotaPrimeRequests[strings.ToLower(auth.Provider)]; !ok {
		return false
	}
	if auth.AuthKind() != coreauth.AuthKindOAuth || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if auth.Unavailable && now.Before(auth.NextRetryAfter) {
		return false
	}
	return coreauth.QuotaWindowIdle(auth, now)
}

func (s *Service) primeQuotaWindow(ctx context.Context, auth *coreauth.Auth) error {
	prime := quotaPrimeRequests[strings.ToLower(auth.Provider)]
	payload := []byte(prime.payload)
	_, errExec := s.coreManager.Execute(ctx, []string{auth.Provider}, cliproxyexecutor.Request{
		Model:   prime.model,
		Payload: payload,
	}, cliproxyexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    prime.format,
		Metadata: map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey:     auth.ID,
			cliproxyexecutor.RequestedModelMetadataKey: prime.model,
		},
	})
	return errExec
}
