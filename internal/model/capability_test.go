package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResponsesCapabilityProbe(t *testing.T) {
	const call = `{"type":"function_call","id":"fc_sample","call_id":"call_sample","name":"clusbot_test_value","arguments":"{}","status":"completed"}`
	const reasoning = `{"type":"reasoning","id":"rs_sample","summary":[],"encrypted_content":"opaque-reasoning"}`
	for _, tc := range []struct {
		name     string
		requests int32
		failure  string
	}{
		{"complete", 3, ""},
		{"wrong-arguments", 2, "未收到一个有效的测试工具调用"},
		{"unfinished-call", 2, "未收到完整的工具调用"},
		{"unrelated-reply", 3, "模型未能根据工具结果回复测试码"},
		{"unfinished-reply", 3, "模型未能根据工具结果回复测试码"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				step := count.Add(1)
				var request struct {
					Input []json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode probe: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				send := func(event string) { fmt.Fprintf(w, "data: %s\n\n", event) }
				w.Header().Set("Content-Type", "text/event-stream")
				switch step {
				case 1:
					send(`{"type":"response.output_text.delta","delta":"OK"}`)
				case 2:
					callItem := call
					if tc.name == "wrong-arguments" {
						callItem = strings.Replace(callItem, `"{}"`, `"{\"unexpected\":1}"`, 1)
					}
					send(`{"type":"response.output_item.done","output_index":1,"item":` + callItem + "}")
					send(`{"type":"response.output_item.done","output_index":0,"item":` + reasoning + "}")
					if tc.name == "unfinished-call" {
						return
					}
				case 3:
					if len(request.Input) != 4 || string(request.Input[1]) != reasoning || string(request.Input[2]) != call {
						t.Errorf("completed items were not replayed in order: %s", request.Input)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					var output struct {
						Type   string `json:"type"`
						CallID string `json:"call_id"`
						Value  string `json:"output"`
					}
					if err := json.Unmarshal(request.Input[3], &output); err != nil || output.Type != "function_call_output" || output.CallID != "call_sample" || len(output.Value) < 26 {
						t.Errorf("invalid function call result: %+v, %v", output, err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					reply := output.Value
					if tc.name == "unrelated-reply" {
						reply = "OK"
					}
					send(fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, reply))
					if tc.name == "unfinished-reply" {
						return
					}
				default:
					t.Errorf("unexpected request %d", step)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				send(`{"type":"response.completed"}`)
			}))
			defer server.Close()
			provider, err := NewResponses(server.URL, "fixture", "")
			if err != nil {
				t.Fatal(err)
			}
			err = provider.testCapabilities(context.Background())
			if tc.failure == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrTestFailed) || !strings.Contains(err.Error(), tc.failure) {
				t.Fatalf("expected %q, got %v", tc.failure, err)
			}
			if count.Load() != tc.requests {
				t.Fatalf("sent %d requests, want %d", count.Load(), tc.requests)
			}
		})
	}
}
