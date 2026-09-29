// Package smart implements a virtual provider that classifies each request
// into a complexity tier and dispatches it to that tier's ordered targets.
package smart

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/classifier"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/server/trace"
)

const DefaultModel = "smart-router"

// Resolver maps a "provider/model" target to a live provider.
type Resolver func(target string) (providers.Provider, string, error)

// Decision is a record of one routing choice, kept for the dashboard.
type Decision struct {
	Time     time.Time         `json:"time"`
	Session  string            `json:"session,omitempty"`
	Result   classifier.Result `json:"result"`
	Tier     classifier.Tier   `json:"tier"`
	Target   string            `json:"target"`
	Pinned   bool              `json:"pinned"`
	Skipped  []string          `json:"skipped,omitempty"`
	Ask      string            `json:"ask,omitempty"`
	ErrorMsg string            `json:"error,omitempty"`
}

type pin struct {
	tier    classifier.Tier
	expires time.Time
}

type health struct {
	fails         int
	cooldownUntil time.Time
}

type Router struct {
	model        string
	classifier   classifier.Classifier
	tiers        map[classifier.Tier][]string
	resolve      Resolver
	affinityTTL  time.Duration
	allowedFails int
	cooldown     time.Duration
	now          func() time.Time

	mu        sync.Mutex
	pins      map[string]pin
	health    map[string]*health
	decisions []Decision
}

func BuildClassifier(cfg config.SmartConfig) classifier.Classifier {
	var chain []classifier.Classifier
	rules := make([]classifier.KeywordRule, 0, len(cfg.Keywords))
	for _, k := range cfg.Keywords {
		rules = append(rules, classifier.KeywordRule{Keywords: k.Keywords, Tier: k.Tier})
	}
	if len(rules) == 0 {
		rules = classifier.DefaultKeywordRules()
	}
	chain = append(chain, &classifier.Keywords{Rules: rules})

	c := cfg.Classifier
	timeout := time.Duration(c.TimeoutMs) * time.Millisecond
	switch strings.ToLower(c.Type) {
	case "laya":
		chain = append(chain, classifier.NewLaya(c.URL, c.APIKey, c.Model, timeout, c.MinProb, c.MaxChars, c.CacheSize))
	case "auto", "":
		if c.URL != "" && !strings.HasPrefix(c.URL, "${") {
			chain = append(chain, classifier.NewLaya(c.URL, c.APIKey, c.Model, timeout, c.MinProb, c.MaxChars, c.CacheSize))
		}
	case "http":
		chain = append(chain, classifier.NewHTTP(c.URL, c.APIKey, timeout, c.MinProb))
	}
	chain = append(chain, &classifier.Heuristic{})

	def, _ := classifier.ParseTier(cfg.DefaultTier)
	minTier, _ := classifier.ParseTier(cfg.MinTier)
	return &classifier.Chain{Classifiers: chain, Default: def, MinTier: minTier}
}

func New(cfg config.SmartConfig, resolve Resolver) *Router {
	r := &Router{
		model:        cfg.Model,
		classifier:   BuildClassifier(cfg),
		tiers:        make(map[classifier.Tier][]string),
		resolve:      resolve,
		affinityTTL:  time.Duration(cfg.SessionAffinityTTLSeconds) * time.Second,
		allowedFails: cfg.AllowedFails,
		cooldown:     time.Duration(cfg.CooldownSeconds) * time.Second,
		now:          time.Now,
		pins:         make(map[string]pin),
		health:       make(map[string]*health),
	}
	if r.model == "" {
		r.model = DefaultModel
	}
	if cfg.SessionAffinityTTLSeconds == 0 {
		r.affinityTTL = time.Hour
	}
	if r.allowedFails <= 0 {
		r.allowedFails = 2
	}
	if r.cooldown <= 0 {
		r.cooldown = 5 * time.Minute
	}
	for name, targets := range cfg.Tiers {
		if tier, ok := classifier.ParseTier(name); ok {
			r.tiers[tier] = append([]string(nil), targets...)
		}
	}
	return r
}

func (r *Router) Model() string { return r.model }

func (r *Router) Name() string     { return r.model }
func (r *Router) Type() string     { return "smart" }
func (r *Router) Endpoint() string { return "smart://" + r.model }

func (r *Router) ListModels(context.Context) ([]providers.ModelInfo, error) {
	return []providers.ModelInfo{{ID: r.model, Name: "Smart Router (auto)", Provider: r.model, Description: "Routes by request complexity"}}, nil
}

