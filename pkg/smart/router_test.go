package smart

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
)

func userText(text string) canonical.Message {
	return canonical.Message{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: text}}}
}

func toolResult() canonical.Message {
	return canonical.Message{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartToolResult, ToolResultID: "t1", ToolResultContent: "ok"}}}
}

func assistantCall() canonical.Message {
	return canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{{Type: canonical.PartToolCall, ToolCallID: "t1", ToolCallName: "Read", ToolCallArgs: "{}"}}}
}

func TestHeuristicTiers(t *testing.T) {
	budget := 32000
	tests := []struct {
		name string
		req  canonical.CanonicalRequest
		want Tier
	}{
		{"typo", canonical.CanonicalRequest{Messages: []canonical.Message{userText("fix the typo in README")}}, TierSimple},
		{"implement with tools", canonical.CanonicalRequest{
			Messages: []canonical.Message{userText("implement a retry wrapper for the HTTP client")},
			Tools:    []canonical.ToolDeclaration{{Name: "Edit"}},
		}, TierComplex},
		{"architecture with thinking", canonical.CanonicalRequest{
			Messages: []canonical.Message{userText("investigate the root cause of this deadlock in the worker pool " + strings.Repeat("context ", 120))},
			Tools:    []canonical.ToolDeclaration{{Name: "Read"}},
			Thinking: &canonical.ThinkingConfig{Type: "enabled", BudgetTokens: &budget},
		}, TierReasoning},
		{"plain question with tools", canonical.CanonicalRequest{
			Messages: []canonical.Message{userText("which files handle routing in this repository and how are they connected?")},
			Tools:    []canonical.ToolDeclaration{{Name: "Read"}},
		}, TierSimple},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Heuristic{}.Classify(context.Background(), &tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Tier != tt.want {
				t.Fatalf("tier = %s, want %s (%s)", got.Tier, tt.want, got.Reason)
			}
		})
	}
}

func TestLastUserTextSkipsToolResults(t *testing.T) {
	req := &canonical.CanonicalRequest{Messages: []canonical.Message{userText("refactor the parser"), assistantCall(), toolResult()}}
	if got := LastUserText(req); got != "refactor the parser" {
		t.Fatalf("LastUserText = %q", got)
	}
	if IsNewTurn(req) {
		t.Fatal("tool result continuation reported as new turn")
	}
	req.Messages = append(req.Messages, canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "done"}}}, userText("thanks"))
	if !IsNewTurn(req) {
		t.Fatal("fresh user text not reported as new turn")
	}
}

func TestRouterKeepsTierForRestOfTurn(t *testing.T) {
	r, err := NewRouter(config.SmartConfig{Tiers: map[string][]string{
		"simple":  {"onprem/small"},
		"complex": {"copilot/sonnet", "sdc/sol"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	start := &canonical.CanonicalRequest{
		SessionID: "s1",
		Messages:  []canonical.Message{userText("implement a retry wrapper for the HTTP client")},
		Tools:     []canonical.ToolDeclaration{{Name: "Edit"}},
	}
	d := r.Decide(context.Background(), start)
	if d.Tier != TierComplex || d.Sticky {
		t.Fatalf("first decision = %+v", d)
	}

	// The follow-up's instruction alone would classify lower; the turn keeps its tier.
	follow := &canonical.CanonicalRequest{
		SessionID: "s1",
		Messages:  []canonical.Message{userText("ok"), assistantCall(), toolResult()},
	}
	d = r.Decide(context.Background(), follow)
	if d.Tier != TierComplex || !d.Sticky {
		t.Fatalf("continuation decision = %+v", d)
	}

	next := &canonical.CanonicalRequest{SessionID: "s1", Messages: []canonical.Message{userText("fix the typo")}}
	if d = r.Decide(context.Background(), next); d.Tier != TierSimple || d.Sticky {
		t.Fatalf("new turn decision = %+v", d)
	}
}

func TestTargetsForBorrowsFromNeighbouringTier(t *testing.T) {
	r, err := NewRouter(config.SmartConfig{Tiers: map[string][]string{"medium": {"a/m"}, "complex": {"a/c"}}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[Tier]string{TierSimple: "a/m", TierMedium: "a/m", TierComplex: "a/c", TierReasoning: "a/c"}
	for tier, want := range cases {
		if got := r.TargetsFor(tier); len(got) != 1 || got[0] != want {
			t.Fatalf("TargetsFor(%s) = %v, want [%s]", tier, got, want)
		}
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	bad := []config.SmartConfig{
		{Tiers: map[string][]string{"hard": {"a/b"}}},
		{Tiers: map[string][]string{"simple": {"smart"}}},
		{Tiers: map[string][]string{"simple": {"a/b"}}, Classifier: config.ClassifierConfig{Mode: "http"}},
		{Tiers: map[string][]string{"simple": {"a/b"}}, Sticky: "forever"},
	}
	for i, cfg := range bad {
		if err := Validate(cfg); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
}

func TestHTTPClassifierAndHeuristicFallback(t *testing.T) {
	confidence := 0.9
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in httpClassifyRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Text != "fix the typo" {
			t.Errorf("classifier got text %q", in.Text)
		}
		_ = json.NewEncoder(w).Encode(httpClassifyResponse{Tier: "reasoning", Confidence: confidence})
	}))
	defer srv.Close()

	r, err := NewRouter(config.SmartConfig{
		Sticky:     "none",
		Classifier: config.ClassifierConfig{Mode: "http", URL: srv.URL},
		Tiers:      map[string][]string{"simple": {"a/s"}, "reasoning": {"a/r"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &canonical.CanonicalRequest{Messages: []canonical.Message{userText("fix the typo")}}
	if d := r.Decide(context.Background(), req); d.Tier != TierReasoning {
		t.Fatalf("confident classifier ignored: %+v", d)
	}
	confidence = 0.2
	if d := r.Decide(context.Background(), req); d.Tier != TierSimple {
		t.Fatalf("low-confidence verdict not replaced by heuristic: %+v", d)
	}
	srv.Close()
	if d := r.Decide(context.Background(), req); d.Tier != TierSimple {
		t.Fatalf("unreachable classifier not replaced by heuristic: %+v", d)
	}
}
