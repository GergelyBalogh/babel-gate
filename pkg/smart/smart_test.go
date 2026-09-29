package smart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vogler75/babel-gate/pkg/budget"
	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/providers"
)

type fakeProvider struct {
	name  string
	fail  bool
	calls []string
	usage canonical.Usage
}

func (f *fakeProvider) Name() string     { return f.name }
func (f *fakeProvider) Type() string     { return "fake" }
func (f *fakeProvider) Endpoint() string { return "http://" + f.name }
func (f *fakeProvider) Execute(_ context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	f.calls = append(f.calls, req.Model)
	if f.fail {
		return nil, errors.New("boom")
	}
	return &canonical.CanonicalResponse{Model: f.name + "/" + req.Model, Usage: f.usage}, nil
}
func (f *fakeProvider) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	if _, err := f.Execute(ctx, req); err != nil {
		return nil, err
	}
	ch := make(chan canonical.CanonicalEvent, 2)
	u := f.usage
	ch <- canonical.CanonicalEvent{Type: canonical.EventMessageStart, Usage: &canonical.Usage{PromptTokens: u.PromptTokens}}
	ch <- canonical.CanonicalEvent{Type: canonical.EventMessageDelta, Usage: &u}
	close(ch)
	return ch, nil
}
func (f *fakeProvider) ListModels(context.Context) ([]providers.ModelInfo, error) { return nil, nil }

func setup(t *testing.T) (*Router, map[string]*fakeProvider) {
	t.Helper()
	provs := map[string]*fakeProvider{"onprem": {name: "onprem"}, "copilot": {name: "copilot"}, "sdc": {name: "sdc"}}
	resolve := func(target string) (providers.Provider, string, error) {
		p, m, _ := strings.Cut(target, "/")
		fp, ok := provs[p]
		if !ok {
			return nil, "", fmt.Errorf("unknown %s", p)
		}
		return fp, m, nil
	}
	cfg := config.SmartConfig{
		Enabled: true,
		Tiers: map[string][]string{
			"SIMPLE":    {"onprem/gpt-oss-120b", "copilot/claude-haiku-4.5"},
			"COMPLEX":   {"copilot/claude-sonnet-5", "sdc/claude-sonnet-5"},
			"REASONING": {"copilot/claude-opus-5.5", "sdc/claude-opus-5-5"},
		},
	}
	return New(cfg, resolve), provs
}

func ask(text, session string) *canonical.CanonicalRequest {
	return &canonical.CanonicalRequest{Model: DefaultModel, SessionID: session, Messages: []canonical.Message{
		{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: text}}},
	}}
}

func TestRoutesByTierAndClimbsForMissingTier(t *testing.T) {
	r, _ := setup(t)
	resp, err := r.Execute(context.Background(), ask("fix typo", ""))
	if err != nil || resp.Model != "onprem/gpt-oss-120b" {
		t.Fatalf("simple -> %v %v", resp, err)
	}
	resp, err = r.Execute(context.Background(), ask("please ultrathink about this", ""))
	if err != nil || resp.Model != "copilot/claude-opus-5.5" {
		t.Fatalf("reasoning -> %v %v", resp, err)
	}
	if got := r.candidates("MEDIUM"); got[0] != "copilot/claude-sonnet-5" {
		t.Fatalf("MEDIUM should climb to COMPLEX first, got %v", got)
	}
}

func TestFailoverAndCooldown(t *testing.T) {
	r, provs := setup(t)
	now := time.Now()
	r.now = func() time.Time { return now }
	provs["onprem"].fail = true
	for i := 0; i < 2; i++ {
		resp, err := r.Execute(context.Background(), ask("fix typo", ""))
		if err != nil || resp.Model != "copilot/claude-haiku-4.5" {
			t.Fatalf("failover -> %v %v", resp, err)
		}
	}
	if len(provs["onprem"].calls) != 2 {
		t.Fatalf("onprem calls = %d", len(provs["onprem"].calls))
	}
	_, _ = r.Execute(context.Background(), ask("fix typo", ""))
	if len(provs["onprem"].calls) != 2 {
		t.Fatal("onprem should be cooling down")
	}
	now = now.Add(6 * time.Minute)
	provs["onprem"].fail = false
	resp, _ := r.Execute(context.Background(), ask("fix typo", ""))
	if resp.Model != "onprem/gpt-oss-120b" {
		t.Fatalf("after cooldown -> %v", resp.Model)
	}
}

func TestSessionAffinityNeverDowngrades(t *testing.T) {
	r, _ := setup(t)
	if _, err := r.Stream(context.Background(), ask("ultrathink: redesign", "s1")); err != nil {
		t.Fatal(err)
	}
	resp, _ := r.Execute(context.Background(), ask("thanks", "s1"))
	if resp.Model != "copilot/claude-opus-5.5" {
		t.Fatalf("session should stay pinned, got %s", resp.Model)
	}
	d := r.Decisions()
	if len(d) != 2 || !d[1].Pinned {
		t.Fatalf("decisions = %+v", d)
	}
	resp, _ = r.Execute(context.Background(), ask("thanks", "s2"))
	if resp.Model != "onprem/gpt-oss-120b" {
		t.Fatalf("other session unaffected, got %s", resp.Model)
	}
}

func TestBudgetSkipsExhaustedProvider(t *testing.T) {
	r, provs := setup(t)
	tr := budget.New(map[string]config.BudgetConfig{
		"copilot": {Limit: 1, PeriodDays: 30, TierWeights: map[string]float64{"REASONING": 1}, DefaultPrice: []float64{1, 1}},
	})
	r.SetBudget(tr)

	provs["copilot"].usage = canonical.Usage{PromptTokens: 2_000_000}
	resp, _ := r.Execute(context.Background(), ask("ultrathink", ""))
	if resp.Model != "copilot/claude-opus-5.5" {
		t.Fatalf("first -> %s", resp.Model)
	}
	resp, _ = r.Execute(context.Background(), ask("ultrathink", ""))
	if resp.Model != "sdc/claude-opus-5-5" {
		t.Fatalf("budget exhausted -> %s", resp.Model)
	}

	ch, err := r.Stream(context.Background(), ask("ultrathink", ""))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	d := r.Decisions()
	if last := d[len(d)-1]; last.Target != "sdc/claude-opus-5-5" || len(last.Skipped) == 0 {
		t.Fatalf("decision = %+v", last)
	}
}

func TestStreamRecordsUsage(t *testing.T) {
	r, provs := setup(t)
	tr := budget.New(map[string]config.BudgetConfig{"onprem": {Limit: 1, DefaultPrice: []float64{1, 1}}})
	r.SetBudget(tr)
	provs["onprem"].usage = canonical.Usage{PromptTokens: 2_000_000}
	ch, err := r.Stream(context.Background(), ask("fix typo", ""))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if tr.Allowed("onprem", "SIMPLE") {
		t.Fatal("stream usage should be recorded")
	}
}

func TestAllTargetsFail(t *testing.T) {
	r, provs := setup(t)
	for _, p := range provs {
		p.fail = true
	}
	if _, err := r.Execute(context.Background(), ask("fix typo", "")); err == nil {
		t.Fatal("expected error")
	}
}