// candidates returns the ordered targets for tier, then the targets of every
// higher tier, then lower tiers, so a request is never dropped while any
// target is healthy.
func (r *Router) candidates(tier classifier.Tier) []string {
	idx := tier.Index()
	var order []classifier.Tier
	for i := idx; i < len(classifier.Tiers); i++ {
		order = append(order, classifier.Tiers[i])
	}
	for i := idx - 1; i >= 0; i-- {
		order = append(order, classifier.Tiers[i])
	}
	seen := make(map[string]bool)
	var out []string
	for _, t := range order {
		for _, target := range r.tiers[t] {
			if !seen[target] {
				seen[target] = true
				out = append(out, target)
			}
		}
	}
	return out
}

func (r *Router) decide(ctx context.Context, req *canonical.CanonicalRequest) (classifier.Result, classifier.Tier, bool, string) {
	ask := classifier.ExtractAsk(req)
	res, _ := r.classifier.Classify(ctx, ask)
	tier := res.Tier
	pinned := false
	if req.SessionID != "" && r.affinityTTL > 0 {
		now := r.now()
		r.mu.Lock()
		if p, ok := r.pins[req.SessionID]; ok && now.Before(p.expires) && p.tier.Index() > tier.Index() {
			tier, pinned = p.tier, true
		}
		r.pins[req.SessionID] = pin{tier: tier, expires: now.Add(r.affinityTTL)}
		if len(r.pins) > 10000 {
			for k, v := range r.pins {
				if now.After(v.expires) {
					delete(r.pins, k)
				}
			}
		}
		r.mu.Unlock()
	}
	return res, tier, pinned, ask
}

func (r *Router) available(target string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.health[target]
	return !ok || !r.now().Before(h.cooldownUntil)
}

func (r *Router) report(target string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.health[target]
	if !ok {
		h = &health{}
		r.health[target] = h
	}
	if err == nil {
		h.fails = 0
		return
	}
	h.fails++
	if h.fails >= r.allowedFails {
		h.cooldownUntil = r.now().Add(r.cooldown)
		h.fails = 0
		log.Printf("[SMART] %s cooling down for %s after error: %v", target, r.cooldown, err)
	}
}

func (r *Router) record(d Decision) {
	if len(d.Ask) > 160 {
		d.Ask = d.Ask[:160]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, d)
	if len(r.decisions) > 200 {
		r.decisions = r.decisions[len(r.decisions)-200:]
	}
}

// Decisions returns the most recent routing decisions, newest last.
func (r *Router) Decisions() []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Decision(nil), r.decisions...)
}

func dispatch[T any](r *Router, ctx context.Context, req *canonical.CanonicalRequest, call func(providers.Provider, *canonical.CanonicalRequest) (T, error)) (T, error) {
	var zero T
	res, tier, pinned, ask := r.decide(ctx, req)
	d := Decision{Time: r.now(), Session: req.SessionID, Result: res, Tier: tier, Pinned: pinned, Ask: ask}

	targets := r.candidates(tier)
	if len(targets) == 0 {
		err := fmt.Errorf("smart router: no targets configured")
		d.ErrorMsg = err.Error()
		r.record(d)
		return zero, err
	}
	var lastErr error
	var cooling []string
	for pass := 0; pass < 2; pass++ {
		list := targets
		if pass == 1 {
			list = cooling
		}
		for _, target := range list {
			if pass == 0 && !r.available(target) {
				cooling = append(cooling, target)
				d.Skipped = append(d.Skipped, target+" (cooldown)")
				continue
			}
			prov, model, err := r.resolve(target)
			if err != nil {
				d.Skipped = append(d.Skipped, target+" (unavailable)")
				lastErr = err
				continue
			}
			targetReq := *req
			targetReq.Model = model
			out, err := call(prov, &targetReq)
			r.report(target, err)
			if err != nil {
				d.Skipped = append(d.Skipped, target+" (error)")
				lastErr = err
				if ctx.Err() != nil {
					break
				}
				continue
			}
			d.Target = target
			r.record(d)
			if tr := trace.FromContext(ctx); tr != nil {
				tr.SetRoute(req.Model, prov.Name(), prov.Endpoint(), model)
				tr.AddNote(fmt.Sprintf("smart %s via %s", tier, res.Source))
			}
			log.Printf("[SMART] tier=%s source=%s conf=%.2f pinned=%v -> %s", tier, res.Source, res.Confidence, pinned, target)
			return out, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("smart router: all targets are unavailable")
	}
	d.ErrorMsg = lastErr.Error()
	r.record(d)
	return zero, lastErr
}

func (r *Router) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	return dispatch(r, ctx, req, func(p providers.Provider, q *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
		return p.Execute(ctx, q)
	})
}

func (r *Router) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	return dispatch(r, ctx, req, func(p providers.Provider, q *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
		return p.Stream(ctx, q)
	})
}
