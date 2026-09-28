package smart

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// LayaClassifier asks a Laya "System 1" decision model (laya-serve, which
// speaks TypeSafe Jev's /v1/systemone protocol) for the tier. Laya reads the
// request and answers a typed choice question; it generates no text, so a
// decision takes tens to a few hundred milliseconds on a local CPU or GPU.
type LayaClassifier struct {
	URL    string // base URL, e.g. http://localhost:8000
	APIKey string // optional LAYA_API_KEY of the server
	Model  string // optional checkpoint: english, multilingual, typed-decisions
	Client *http.Client
}

// Laya's checkpoints read at most 512 tokens, so only the start and end of a
// long instruction are sent; that is where the ask usually is.
const (
	layaHeadChars = 1500
	layaTailChars = 500
)

const layaQuestion = "tier"

var layaCriteria = map[string]string{
	"simple":    "a quick, mechanical ask: a question, a typo, a rename, a format change, a summary or translation",
	"medium":    "an ordinary coding task limited to a small, well-defined change in one place",
	"complex":   "substantial engineering work: implementing a feature, refactoring, debugging, or changes across several files",
	"reasoning": "hard open-ended thinking: architecture or design decisions, root-cause analysis, concurrency, security or proofs",
}

type layaState struct {
	Request        string `json:"request"`
	ToolsAvailable bool   `json:"tools_available"`
	Thinking       bool   `json:"thinking_requested"`
	Messages       int    `json:"conversation_messages"`
}

type layaQuestionDef struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type layaRequest struct {
	Model     string                     `json:"model,omitempty"`
	State     layaState                  `json:"state"`
	Questions map[string]layaQuestionDef `json:"questions"`
}

type layaAnswer struct {
	Choice           string   `json:"choice"`
	Confidence       float64  `json:"confidence"`
	AnswerConfidence *float64 `json:"answer_confidence"`
}

type layaResponse struct {
	Model   string                `json:"model"`
	Answers map[string]layaAnswer `json:"answers"`
}

// NewLayaClassifier creates a classifier for a laya-serve base URL.
func NewLayaClassifier(url, apiKey, model string, timeout time.Duration) *LayaClassifier {
	return &LayaClassifier{
		URL:    strings.TrimSuffix(strings.TrimSuffix(url, "/"), "/v1/systemone"),
		APIKey: apiKey,
		Model:  model,
		Client: &http.Client{Timeout: timeout},
	}
}

// Classify implements Classifier.
func (c *LayaClassifier) Classify(ctx context.Context, req *canonical.CanonicalRequest) (Classification, error) {
	text := LastUserText(req)
	if text == "" {
		return Classification{}, fmt.Errorf("laya: request has no user instruction")
	}
	body, err := json.Marshal(layaRequest{
		Model: c.Model,
		State: layaState{
			Request:        clipMiddle(text, layaHeadChars, layaTailChars),
			ToolsAvailable: len(req.Tools) > 0,
			Thinking:       req.Thinking != nil && req.Thinking.Type != "" && req.Thinking.Type != "disabled",
			Messages:       len(req.Messages),
		},
		Questions: map[string]layaQuestionDef{layaQuestion: {
			Type:         "choice",
			Instructions: "How capable a model does the task in `request` need?",
			Criteria:     layaCriteria,
		}},
	})
	if err != nil {
		return Classification{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Classification{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	start := time.Now()
	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return Classification{}, fmt.Errorf("laya: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Classification{}, fmt.Errorf("laya status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out layaResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Classification{}, fmt.Errorf("decoding laya response: %w", err)
	}
	answer, ok := out.Answers[layaQuestion]
	if !ok {
		return Classification{}, fmt.Errorf("laya response has no %q answer", layaQuestion)
	}
	tier, err := ParseTier(answer.Choice)
	if err != nil {
		return Classification{}, fmt.Errorf("laya: %w", err)
	}
	// answer_confidence is the calibrated probability of the chosen option;
	// older servers only report the entropy-based confidence.
	confidence := answer.Confidence
	if answer.AnswerConfidence != nil {
		confidence = *answer.AnswerConfidence
	}
	reason := fmt.Sprintf("laya %.2f in %dms", confidence, time.Since(start).Milliseconds())
	if out.Model != "" {
		reason = fmt.Sprintf("laya %s %.2f in %dms", out.Model, confidence, time.Since(start).Milliseconds())
	}
	return Classification{Tier: tier, Confidence: confidence, Reason: reason}, nil
}

// clipMiddle keeps the first head and last tail bytes of s, cut on rune
// boundaries, when s is longer than both together.
func clipMiddle(s string, head, tail int) string {
	if len(s) <= head+tail {
		return s
	}
	h := head
	for h > 0 && !isRuneStart(s[h]) {
		h--
	}
	t := len(s) - tail
	for t < len(s) && !isRuneStart(s[t]) {
		t++
	}
	return s[:h] + "\n[...]\n" + s[t:]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
