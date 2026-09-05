package upstream

import (
	"encoding/json"
	"strings"
)

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Event struct {
	Done             bool
	OutputCharacters int
	Usage            Usage
}

func ParseEvent(data []byte) Event {
	var event Event
	var payload strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if value == "[DONE]" {
			event.Done = true
			continue
		}
		if payload.Len() > 0 {
			payload.WriteByte('\n')
		}
		payload.WriteString(value)
	}
	if payload.Len() == 0 {
		return event
	}
	var raw struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if json.Unmarshal([]byte(payload.String()), &raw) != nil {
		return event
	}
	for _, choice := range raw.Choices {
		event.OutputCharacters += len([]rune(choice.Delta.Content))
	}
	event.Usage = raw.Usage
	return event
}
