package inbound

import (
	"encoding/json"

	"github.com/vogler75/babel-gate/pkg/providers/anthropic"
)

// detectReadLoop examines the suffix of request conversation history. A
// model request contains the earlier tool calls even though its HTTP body
// changes on every turn. Only Read calls count; a new user instruction or any
// different tool call resets the sequence. Short cycles catch alternating file
// ranges as well as identical calls.
func detectReadLoop(messages []anthropic.Message, repetitions int) int {
	if repetitions < 2 {
		return 0
	}
	const maxCycleLength = 3
	maxReads := maxCycleLength * repetitions
	var reads []string
	for _, message := range messages {
		blocks, ok := message.Content.([]any)
		if !ok {
			if content, isText := message.Content.(string); message.Role == "user" && isText && content != "" {
				reads = reads[:0]
			}
			continue
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "tool_use":
				if message.Role != "assistant" || block["name"] != "Read" {
					reads = reads[:0]
					continue
				}
				input, err := json.Marshal(block["input"])
				if err != nil {
					reads = reads[:0]
					continue
				}
				reads = append(reads, string(input))
				if len(reads) > maxReads {
					reads = reads[1:]
				}
			case "text", "image":
				if message.Role == "user" {
					reads = reads[:0]
				}
			}
		}
	}
	for length := 1; length <= maxCycleLength; length++ {
		needed := length * repetitions
		if len(reads) < needed {
			continue
		}
		tail := reads[len(reads)-needed:]
		matches := true
		for i := length; i < len(tail); i++ {
			if tail[i] != tail[i%length] {
				matches = false
				break
			}
		}
		if matches {
			return length
		}
	}
	return 0
}
