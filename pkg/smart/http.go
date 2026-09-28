package smart

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// HTTPClassifier delegates classification to an external service, such as a
// locally hosted decision model. Protocol:
//
//	POST {"text": "...", "tools": 3, "messages": 12}
//	200  {"tier": "complex", "confidence": 0.82}
type HTTPClassifier struct {
	URL    string
	Client *http.Client
}

type httpClassifyRequest struct {
	Text     string `json:"text"`
	Tools    int    `json:"tools"`
	Messages int    `json:"messages"`
}

type httpClassifyResponse struct {
	Tier       string  `json:"tier"`
	Confidence float64 `json:"confidence"`
}

// NewHTTPClassifier creates a classifier with a per-call timeout.
func NewHTTPClassifier(url string, timeout time.Duration) *HTTPClassifier {
	return &HTTPClassifier{URL: url, Client: &http.Client{Timeout: timeout}}
}

// Classify implements Classifier.
func (c *HTTPClassifier) Classify(ctx context.Context, req *canonical.CanonicalRequest) (Classification, error) {
	body, err := json.Marshal(httpClassifyRequest{Text: LastUserText(req), Tools: len(req.Tools), Messages: len(req.Messages)})
	if err != nil {
		return Classification{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return Classification{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return Classification{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Classification{}, fmt.Errorf("classifier status %d: %s", resp.StatusCode, msg)
	}
	var out httpClassifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Classification{}, fmt.Errorf("decoding classifier response: %w", err)
	}
	tier, err := ParseTier(out.Tier)
	if err != nil {
		return Classification{}, err
	}
	return Classification{Tier: tier, Confidence: out.Confidence, Reason: fmt.Sprintf("classifier %.2f", out.Confidence)}, nil
}
