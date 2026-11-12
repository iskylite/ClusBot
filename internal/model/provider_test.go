package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestResponsesRequestBudgetRetainsCurrentInputAndToolPairs(t *testing.T) {
	const limit = 4096
	const oldMaterial = "原选材料中的交付编号"
	const currentMaterial = "本次选中的补充材料"
	const currentRequest = "当前目标和验收条件"
	const functionCall = `{"type":"function_call","call_id":"read-one","name":"read_selected_source","arguments":"{}"}`
	const functionOutput = `{"type":"function_call_output","call_id":"read-one","output":"已确认的读取证据"}`
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) > limit || strings.Contains(string(body), "历史旧回复") {
			t.Errorf("unexpected request length or retained history: %d, %v", len(body), err)
		}
		var payload struct{ Input []json.RawMessage }
		if err = json.Unmarshal(body, &payload); err != nil || len(payload.Input) != 5 {
			t.Errorf("expected a whole recent turn, the current request and the tool pair: %s, %v", body, err)
			return
		}
		if !strings.Contains(string(payload.Input[0]), "上一轮问题") || !strings.Contains(string(payload.Input[1]), "上一轮结果") ||
			!strings.Contains(string(payload.Input[2]), currentRequest) || !strings.Contains(string(payload.Input[2]), currentMaterial) ||
			!strings.Contains(string(payload.Input[2]), oldMaterial) {
			t.Errorf("recent messages or selected materials were lost: %s", body)
		}
		if string(payload.Input[3]) != functionCall || string(payload.Input[4]) != functionOutput {
			t.Errorf("tool pair changed: %s", body)
		}
		io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	provider, err := NewResponses(server.URL+"/v1", "fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	provider.maxRequestBytes = limit
	request, err := conversationRequest([]work.Message{
		{Role: "user", Text: "较早请求", Materials: []work.Material{{Path: "原始材料.md", Content: oldMaterial}}},
		{Role: "assistant", Text: strings.Repeat("历史旧回复", 900)},
		{Role: "user", Text: "上一轮问题"},
		{Role: "assistant", Text: "上一轮结果"},
		{Role: "user", Text: currentRequest, Materials: []work.Material{{Path: "补充材料.md", Content: currentMaterial}}},
	}, true, 256)
	if err != nil {
		t.Fatal(err)
	}
	request.Input = append(request.Input, json.RawMessage(functionCall), json.RawMessage(functionOutput))
	if _, err = provider.requestStream(t.Context(), request, nil); err != nil || !called.Load() {
		t.Fatalf("bounded request did not reach the provider: %v", err)
	}
	if len(request.Input) != 7 || strings.Contains(request.Input[4].(responseMessage).Content, oldMaterial) {
		t.Fatal("compaction modified the reusable request")
	}
}

func TestResponsesRequestBudgetRejectsOversizeRequiredInputBeforeHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("oversized input was posted")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer server.Close()
	provider, err := NewResponses(server.URL+"/v1", "fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	provider.maxRequestBytes = 4096
	request, err := conversationRequest([]work.Message{
		{Role: "user", Text: "请整理这份记录", Materials: []work.Material{{Path: "选中的记录.md", Content: strings.Repeat("<", 900)}}},
	}, true, 128)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.requestStream(t.Context(), request, nil); !errors.Is(err, work.ErrModelRequestBudget) {
		t.Fatalf("expected an input budget error before transport, got %v", err)
	}
}

