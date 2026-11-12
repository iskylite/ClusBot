package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestInputRequestRequiresOneCompletedQuestion(t *testing.T) {
	call := func(name, status, arguments string) json.RawMessage {
		item, err := json.Marshal(map[string]string{
			"type": "function_call", "call_id": "ask-1", "name": name, "status": status, "arguments": arguments,
		})
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	valid := call(inputRequestFunction, "completed", `{"question":" 核对哪个日期？ "}`)
	answer, err := inputRequest([]json.RawMessage{json.RawMessage(`{"type":"reasoning"}`), valid})
	if err != nil || answer != "核对哪个日期？" {
		t.Fatalf("valid question: %q, %v", answer, err)
	}
	for name, output := range map[string][]json.RawMessage{
		"two calls":  {valid, valid},
		"extra key":  {call(inputRequestFunction, "completed", `{"question":"需要日期？","extra":true}`)},
		"wrong name": {call(taskReadFunction, "completed", `{"question":"需要日期？"}`)},
		"unfinished": {call(inputRequestFunction, "in_progress", `{"question":"需要日期？"}`)},
		"blank":      {call(inputRequestFunction, "completed", `{"question":" "}`)},
		"overlong":   {call(inputRequestFunction, "completed", `{"question":"`+strings.Repeat("字", 601)+`"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if question, err := inputRequest(output); question != "" || !errors.Is(err, work.ErrTaskAnswer) {
				t.Fatalf("invalid question accepted: %q, %v", question, err)
			}
		})
	}
}

func TestResponsesWorkerPlanStartsWorkflow(t *testing.T) {
	var planned atomic.Int64
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request responseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("model request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tool := range request.Tools {
			if tool.Name != workerPlanFunction {
				continue
			}
			planned.Add(1)
			args := `{"steps":[{"key":"collect","goal":"整理资料","acceptance_criteria":"列出来源","depends_on":[],"optional":false},{"key":"review","goal":"核对资料","acceptance_criteria":"列出差异","depends_on":["collect"],"optional":false}]}`
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"name\":%q,\"call_id\":\"plan-1\",\"status\":\"completed\",\"arguments\":%q}],\"usage\":{\"input_tokens\":20,\"output_tokens\":60}}}\n\n", workerPlanFunction, args)
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"资料与来源\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n")
	}))
	defer model.Close()
	store, settings := classificationSettings(t, model.URL)
	run, err := store.Submit(t.Context(), "worker-plan-e2e", "先整理再独立核对资料，最后汇总", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	workers := store.Start(ctx, settings, 1, work.RunLimits{ActiveTime: 10 * time.Second, ModelTimeout: time.Second, OutputTokens: 1200})
	defer func() { cancel(); workers.Wait() }()
	for {
		graph, err := store.Workflow(ctx, run.TaskID)
		if err == nil {
			if len(graph.Children) != 2 || len(graph.Children[1].Depends) != 1 || graph.Children[1].Depends[0] != "collect" {
				t.Fatalf("invalid graph: %+v", graph)
			}
			child, err := store.Detail(graph.Children[0].TaskID, true)
			if err != nil {
				t.Fatal(err)
			}
			if child.Artifact != nil {
				record, err := store.RunRecord(ctx, run.ID, run.TaskID)
				if err != nil || record.Status != "completed" || len(record.Artifacts) != 0 || planned.Load() != 1 {
					t.Fatalf("planning should produce one graph and no parent artifact: %+v, calls=%d, %v", record, planned.Load(), err)
				}
				return
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("planning or child did not finish: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
