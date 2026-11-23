package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingDemoConversationChatEndpoints(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "demo-chat.sqlite"))
	defer a.close()
	if err := a.store.SeedDomainDemo(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/chat/conv-chat-1",
		"/api/v1/conversations/conv-chat-1/history",
		"/api/v1/conversations/conv-chat-1/events",
		"/api/v1/assistant/composer-options?conversation_id=conv-chat-1",
	} {
		response, err := a.ts.Client().Do(a.request(http.MethodGet, path, ""))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s: got %d, want 200", path, response.StatusCode)
		}
	}
	for _, path := range []string{
		"/api/v1/conversations/conv-not-present/history",
		"/api/v1/conversations/conv-not-present/events",
	} {
		response, err := a.ts.Client().Do(a.request(http.MethodGet, path, ""))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404 for an absent conversation", path, response.StatusCode)
		}
	}
}

func TestAssistantCommandExplainsMalformedArguments(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "assistant-command.sqlite"))
	defer a.close()
	for _, example := range []struct {
		name, text string
		parts      []string
	}{
		{"curly quote", `/助手 {"calls":[{"tool":"get_cluster_trends” }]}`, []string{"中文", "”", "英文双引号", "单次"}},
		{"single batch item", `/助手 {"calls":[{"tool":"get_cluster_trends"}]}`, []string{"2 至 6", "单次"}},
		{"other syntax", `/助手 {"tool":"get_cluster_trends",}`, []string{"JSON", "检查"}},
		{"quote in string", `/助手 {"tool":"list_tasks","body":{"text":"中文“内容”"},}`, []string{"JSON", "检查"}},
	} {
		t.Run(example.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"client_message_id": "malformed-test", "text": example.text})
			if err != nil {
				t.Fatal(err)
			}
			code, out := a.post("/api/v1/conversations", string(body))
			if code != http.StatusBadRequest || out["code"] != "invalid_assistant_arguments" {
				t.Fatalf("unexpected response: %d %+v", code, out)
			}
			message, _ := out["message"].(string)
			for _, part := range example.parts {
				if !strings.Contains(message, part) {
					t.Errorf("message %q does not explain %q", message, part)
				}
			}
			if example.name == "quote in string" && strings.Contains(message, "中文弯引号") {
				t.Errorf("valid quotation inside a JSON string was blamed: %q", message)
			}
		})
	}
	page, err := a.store.ConversationsPage(t.Context(), false, "")
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("malformed requests created a conversation: %+v %v", page, err)
	}
}
