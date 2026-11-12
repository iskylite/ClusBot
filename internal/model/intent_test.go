package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/work"
)

func classificationSettings(t *testing.T, endpoint string) (*work.Store, *Settings) {
	t.Helper()
	store, err := work.Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	settings := &Settings{Store: store}
	if _, err := settings.Initialize(t.Context(), work.ModelSettings{BaseURL: endpoint, ModelID: "fixture", KeyRef: "env:CLUSBOT_INTENT_TEST_KEY"}); err != nil {
		t.Fatal(err)
	}
	return store, settings
}

func TestConcurrentClassificationMakesOneHTTPCallAndChargesTask(t *testing.T) {
	var classifications, answers atomic.Int64
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request responseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode classification request: %v", err)
			return
		}
		text, input, output := "最终成果", 13, 6
		if request.Instructions == delegationInstructions {
			classifications.Add(1)
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			text, input, output = "TASK", 11, 4
		} else {
			answers.Add(1)
			if request.MaxOutputTokens > 60 {
				t.Errorf("classification output was not charged: answer limit=%d", request.MaxOutputTokens)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", text)
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d}}}\n\n", input, output)
	}))
	defer server.Close()
	store, settings := classificationSettings(t, server.URL)
	type receipt struct {
		run work.Run
		err error
	}
	results := make(chan receipt, 8)
	for range 8 {
		go func() {
			run, err := store.SubmitClassifiedChat(t.Context(), "one-classification", "请整理资料", "", "", work.Delivery{}, settings.ClassifyDelegation)
			results <- receipt{run, err}
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("classification HTTP request did not start")
	}
	close(release)
	var run work.Run
	for range 8 {
		result := <-results
		if result.err != nil || result.run.TaskID == "" || (run.ID != "" && result.run.ID != run.ID) {
			t.Fatalf("duplicate input changed receipt: %+v", result)
		}
		run = result.run
	}
	if classifications.Load() != 1 {
		t.Fatalf("same-key input sent %d classification requests", classifications.Load())
	}
	before, err := store.Detail(run.TaskID, true)
	if err != nil || before.TaskUsage.ModelRequests != 1 || before.TaskUsage.InputTokens == nil || *before.TaskUsage.InputTokens != 11 {
		t.Fatalf("classification usage was absent before execution: %+v %v", before.TaskUsage, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	workers := store.Start(ctx, settings, 1, work.RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 64})
	defer func() { cancel(); workers.Wait() }()
	deadline := time.After(3 * time.Second)
	for {
		detail, err := store.Detail(run.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Run.Status == "completed" {
			if detail.TaskUsage.ModelRequests != 2 || detail.TaskUsage.UnreportedRequests != 0 || detail.TaskUsage.InputTokens == nil || *detail.TaskUsage.InputTokens != 24 ||
				detail.TaskUsage.OutputTokens == nil || *detail.TaskUsage.OutputTokens != 10 || classifications.Load() != 1 || answers.Load() != 1 {
				t.Fatalf("combined actual usage is incorrect: %+v classifier=%d answer=%d", detail.TaskUsage, classifications.Load(), answers.Load())
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("classified run did not complete: %+v", detail.Run)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestClassificationModelFailureDoesNotBecomeChat(t *testing.T) {
	for _, scenario := range []string{"invalid_label", "http_error", "input_budget"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if scenario == "http_error" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"MAYBE\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
			}))
			defer server.Close()
			store, settings := classificationSettings(t, server.URL)
			text := "如何整理资料"
			if scenario == "input_budget" {
				settings.MaxRequestBytes = 1024
				text = strings.Repeat("资料", 1024)
			}
			for range 2 {
				run, err := store.SubmitClassifiedChat(t.Context(), "failed-label", text, "", "", work.Delivery{}, settings.ClassifyDelegation)
				if !errors.Is(err, work.ErrClassification) || run.ID != "" {
					t.Fatalf("failed classification was accepted as CHAT: %+v %v", run, err)
				}
			}
			want := int64(1)
			if scenario == "input_budget" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("classifier sent %d HTTP requests, want %d", calls.Load(), want)
			}
		})
	}
}

