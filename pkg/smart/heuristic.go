package smart

import (
	"context"
	"fmt"
	"strings"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// Classification is a classifier's verdict for one request.
type Classification struct {
	Tier       Tier
	Confidence float64 // 0..1
	Reason     string
}

// Classifier assigns a tier to a request.
type Classifier interface {
	Classify(ctx context.Context, req *canonical.CanonicalRequest) (Classification, error)
}

// Heuristic is the dependency-free default classifier. It scores the latest
// user instruction by keywords, size and request features.
type Heuristic struct{}

var (
	reasoningKeywords = []string{
		"architecture", "architect", "design a", "design the", "root cause", "race condition", "deadlock",
		"prove", "trade-off", "tradeoff", "investigate", "analyze", "analyse", "security review",
		"concurrency", "performance bottleneck", "step by step", "think hard", "ultrathink",
		"architektur", "analysiere", "ursache", "warum",
	}
	complexKeywords = []string{
		"implement", "refactor", "debug", "migrate", "feature", "write tests", "add tests", "integrate",
		"optimize", "optimise", "rewrite", "fix the bug", "failing test", "plan",
		"implementier", "umbauen", "fehler", "erweitere", "plane",
	}
	simpleKeywords = []string{
		"typo", "rename", "reformat", "format ", "spelling", "comment", "translate", "summarize", "summarise",
		"title", "commit message", "what is", "list ",
		"tippfehler", "umbenennen", "übersetze", "zusammenfass",
	}
)

// Classify implements Classifier.
func (Heuristic) Classify(_ context.Context, req *canonical.CanonicalRequest) (Classification, error) {
	text := strings.ToLower(LastUserText(req))
	score := 0
	var reasons []string
	add := func(points int, reason string) {
		score += points
		reasons = append(reasons, fmt.Sprintf("%+d %s", points, reason))
	}

	strongKeyword := false
	if hit := firstKeyword(text, reasoningKeywords); hit != "" {
		add(3, "keyword "+hit)
		strongKeyword = true
	} else if hit := firstKeyword(text, complexKeywords); hit != "" {
		add(2, "keyword "+hit)
		strongKeyword = true
	} else if hit := firstKeyword(text, simpleKeywords); hit != "" {
		add(-1, "keyword "+hit)
	}

	switch n := len(text); {
	case n > 4000:
		add(2, "long instruction")
	case n > 800:
		add(1, "medium instruction")
	case n < 120 && !strongKeyword:
		// A terse "implement X" is still real work; only plain short asks are cheap.
		add(-1, "short instruction")
	}
	if blocks := strings.Count(text, "```") / 2; blocks >= 2 {
		add(1, "multiple code blocks")
	}
	if len(req.Tools) > 0 {
		add(1, "tools available")
	}
	if req.Thinking != nil && req.Thinking.Type != "" && req.Thinking.Type != "disabled" {
		points := 1
		if req.Thinking.BudgetTokens != nil && *req.Thinking.BudgetTokens >= 16000 {
			points = 2
		}
		if l := strings.ToLower(req.Thinking.Level); l == "high" || l == "max" || l == "xhigh" {
			points = 2
		}
		add(points, "thinking requested")
	}

	contextChars := 0
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			contextChars += len(p.Text) + len(p.ToolCallArgs) + len(p.ToolResultText())
		}
	}
	if contextChars > 400_000 {
		add(2, "very large context")
	} else if contextChars > 120_000 {
		add(1, "large context")
	}

	tier := TierSimple
	switch {
	case score >= 5:
		tier = TierReasoning
	case score >= 3:
		tier = TierComplex
	case score >= 1:
		tier = TierMedium
	}
	// Scores just across a boundary are less certain than scores deep inside a band.
	confidence := 0.8
	if score == 1 || score == 3 || score == 5 {
		confidence = 0.6
	}
	return Classification{Tier: tier, Confidence: confidence, Reason: "heuristic score " + fmt.Sprint(score) + " (" + strings.Join(reasons, ", ") + ")"}, nil
}

func firstKeyword(text string, keywords []string) string {
	for _, k := range keywords {
		if strings.Contains(text, k) {
			return strings.TrimSpace(k)
		}
	}
	return ""
}

// LastUserText returns the text of the most recent user message that carries
// text. Tool results are skipped so that a whole agent turn is judged by the
// instruction that started it.
func LastUserText(req *canonical.CanonicalRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != canonical.RoleUser {
			continue
		}
		if text := strings.TrimSpace(m.TextContent()); text != "" && !isContinuation(m) {
			return text
		}
	}
	return ""
}

// IsNewTurn reports whether the request starts a new user turn rather than
// continuing an agent loop with tool results.
func IsNewTurn(req *canonical.CanonicalRequest) bool {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		switch m.Role {
		case canonical.RoleSystem:
			continue
		case canonical.RoleTool:
			return false
		case canonical.RoleUser:
			return !isContinuation(m)
		default:
			return true
		}
	}
	return true
}

func isContinuation(m canonical.Message) bool {
	for _, p := range m.Parts {
		if p.Type == canonical.PartToolResult {
			return true
		}
	}
	return false
}
