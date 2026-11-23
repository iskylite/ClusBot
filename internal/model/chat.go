package model

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"clusbot/internal/work"
)

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []chatCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Chat Completions groups all function calls from a turn into one assistant
// message. Keep that message together when converting the shared history.
type chatOutputItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Content   string `json:"content,omitempty"`
	Output    string `json:"output,omitempty"`
}

func marshalChatRequest(value any) ([]byte, error) {
	request := value.(*responseRequest)
	var messages []chatMessage
	if request.Instructions != "" {
		messages = append(messages, chatMessage{Role: "system", Content: &request.Instructions})
	}
	var assistant *chatMessage
	for _, input := range request.Input {
		if message, ok := input.(responseMessage); ok {
			assistant = nil
			var content any = &message.Content
			if len(message.images) > 0 {
				parts := []map[string]any{{"type": "text", "text": message.Content}}
				for _, url := range message.images {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url, "detail": "low"}})
				}
				content = parts
			}
			messages = append(messages, chatMessage{Role: message.Role, Content: content})
			continue
		}
		var item chatOutputItem
		raw, err := json.Marshal(input)
		if err != nil || json.Unmarshal(raw, &item) != nil {
			return nil, work.ErrReadCheckpoint
		}
		switch item.Type {
		case "message":
			messages = append(messages, chatMessage{Role: "assistant", Content: &item.Content})
			assistant = &messages[len(messages)-1]
		case "function_call":
			if assistant == nil {
				messages = append(messages, chatMessage{Role: "assistant"})
				assistant = &messages[len(messages)-1]
			}
			assistant.ToolCalls = append(assistant.ToolCalls, chatCall{
				ID: item.CallID, Type: "function", Function: chatFunction{Name: item.Name, Arguments: item.Arguments},
			})
		case "function_call_output":
			assistant = nil
			messages = append(messages, chatMessage{Role: "tool", ToolCallID: item.CallID, Content: &item.Output})
		default:
			return nil, work.ErrReadCheckpoint
		}
	}
	type function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      bool            `json:"strict"`
	}
	type tool struct {
		Type     string   `json:"type"`
		Function function `json:"function"`
	}
	var tools []tool
	for _, definition := range request.Tools {
		tools = append(tools, tool{definition.Type, function{
			definition.Name, definition.Description, definition.Parameters, definition.Strict,
		}})
	}
	effort := ""
	if request.Reasoning != nil {
		effort = request.Reasoning.Effort
	}
	return json.Marshal(struct {
		ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
		Model               string          `json:"model"`
		Messages            []chatMessage   `json:"messages"`
		Stream              bool            `json:"stream"`
		StreamOptions       map[string]bool `json:"stream_options"`
		MaxCompletionTokens int             `json:"max_completion_tokens"`
		Tools               []tool          `json:"tools,omitempty"`
		ToolChoice          string          `json:"tool_choice,omitempty"`
	}{effort, request.Model, messages, true, map[string]bool{"include_usage": true}, request.MaxOutputTokens, tools, request.ToolChoice})
}

type chatChunk struct {
	Model json.RawMessage `json:"model"`
	Error json.RawMessage
	Usage *struct {
		Prompt     *int64 `json:"prompt_tokens"`
		Completion *int64 `json:"completion_tokens"`
	}
	Choices []struct {
		Index  int
		Finish string `json:"finish_reason"`
		Delta  struct {
			Content          string
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			Calls            []struct {
				Index int
				chatCall
			} `json:"tool_calls"`
		}
	}
}

func readChatStream(ctx context.Context, body io.Reader, emit func(string) error) (responseData, error) {
	const maxEventBytes = 2 << 20
	reader := bufio.NewScanner(body)
	reader.Buffer(make([]byte, 4096), maxEventBytes)
	var result responseData
	var event, content strings.Builder
	var calls []chatCall
	var outputBytes int
	var finish string
	for reader.Scan() {
		line := reader.Text()
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if event.Len()+len(data)+1 > maxEventBytes {
				return result, errors.New("model event exceeds size limit")
			}
			if event.Len() > 0 {
				event.WriteByte('\n')
			}
			event.WriteString(data)
			continue
		}
		if line != "" || event.Len() == 0 {
			continue
		}
		data := event.String()
		event.Reset()
		if data == "[DONE]" {
			switch finish {
			case "length":
				return result, work.ErrOutputBudget
			case "stop":
				if len(calls) == 0 {
					return result, nil
				}
			case "tool_calls":
				return completeChatCalls(result, content.String(), calls)
			}
			return result, errors.New("model stream did not complete")
		}
		var chunk chatChunk
		if json.Unmarshal([]byte(data), &chunk) != nil || (len(chunk.Error) > 0 && string(chunk.Error) != "null") {
			return result, errors.New("invalid chat completion event")
		}
		result.observeModel(chunk.Model)
		if chunk.Usage != nil {
			result.Usage = &work.TokenUsage{InputTokens: chunk.Usage.Prompt, OutputTokens: chunk.Usage.Completion}
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 || finish != "" {
				return result, errors.New("unexpected chat completion choice")
			}
			finish = choice.Finish
			reasoning := choice.Delta.ReasoningContent
			if reasoning == "" {
				reasoning = choice.Delta.Reasoning
			}
			if reasoning != "" {
				outputBytes += len(reasoning)
				if outputBytes > maxEventBytes {
					return result, errors.New("model output exceeds size limit")
				}
				if err := work.EmitReasoning(ctx, reasoning); err != nil && ctx.Err() != nil {
					return result, err
				}
			}
			outputBytes += len(choice.Delta.Content)
			if outputBytes > maxEventBytes {
				return result, errors.New("model output exceeds size limit")
			}
			content.WriteString(choice.Delta.Content)
			if emit != nil && choice.Delta.Content != "" {
				if err := emit(choice.Delta.Content); err != nil {
					return result, err
				}
			}
			for _, delta := range choice.Delta.Calls {
				outputBytes += len(delta.ID) + len(delta.Function.Name) + len(delta.Function.Arguments)
				if delta.Index < 0 || delta.Index >= 32 || outputBytes > maxEventBytes ||
					(delta.Type != "" && delta.Type != "function") {
					return result, work.ErrReadModelCall
				}
				for len(calls) <= delta.Index {
					calls = append(calls, chatCall{})
				}
				call := &calls[delta.Index]
				call.ID += delta.ID
				call.Function.Name += delta.Function.Name
				call.Function.Arguments += delta.Function.Arguments
			}
		}
	}
	if err := reader.Err(); err != nil {
		return result, err
	}
	return result, errors.New("model stream closed before completion")
}

func completeChatCalls(result responseData, content string, calls []chatCall) (responseData, error) {
	if len(calls) == 0 {
		return result, work.ErrReadModelCall
	}
	if content != "" {
		item, _ := json.Marshal(chatOutputItem{Type: "message", Content: content})
		result.Output = append(result.Output, item)
	}
	seen := make(map[string]bool, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.ID) == "" || len(call.ID) > 256 || seen[call.ID] ||
			call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
			return result, work.ErrReadModelCall
		}
		seen[call.ID] = true
		item, _ := json.Marshal(chatOutputItem{
			Type: "function_call", CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments,
		})
		result.Output = append(result.Output, item)
	}
	return result, nil
}
