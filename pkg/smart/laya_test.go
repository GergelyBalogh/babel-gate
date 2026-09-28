package smart

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
)

// fakeLaya mimics laya-serve's /v1/systemone: bearer check, request
// validation as in serve.py, and a Jev-shaped answer.
func fakeLaya(t *testing.T, apiKey string, answer func(req map[string]any) map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if apiKey != "" && r.Header.Get("Authorization") != "Bearer "+apiKey {
			http.Error(w, `{"detail":"invalid or missing bearer token"}`, http.StatusUnauthorized)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"detail":"request body must be valid JSON"}`, http.StatusBadRequest)
			return
		}
		if req["state"] == nil {
			http.Error(w, `{"detail":"'state' is required"}`, http.StatusBadRequest)
			return
		}
		q, _ := req["questions"].(map[string]any)["tier"].(map[string]any)
		if q["type"] != "choice" || q["instructions"] == "" {
			http.Error(w, `{"detail":"bad question"}`, http.StatusUnprocessableEntity)
			return
		}
		if crit, _ := q["criteria"].(map[string]any); len(crit) != 4 {
			http.Error(w, `{"detail":"bad criteria"}`, http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "english",
			"answers": map[string]any{"tier": answer(req)},
			"usage":   map[string]any{"input_tokens": 42, "output_tokens": 0},
		})
	}))
}

func TestLayaClassifier(t *testing.T) {
	var gotState map[string]any
	srv := fakeLaya(t, "secret", func(req map[string]any) map[string]any {
		gotState = req["state"].(map[string]any)
		return map[string]any{"type": "choice", "choice": "complex", "confidence": 0.41, "answer_confidence": 0.87}
	})
	defer srv.Close()

	c := NewLayaClassifier(srv.URL+"/v1/systemone", "secret", "", 0)
	req := &canonical.CanonicalRequest{
		Messages: []canonical.Message{userText("refactor the session store"), assistantCall(), toolResult()},
		Tools:    []canonical.ToolDeclaration{{Name: "Edit"}},
	}
	got, err := c.Classify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != TierComplex || got.Confidence != 0.87 {
		t.Fatalf("classification = %+v, want complex with calibrated confidence", got)
	}
	if gotState["request"] != "refactor the session store" || gotState["tools_available"] != true {
		t.Fatalf("state sent to laya = %v", gotState)
	}

	c.APIKey = "wrong"
	if _, err := c.Classify(context.Background(), req); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestLayaRouterFallsBackToHeuristic(t *testing.T) {
	answer := map[string]any{"type": "choice", "choice": "reasoning", "confidence": 0.9}
	srv := fakeLaya(t, "", func(map[string]any) map[string]any { return answer })
	defer srv.Close()

	r, err := NewRouter(config.SmartConfig{
		Sticky:     "none",
		Classifier: config.ClassifierConfig{Mode: "laya", URL: srv.URL},
		Tiers:      map[string][]string{"simple": {"a/s"}, "reasoning": {"a/r"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &canonical.CanonicalRequest{Messages: []canonical.Message{userText("fix the typo")}}
	if d := r.Decide(context.Background(), req); d.Tier != TierReasoning || !strings.HasPrefix(d.Reason, "laya") {
		t.Fatalf("confident laya verdict ignored: %+v", d)
	}
	answer = map[string]any{"type": "choice", "choice": "reasoning", "confidence": 0.3}
	if d := r.Decide(context.Background(), req); d.Tier != TierSimple {
		t.Fatalf("low-confidence verdict not replaced by heuristic: %+v", d)
	}
	answer = map[string]any{"type": "choice", "choice": "genius", "confidence": 0.9}
	if d := r.Decide(context.Background(), req); d.Tier != TierSimple {
		t.Fatalf("unknown choice not replaced by heuristic: %+v", d)
	}
	srv.Close()
	if d := r.Decide(context.Background(), req); d.Tier != TierSimple {
		t.Fatalf("unreachable laya not replaced by heuristic: %+v", d)
	}
	if err := Validate(config.SmartConfig{Tiers: map[string][]string{"simple": {"a/s"}}, Classifier: config.ClassifierConfig{Mode: "laya"}}); err == nil {
		t.Fatal("laya without url accepted")
	}
}

func TestClipMiddleKeepsRunesIntact(t *testing.T) {
	s := strings.Repeat("ä", 3000) // two bytes per rune
	got := clipMiddle(s, layaHeadChars, layaTailChars)
	if !utf8.ValidString(got) || len(got) > layaHeadChars+layaTailChars+len("\n[...]\n") {
		t.Fatalf("clipped to %d bytes, valid utf8 %v", len(got), utf8.ValidString(got))
	}
	if clipMiddle("short", 10, 10) != "short" {
		t.Fatal("short text changed")
	}
}
