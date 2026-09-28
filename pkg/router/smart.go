package router

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/smart"
)

type smartChainKey struct{}

// IsSmartModel reports whether a requested model should be routed by tier.
func (e *Engine) IsSmartModel(model string) bool {
	return e.smart != nil && smart.IsSmartModel(model)
}

// ApplySmart classifies a smart request and rewrites req.Model to the first
// usable target of its tier. The remaining targets are stored in the returned
// context and become the fallback chain for Execute and Stream. Requests for
// other models are returned unchanged with a nil decision.
func (e *Engine) ApplySmart(ctx context.Context, req *canonical.CanonicalRequest) (context.Context, *smart.Decision, error) {
	if !e.IsSmartModel(req.Model) {
		return ctx, nil, nil
	}
	decision := e.smart.Decide(ctx, req)
	targets := e.usableTargets(decision.Targets)
	if len(targets) == 0 {
		return ctx, &decision, fmt.Errorf("smart tier %s has no usable target (configured: %s)", decision.Tier, strings.Join(decision.Targets, ", "))
	}
	decision.Targets = targets
	req.Model = targets[0]

	if tr := trace.FromContext(ctx); tr != nil {
		tr.AddNote(fmt.Sprintf("smart %s: %s", decision.Tier, decision.Reason))
	}
	return context.WithValue(ctx, smartChainKey{}, targets[1:]), &decision, nil
}

func smartChainFromContext(ctx context.Context) ([]string, bool) {
	chain, ok := ctx.Value(smartChainKey{}).([]string)
	return chain, ok
}

// usableTargets keeps resolvable targets, preferring providers that are not
// cooling down. Cooling providers move to the end rather than disappearing,
// so an outage of every provider still gets one real attempt each.
func (e *Engine) usableTargets(targets []string) []string {
	now := time.Now()
	var ready, cooling []string
	for _, target := range targets {
		route, err := e.ResolveModel(target)
		if err != nil {
			continue
		}
		e.mu.RLock()
		until := e.cooldowns[route.Provider.Name()]
		e.mu.RUnlock()
		if now.Before(until) {
			cooling = append(cooling, target)
		} else {
			ready = append(ready, target)
		}
	}
	return append(ready, cooling...)
}

// noteSmartFailure pauses a provider for smart routing after errors that are
// likely to persist for a while: rate limits, exhausted quota, server errors
// and transport failures. Request-shaped errors (4xx) only skip to the next
// target without penalizing the provider.
func (e *Engine) noteSmartFailure(provider string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	status := errorStatus(err)
	if status != 0 && status != 402 && status != 429 && status < 500 {
		log.Printf("[SMART] %s failed with status %d, trying next target", provider, status)
		return
	}
	e.mu.Lock()
	e.cooldowns[provider] = time.Now().Add(e.smartCooldown)
	e.mu.Unlock()
	log.Printf("[SMART] %s failed (%v), pausing it for %s", provider, truncateErr(err), e.smartCooldown)
}

var statusPattern = regexp.MustCompile(`(?:status|error) (\d{3})\b`)

// errorStatus extracts an upstream HTTP status from a provider error, or 0.
func errorStatus(err error) int {
	var apiErr *providers.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	if m := statusPattern.FindStringSubmatch(err.Error()); m != nil {
		status, _ := strconv.Atoi(m[1])
		return status
	}
	return 0
}

func truncateErr(err error) string {
	msg := err.Error()
	if len(msg) > 200 {
		return msg[:200] + "..."
	}
	return msg
}