func TestResponsesAdapter(t *testing.T) {
	for _, tc := range []struct {
		name, terminal string
		input, output  int64
	}{
		{"completed", `{"type":"response.completed","response":{"usage":{"input_tokens":15,"output_tokens":6}}}`, 15, 6},
		{"output-limit", `{"type":"response.incomplete","response":{"usage":{"input_tokens":15,"output_tokens":64},"incomplete_details":{"reason":"max_output_tokens"}}}`, 15, 64},
		{"missing-usage", `{"type":"response.completed"}`, 0, 0},
		{"closed-before-completion", "", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("wrong Responses request")
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["stream"] != true || payload["store"] != false || payload["max_output_tokens"] != float64(64) {
					t.Errorf("wrong streaming payload: %v, %v", payload, err)
				}
				input, err := json.Marshal(payload["input"])
				if err != nil || !strings.Contains(string(input), "材料正文") || !strings.Contains(string(input), "fixture-digest") {
					t.Errorf("material snapshot missing from Responses input: %s, %v", input, err)
				}
				var messages []responseMessage
				if err := json.Unmarshal(input, &messages); err != nil || len(messages) != 3 ||
					messages[0].Type != "message" || messages[1].Type != "message" || messages[1].Role != "assistant" || messages[2].Type != "message" {
					t.Errorf("Responses input is missing an explicit message type: %s, %v", input, err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"收到\"}\n\n")
				if tc.terminal != "" {
					io.WriteString(w, "data: "+tc.terminal+"\n\n")
				}
			}))
			defer server.Close()
			provider, err := NewResponses(server.URL+"/v1", "fixture", "test-key")
			if err != nil {
				t.Fatal(err)
			}
			var text string
			messages := []work.Message{{
				Role: "user",
				Text: "你好",
				Materials: []work.Material{{
					Path: "materials/会议.md", Digest: "fixture-digest", Content: "材料正文",
				}},
			}, {Role: "assistant", Text: "之前的回答"}, {Role: "user", Text: "请继续"}}
			usage, err := provider.Stream(context.Background(), messages, false, 64, func(delta string) error {
				text += delta
				return nil
			})
			success := tc.name == "completed" || tc.name == "missing-usage"
			if (err == nil) != success || text != "收到" || errors.Is(err, work.ErrOutputBudget) != (tc.name == "output-limit") {
				t.Fatalf("stream result %q, %v", text, err)
			}
			if tc.input == 0 {
				if usage != nil {
					t.Fatalf("usage should be unknown: %+v", usage)
				}
			} else if usage == nil || usage.InputTokens == nil || usage.OutputTokens == nil || *usage.InputTokens != tc.input || *usage.OutputTokens != tc.output {
				t.Fatalf("wrong usage: %+v", usage)
			}
		})
	}
}

func TestModelContextFitsBothProtocols(t *testing.T) {
	for _, protocol := range []string{ProtocolChat, ProtocolResponses} {
		t.Run(protocol, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				text := string(body)
				for _, required := range []string{"CURRENT_REQUEST", "MATERIAL_CONTENT", "call_budget", "TOOL_RECEIPT", "较早对话节选"} {
					if !strings.Contains(text, required) {
						t.Errorf("missing %s", required)
					}
				}
				if strings.Contains(text, strings.Repeat("STALE ", 25)) {
					t.Error("long historical reply was not compacted")
				}
				if protocol == ProtocolChat {
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
				}
			}))
			defer server.Close()
			p, err := NewProvider(server.URL+"/v1", "fixture", "", protocol)
			if err != nil {
				t.Fatal(err)
			}
			window := 2400
			p.attributes.ContextTokens = &window
			request, err := conversationRequest([]work.Message{
				{ID: 1, Role: "user", Text: "earlier question"},
				{ID: 2, Role: "assistant", Text: strings.Repeat("STALE ", 1000)},
				{ID: 3, Role: "user", Text: "CURRENT_REQUEST", RequiredInput: true,
					Materials: []work.Material{{Path: "materials/source.md", Content: "MATERIAL_CONTENT"}}},
			}, true, 300)
			if err != nil {
				t.Fatal(err)
			}
			request.Input = append(request.Input,
				json.RawMessage(`{"type":"function_call","call_id":"call_budget","name":"read","arguments":"{}"}`),
				json.RawMessage(`{"type":"function_call_output","call_id":"call_budget","output":"TOOL_RECEIPT"}`))
			if _, err := p.requestStream(t.Context(), request, nil); err != nil {
				t.Fatal(err)
			}
			required := request.Input[2].(responseMessage)
			required.Content += strings.Repeat("REQUIRED ", 1000)
			request.Input[2] = required
			if _, err := p.requestStream(t.Context(), request, nil); !errors.Is(err, work.ErrModelRequestBudget) {
				t.Fatalf("expected required input to be rejected, got %v", err)
			}
			if calls != 1 {
				t.Fatalf("expected exactly one upstream request, got %d", calls)
			}
		})
	}
}

