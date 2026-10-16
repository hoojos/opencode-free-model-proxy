package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type collectChatKey struct{}

// Keep unknown model-specific options intact while adapting anonymous free chat.
func prepareChat(r *http.Request) (bool, error) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return false, fmt.Errorf("read request: %w", err)
	}
	var request struct {
		Model  string            `json:"model"`
		Stream bool              `json:"stream"`
		Tools  []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return false, fmt.Errorf("invalid chat request: %w", err)
	}
	if strings.HasSuffix(request.Model, "-free") {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return false, err
		}
		fields["stream"] = json.RawMessage(`true`)
		if !request.Stream {
			fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
		// The free endpoint expects editor tool declarations even for plain chat.
		// Disable them explicitly: this bridge never executes filesystem tools.
		if len(request.Tools) == 0 {
			fields["tools"] = json.RawMessage(`[
				{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{}}}},
				{"type":"function","function":{"name":"edit","parameters":{"type":"object","properties":{}}}},
				{"type":"function","function":{"name":"write","parameters":{"type":"object","properties":{}}}}
			]`)
			fields["tool_choice"] = json.RawMessage(`"none"`)
		}
		body, err = json.Marshal(fields)
		if err != nil {
			return false, err
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Del("Content-Length")
	return strings.HasSuffix(request.Model, "-free") && !request.Stream, nil
}

type toolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	Reasoning string     `json:"reasoning_content,omitempty"`
	Tools     []toolCall `json:"tool_calls,omitempty"`
}

type chatChoice struct {
	Index   int         `json:"index"`
	Message chatMessage `json:"message"`
	Finish  *string     `json:"finish_reason"`
}

func collectChat(r *http.Response) error {
	defer r.Body.Close()
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("expected an SSE chat response")
	}
	var completion struct {
		ID      string          `json:"id"`
		Object  string          `json:"object"`
		Created int64           `json:"created"`
		Model   string          `json:"model"`
		Choices []chatChoice    `json:"choices"`
		Usage   json.RawMessage `json:"usage,omitempty"`
	}
	completion.Object = "chat.completion"
	choices := map[int]*chatChoice{}
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	done := false
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk struct {
			ID      string          `json:"id"`
			Created int64           `json:"created"`
			Model   string          `json:"model"`
			Usage   json.RawMessage `json:"usage"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index  int         `json:"index"`
				Delta  chatMessage `json:"delta"`
				Finish *string     `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("invalid chat stream: %w", err)
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return fmt.Errorf("upstream chat stream error: %s", chunk.Error)
		}
		if chunk.ID != "" {
			completion.ID, completion.Created, completion.Model = chunk.ID, chunk.Created, chunk.Model
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			completion.Usage = chunk.Usage
		}
		for _, delta := range chunk.Choices {
			choice := choices[delta.Index]
			if choice == nil {
				choice = &chatChoice{Index: delta.Index, Message: chatMessage{Role: "assistant"}}
				choices[delta.Index] = choice
			}
			if delta.Delta.Content != nil {
				if choice.Message.Content == nil {
					choice.Message.Content = new(string)
				}
				*choice.Message.Content += *delta.Delta.Content
			}
			choice.Message.Reasoning += delta.Delta.Reasoning
			if delta.Finish != nil {
				choice.Finish = delta.Finish
			}
			for _, tool := range delta.Delta.Tools {
				index := -1
				for i := range choice.Message.Tools {
					if choice.Message.Tools[i].Index == tool.Index {
						index = i
						break
					}
				}
				if index == -1 {
					choice.Message.Tools = append(choice.Message.Tools, tool)
					continue
				}
				current := &choice.Message.Tools[index]
				if tool.ID != "" {
					current.ID = tool.ID
				}
				if tool.Type != "" {
					current.Type = tool.Type
				}
				current.Function.Name += tool.Function.Name
				current.Function.Arguments += tool.Function.Arguments
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !done || len(choices) == 0 {
		return fmt.Errorf("incomplete upstream chat stream")
	}
	for _, choice := range choices {
		completion.Choices = append(completion.Choices, *choice)
	}
	sort.Slice(completion.Choices, func(i, j int) bool { return completion.Choices[i].Index < completion.Choices[j].Index })
	body, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}
