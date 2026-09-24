package inbound

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/providers/anthropic"
)

func readExchange(path string, offset int) []anthropic.Message {
	return []anthropic.Message{
		{Role: "assistant", Content: []any{map[string]any{
			"type": "tool_use", "name": "Read", "id": fmt.Sprintf("tool-%d", offset),
			"input": map[string]any{"file_path": path, "offset": offset, "limit": 30},
		}}},
		{Role: "user", Content: []any{map[string]any{
			"type": "tool_result", "tool_use_id": fmt.Sprintf("tool-%d", offset),
			"content": "unchanged since last read",
		}}},
	}
}

func TestDetectReadLoop(t *testing.T) {
	tests := []struct {
		name        string
		offsets     []int
		repetitions int
		want        int
	}{
		{"disabled", []int{240, 240, 240}, 0, 0},
		{"two reads are allowed", []int{240, 240}, 3, 0},
		{"identical reads", []int{240, 240, 240}, 3, 1},
		{"alternating ranges", []int{240, 270, 240, 270, 240, 270}, 3, 2},
		{"three step cycle", []int{270, 240, 240, 270, 240, 240, 270, 240, 240}, 3, 3},
		{"progressive reads", []int{240, 270, 300, 330, 360, 390}, 3, 0},
		{"insufficient cycle", []int{240, 270, 240, 270}, 3, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var messages []anthropic.Message
			for _, offset := range tc.offsets {
				messages = append(messages, readExchange("file.ts", offset)...)
			}
			if got := detectReadLoop(messages, tc.repetitions); got != tc.want {
				t.Fatalf("detectReadLoop() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDetectReadLoopResetsOnProgress(t *testing.T) {
	for _, reset := range []anthropic.Message{
		{Role: "user", Content: "Please inspect the file again"},
		{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "name": "Edit", "input": map[string]any{"file_path": "file.ts"}}}},
	} {
		messages := append(readExchange("file.ts", 240), readExchange("file.ts", 240)...)
		messages = append(messages, reset)
		messages = append(messages, readExchange("file.ts", 240)...)
		if got := detectReadLoop(messages, 3); got != 0 {
			t.Fatalf("expected progress to reset Read history, got cycle length %d", got)
		}
	}
	messages := append(readExchange("file.ts", 240), readExchange("other.ts", 240)...)
	messages = append(messages, readExchange("file.ts", 240)...)
	if got := detectReadLoop(messages, 3); got != 0 {
		t.Fatalf("different file paths must not count as identical reads, got %d", got)
	}
}

func TestReadLoopGuardRejectsBeforeRouting(t *testing.T) {
	var messages []anthropic.Message
	for range 3 {
		messages = append(messages, readExchange("file.ts", 240)...)
	}
	body, err := json.Marshal(anthropic.MessageRequest{Model: "gemini-test", MaxTokens: 100, Messages: messages, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	h := NewAnthropicHandler(nil, nil, nil)
	h.SetReadLoopRepetitions(3)
	w := httptest.NewRecorder()
	h.HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "repeated Read tool calls detected") {
		t.Fatalf("guard response: status=%d body=%q", w.Code, w.Body.String())
	}
}