func TestDefaultContextCompactsWhenWindowUnknown(t *testing.T) {
	// Without a configured context window, compaction must still apply the
	// default 90% threshold rather than only the serialized byte limit.
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(body), strings.Repeat("ANCIENT ", 60)) {
			t.Errorf("early history survived without a configured window")
		}
		io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	p, err := NewResponses(server.URL+"/v1", "fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	// No p.attributes.ContextTokens: rely on the default window.
	if p.attributes.ContextTokens != nil {
		t.Fatal("precondition: window should be unknown")
	}
	huge := strings.Repeat("ANCIENT ", 40000) // ~280KB, far above DefaultContextTokens
	request, err := conversationRequest([]work.Message{
		{ID: 1, Role: "user", Text: "earlier"},
		{ID: 2, Role: "assistant", Text: huge},
		{ID: 3, Role: "user", Text: "CURRENT", RequiredInput: true},
	}, true, 256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.requestStream(t.Context(), request, nil); err != nil {
		t.Fatalf("default-window compaction should have delivered the request: %v", err)
	}
	if !called.Load() {
		t.Fatal("request never reached the provider")
	}
}

func TestMaxOutputCapAppliesToRequest(t *testing.T) {
	var sent int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			MaxOutputTokens int `json:"max_output_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		sent = payload.MaxOutputTokens
		io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	p, err := NewResponses(server.URL+"/v1", "fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	cap := 500
	p.attributes.MaxOutputTokens = &cap
	// Ask for more than the model's declared max output; it must be clamped.
	if _, err := p.Stream(t.Context(), []work.Message{{Role: "user", Text: "hi"}}, false, 4000, nil); err != nil {
		t.Fatal(err)
	}
	if sent != 500 {
		t.Fatalf("max_output_tokens sent = %d, want clamped to 500", sent)
	}
}

func TestRequestStreamRetriesTransientFailures(t *testing.T) {
	defer func(old time.Duration) { initialRetryDelay = old }(initialRetryDelay)
	initialRetryDelay = time.Millisecond
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	p, err := NewResponses(server.URL+"/v1", "fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stream(t.Context(), []work.Message{{Role: "user", Text: "hi"}}, false, 512, nil); err != nil {
		t.Fatalf("stream after transient 503 should succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry after 503)", got)
	}
}

func TestRequestStreamDoesNotRetryClientErrors(t *testing.T) {
	defer func(old time.Duration) { initialRetryDelay = old }(initialRetryDelay)
	initialRetryDelay = time.Millisecond
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	p, err := NewResponses(server.URL+"/v1", "fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stream(t.Context(), []work.Message{{Role: "user", Text: "hi"}}, false, 512, nil); err == nil {
		t.Fatal("401 must surface as an error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("attempts = %d, want 1 (401 is not retryable)", got)
	}
}

func TestRequestStreamExhaustsRetriesOnPersistentServerError(t *testing.T) {
	defer func(old time.Duration) { initialRetryDelay = old }(initialRetryDelay)
	initialRetryDelay = time.Millisecond
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	p, err := NewResponses(server.URL+"/v1", "fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stream(t.Context(), []work.Message{{Role: "user", Text: "hi"}}, false, 512, nil); err == nil {
		t.Fatal("persistent 502 must surface as an error")
	}
	if got := atomic.LoadInt32(&calls); got != maxSendAttempts {
		t.Fatalf("attempts = %d, want %d", got, maxSendAttempts)
	}
}
