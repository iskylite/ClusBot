package model

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clusbot/internal/work"
)

func TestChatCompletionsCapabilityRoundTrip(t *testing.T) {
	var requests int
	var conversation bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("wrong Chat Completions endpoint or authentication")
		}
		var input struct {
			Messages            []chatMessage
			Stream              bool
			MaxCompletionTokens int    `json:"max_completion_tokens"`
			ToolChoice          string `json:"tool_choice"`
			Tools               []struct {
				Type     string
				Function struct{ Name string }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if !input.Stream || input.MaxCompletionTokens == 0 {
			t.Error("missing streaming options or output budget")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		content := "OK"
		switch requests {
		case 1:
			if !conversation && (len(input.Messages) != 1 || input.Messages[0].Role != "user") {
				t.Error("missing user message")
			}
			if conversation && (len(input.Messages) != 2 || input.Messages[0].Role != "system" || input.Messages[1].Role != "user") {
				t.Error("missing system instructions or conversation message")
			}
		case 2:
			if len(input.Tools) != 1 || input.Tools[0].Type != "function" || input.Tools[0].Function.Name != probeTool || input.ToolChoice != "required" {
				t.Error("invalid Chat Completions tool definition")
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_fixture\",\"type\":\"function\",\"function\":{\"name\":\"clusbot_test_value\",\"arguments\":\"{\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		case 3:
			if len(input.Messages) != 3 {
				t.Error("missing tool history")
				return
			}
			assistant, tool := input.Messages[1], input.Messages[2]
			if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 ||
				assistant.ToolCalls[0].ID != "call_fixture" || assistant.ToolCalls[0].Function.Arguments != "{}" ||
				tool.Role != "tool" || tool.ToolCallID != "call_fixture" || tool.Content == nil || input.ToolChoice != "none" {
				t.Error("invalid replay of the assistant tool call and result")
				return
			}
			content, _ = tool.Content.(string)
		default:
			t.Error("unexpected model request")
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\n", content)
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	provider, err := NewProvider(server.URL+"/v1", "fixture", "fixture-key", ProtocolChat)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.testCapabilities(t.Context()); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	requests = 0
	conversation = true
	usage, err := provider.Stream(t.Context(), []work.Message{{Role: "user", Text: "test"}}, false, 128,
		func(delta string) error { text.WriteString(delta); return nil })
	if err != nil || text.String() != "OK" || usage == nil || usage.InputTokens == nil || *usage.InputTokens != 3 ||
		usage.OutputTokens == nil || *usage.OutputTokens != 2 {
		t.Fatalf("unexpected Chat Completions stream: %q, %+v, %v", text.String(), usage, err)
	}
}