func TestV2RouterAndWorkerPersistContractAndContext(t *testing.T) {
	var routes, answers atomic.Int64
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request responseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			return
		}
		answer := "报告正文"
		if request.Instructions == routerInstructions {
			routes.Add(1)
			answer = `{"kind":"new_task","goal":"生成项目周报","criteria":"先写结论"}`
		} else {
			answers.Add(1)
			body, _ := json.Marshal(request.Input)
			if !strings.Contains(string(body), "生成项目周报") || !strings.Contains(string(body), "先写结论") {
				t.Errorf("worker did not receive the routed contract: %s", body)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", answer)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n")
	}))
	defer modelServer.Close()
	store, settings := classificationSettings(t, modelServer.URL)
	run, err := store.SubmitRoutedChat(t.Context(), "route-worker-v2", "帮我把情况整理一下", "项目 A", "", work.Delivery{}, settings.RouteSubmission)
	if err != nil || run.TaskID == "" {
		t.Fatalf("routing did not produce a task: %+v, %v", run, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	workers := store.Start(ctx, settings, 1, work.RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 128})
	defer func() { cancel(); workers.Wait() }()
	for {
		record, err := store.RunRecord(ctx, run.ID, run.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if record.Status == "completed" {
			if routes.Load() != 1 || answers.Load() != 1 || len(record.Contexts) != 1 ||
				record.Contexts[0].TaskRevision == nil || record.Contexts[0].AssignmentGeneration == nil {
				t.Fatalf("wrong router, worker or execution record: %+v", record)
			}
			var manifest work.ManifestRequest
			err = json.Unmarshal(record.Contexts[0].Manifest, &manifest)
			if err != nil || len(manifest.Messages) != 1 || len(manifest.Digest) != 64 || manifest.Messages[0] != record.Messages[0].ID {
				t.Fatalf("actual context not recorded: %+v, %v", manifest, err)
			}
			break
		}
		if record.Status != "queued" && record.Status != "running" {
			t.Fatalf("worker ended unexpectedly: %+v", record.Run)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestResponsesRouterAcceptsIndependentIntents(t *testing.T) {
	var target, before, after work.Run
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request responseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("route request: %v", err)
			return
		}
		if request.Instructions != routerInstructions || request.MaxOutputTokens < 512 {
			t.Errorf("unexpected router prompt or budget: %d", request.MaxOutputTokens)
		}
		input, _ := json.Marshal(request.Input)
		if !strings.Contains(string(input), target.TaskID) || !strings.Contains(string(input), "normal") {
			t.Errorf("router context omitted the target or its priority: %s", input)
		}
		answer := fmt.Sprintf(`{"kind":"batch","intents":[{"kind":"new_task","fragment":"写周报","goal":"写项目周报"},{"kind":"clarify","fragment":"也处理那件事","question":"你指的是哪件事？"},{"kind":"set_goal","task_id":%q,"fragment":"纪要改成整理会议决定","goal":"整理会议决定"},{"kind":"set_priority","task_id":%q,"fragment":"纪要设为高优先级","priority":"high"},{"kind":"order_tasks","task_id":%q,"after_task_id":%q,"fragment":"先核对预算再整理账单"}]}`,
			target.TaskID, target.TaskID, before.TaskID, after.TaskID)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", answer)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":90}}}\n\n")
	}))
	defer server.Close()
	store, settings := classificationSettings(t, server.URL)
	var err error
	target, err = store.Submit(t.Context(), "router-edit-existing", "写会议纪要", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	before, err = store.Submit(t.Context(), "router-queue-order-before", "核对预算", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	after, err = store.Submit(t.Context(), "router-queue-order-after", "整理账单", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.SubmitRoutedChat(t.Context(), "router-batch-provider", "写周报；也处理那件事；纪要改成整理会议决定；纪要设为高优先级；先核对预算再整理账单", "", "", work.Delivery{}, settings.RouteSubmission)
	if err != nil || source.RouterIntents == nil || len(*source.RouterIntents) != 5 || source.Usage == nil || source.Usage.ModelRequests != 1 {
		t.Fatalf("route result: %+v, %v", source, err)
	}
	items := *source.RouterIntents
	if items[0].Status != "applied" || items[1].Status != "clarify" || items[1].Detail != "你指的是哪件事？" {
		t.Fatalf("wrong split: %+v", items)
	}
	child, err := store.Detail(items[0].TaskID, true)
	if err != nil || child.Task.Goal != "写项目周报" || child.Run.Usage != nil {
		t.Fatalf("routing usage or task leaked across runs: %+v, %v", child, err)
	}
	edited, err := store.Detail(target.TaskID, true)
	if err != nil || items[2].Status != "applied" || items[3].Status != "applied" ||
		edited.Task.Goal != "整理会议决定" || edited.Task.Priority != "high" || !edited.Task.RequirementsChanged {
		t.Fatalf("model edits did not reach the existing task: %+v, intents=%+v, %v", edited, items, err)
	}
	if items[4].Status != "applied" || items[4].TaskID != before.TaskID || items[4].AfterTaskID != after.TaskID {
		t.Fatalf("model relative order did not reach both queued tasks: %+v", items[4])
	}
}
