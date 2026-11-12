package work

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClassificationReceiptSurvivesSubmissionRollbackAndChargesRun(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	limits := RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 80}
	s.configureRuntime(1, limits)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_submission BEFORE INSERT ON messages
BEGIN SELECT RAISE(ABORT,'submission write unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	var calls int
	input, output := int64(12), int64(7)
	classify := func(_ context.Context, _ string, _ []Message, limit int) (DelegationResult, error) {
		calls++
		if limit != 64 {
			t.Errorf("classification did not reserve reply budget: %d", limit)
		}
		time.Sleep(20 * time.Millisecond)
		return DelegationResult{Task: true, Requests: 1, Usage: &TokenUsage{InputTokens: &input, OutputTokens: &output}}, nil
	}
	if _, err := s.SubmitClassifiedChat(t.Context(), "classified-retry", "整理资料", "", "", Delivery{}, classify); err == nil {
		t.Fatal("fault injection did not reject submission")
	}
	if _, err := s.Submit(t.Context(), "classified-retry", "不同消息", "", "", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("durable admission key accepted other content: %v", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_submission"); err != nil {
		t.Fatal(err)
	}
	run, err := s.SubmitClassifiedChat(t.Context(), "classified-retry", "整理资料", "", "", Delivery{}, classify)
	if err != nil || run.TaskID == "" || calls != 1 || run.Usage == nil || run.Usage.ModelRequests != 1 || run.Usage.ActiveMS < 15 {
		t.Fatalf("retry lost classification receipt: %+v, calls=%d, %v", run, calls, err)
	}
	claimed, err := s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	answerInput, answerOutput := int64(8), int64(10)
	s.execute(t.Context(), testProvider(func(_ context.Context, _ []Message, _ bool, limit int, emit func(string) error) (*TokenUsage, error) {
		if limit != 73 {
			t.Errorf("classification was not charged against output budget: %d", limit)
		}
		return &TokenUsage{InputTokens: &answerInput, OutputTokens: &answerOutput}, emit("唯一成果")
	}), claimed, limits)
	detail, err := s.Detail(run.TaskID, true)
	if err != nil || detail.Run.Status != "completed" || detail.TaskUsage.ModelRequests != 2 || detail.TaskUsage.UnreportedRequests != 0 ||
		detail.TaskUsage.InputTokens == nil || *detail.TaskUsage.InputTokens != 20 || detail.TaskUsage.OutputTokens == nil || *detail.TaskUsage.OutputTokens != 17 {
		t.Fatalf("classification and execution accounting differ: %+v %v", detail, err)
	}
	var classifiedEvents int
	if err := s.db.QueryRow("SELECT count(*) FROM run_events WHERE run_id=? AND kind='submission.classified'", run.ID).Scan(&classifiedEvents); err != nil || classifiedEvents != 1 {
		t.Fatalf("classification event was duplicated or missing: %d %v", classifiedEvents, err)
	}
}

func TestClassificationFailuresAreExplicitAndNotReplayed(t *testing.T) {
	for _, scenario := range []string{"provider_error", "time_budget", "output_budget", "no_output_room", "unknown_receipt", "receipt_write_failure"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "work.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			s.configureRuntime(1, RunLimits{ActiveTime: 30 * time.Millisecond, ModelTimeout: time.Second, OutputTokens: 64})
			if scenario == "no_output_room" {
				s.configureRuntime(1, RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 16})
			}
			if scenario == "unknown_receipt" {
				if _, err := s.db.Exec(`INSERT INTO submission_classifications(request_key,fingerprint,conversation_id,state,requests,created_at)
VALUES(?,?,'','running',1,?)`, "failed-classification", submissionFingerprint("整理资料", "", "", false), now()); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			classify := func(ctx context.Context, _ string, _ []Message, limit int) (DelegationResult, error) {
				calls++
				switch scenario {
				case "provider_error":
					return DelegationResult{Requests: 1}, errors.New("模型返回HTTP 503")
				case "time_budget":
					<-ctx.Done()
					return DelegationResult{Requests: 1}, ctx.Err()
				case "output_budget":
					over := int64(limit + 1)
					return DelegationResult{Requests: 1, Task: true, Usage: &TokenUsage{OutputTokens: &over}}, nil
				case "receipt_write_failure":
					if _, err := s.db.Exec(`CREATE TRIGGER reject_classification_receipt BEFORE UPDATE ON submission_classifications
BEGIN SELECT RAISE(ABORT,'receipt storage unavailable'); END`); err != nil {
						t.Fatal(err)
					}
					return DelegationResult{Requests: 1, Task: true}, nil
				default:
					t.Error("uncertain classification request was replayed")
					return DelegationResult{}, nil
				}
			}
			for range 2 {
				run, err := s.SubmitClassifiedChat(t.Context(), "failed-classification", "整理资料", "", "", Delivery{}, classify)
				if !errors.Is(err, ErrClassification) || run.ID != "" {
					t.Fatalf("classification failure was disguised as an accepted chat: %+v %v", run, err)
				}
				wantUnconfirmed := scenario == "unknown_receipt" || scenario == "receipt_write_failure"
				if errors.Is(err, ErrClassificationUnconfirmed) != wantUnconfirmed {
					t.Fatalf("classification error has wrong certainty: unconfirmed=%t error=%v", wantUnconfirmed, err)
				}
			}
			want := 1
			if scenario == "unknown_receipt" || scenario == "no_output_room" {
				want = 0
			}
			var runs int
			if err := s.db.QueryRow("SELECT count(*) FROM runs").Scan(&runs); err != nil || runs != 0 || calls != want {
				t.Fatalf("failure left work or repeated classification: runs=%d calls=%d %v", runs, calls, err)
			}
		})
	}
}

func TestClassificationAndExecutionShareConcurrency(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	limits := RunLimits{ActiveTime: 2 * time.Second, ModelTimeout: time.Second, OutputTokens: 128}
	s.configureRuntime(1, limits)
	run, err := s.Submit(t.Context(), "execution", "普通执行", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	classified := make(chan error, 1)
	var active, maximum atomic.Int64
	enter := func() {
		current := active.Add(1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
	}
	go func() {
		_, err := s.SubmitClassifiedChat(t.Context(), "classification", "另一个输入", "", "", Delivery{}, func(ctx context.Context, _ string, _ []Message, _ int) (DelegationResult, error) {
			enter()
			defer active.Add(-1)
			close(started)
			select {
			case <-release:
				return DelegationResult{Task: false, Requests: 1}, nil
			case <-ctx.Done():
				return DelegationResult{Requests: 1}, ctx.Err()
			}
		})
		classified <- err
	}()
	<-started
	modelStarted := make(chan struct{})
	executed := make(chan struct{})
	go func() {
		s.execute(t.Context(), testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
			enter()
			defer active.Add(-1)
			close(modelStarted)
			return nil, emit("回复")
		}), run, limits)
		close(executed)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		t.Fatalf("classification held SQLite while waiting: %v", err)
	}
	select {
	case <-modelStarted:
		t.Error("execution bypassed classification concurrency limit")
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-classified; err != nil {
		t.Fatal(err)
	}
	select {
	case <-executed:
	case <-ctx.Done():
		t.Fatal("execution did not proceed when classification released capacity")
	}
	if maximum.Load() != 1 {
		t.Fatalf("concurrent provider calls = %d", maximum.Load())
	}
}

func TestClassificationCountDoesNotChangeSingleReadPhases(t *testing.T) {
	for _, modelDriven := range []bool{false, true} {
		t.Run(map[bool]string{false: "fixed_read", true: "model_read"}[modelDriven], func(t *testing.T) {
			s, read := taskReadFixture(t)
			run, err := s.SubmitWithDelivery(t.Context(), "read-after-classification", "读取资料", "", "", true, Delivery{Read: &read})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE runs SET usage_tracked=1,admission_requests=1,model_requests=1,admission_output_used=4 WHERE id=?", run.ID); err != nil {
				t.Fatal(err)
			}
			run, err = s.claim(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var provider Provider = testProvider(func(_ context.Context, input []Message, _ bool, limit int, emit func(string) error) (*TokenUsage, error) {
				if input[len(input)-1].Read == nil || limit != 124 {
					t.Errorf("fixed read failed after classification: input=%+v limit=%d", input, limit)
				}
				return nil, emit("已读取")
			})
			if modelDriven {
				provider = taskReadModelFixture{
					plan: func(context.Context, []Message, int) (TaskReadCall, *TokenUsage, error) {
						return TaskReadCall{Protocol: "responses/v1", CallID: "single-read", Output: json.RawMessage(`[]`)}, nil, nil
					},
					complete: func(_ context.Context, _ []Message, _ TaskReadCall, read *TaskRead, _ int, emit func(string) error) (*TokenUsage, error) {
						if read == nil || read.Status != "completed" {
							t.Errorf("model read result unavailable: %+v", read)
						}
						return nil, emit("已读取")
					},
				}
			}
			s.execute(t.Context(), provider, run, RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 128})
			detail, err := s.Detail(run.TaskID, true)
			want := 2
			if modelDriven {
				want = 3
			}
			if err != nil || detail.Run.Status != "completed" || detail.TaskUsage.ModelRequests != want || detail.Read == nil || detail.Read.Status != "completed" || !strings.Contains(detail.Run.Response, "已读取") {
				t.Fatalf("classification altered a tool phase: run=%+v read=%+v error=%v", detail.Run, detail.Read, err)
			}
		})
	}
}

type admissionCheckpointProvider struct{ testProvider }

func (p admissionCheckpointProvider) TextRunProvider(context.Context, bool) (Provider, string, error) {
	return p.testProvider, "classified-text-fixture", nil
}

func TestClassificationUnreportedUsageKeepsBudgetAcrossContinuation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	limits := RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 512}
	s.configureRuntime(1, limits)
	classifications := 0
	run, err := s.SubmitClassifiedChat(t.Context(), "unknown-classifier-usage", "整理资料", "", "", Delivery{}, func(context.Context, string, []Message, int) (DelegationResult, error) {
		classifications++
		return DelegationResult{Task: true, Requests: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	provider := admissionCheckpointProvider{testProvider: func(_ context.Context, _ []Message, _ bool, limit int, emit func(string) error) (*TokenUsage, error) {
		requests++
		if requests == 1 {
			if limit != 192 {
				t.Errorf("first answer lost reserved classification budget: %d", limit)
			}
			return nil, errors.New("stream interrupted before reported usage")
		}
		if limit != 64 {
			t.Errorf("continuation charged classification twice or forgot previous output: %d", limit)
		}
		input, output := int64(3), int64(4)
		return &TokenUsage{InputTokens: &input, OutputTokens: &output}, emit("已完成")
	}}
	s.execute(t.Context(), provider, run, limits)
	detail, err := s.Detail(run.TaskID, true)
	if err != nil || detail.ReadCheckpoint != "ready" || detail.TaskUsage.ModelRequests != 2 || detail.TaskUsage.UnreportedRequests != 2 {
		t.Fatalf("unreported admission use did not remain explicit: checkpoint=%s usage=%+v error=%v", detail.ReadCheckpoint, detail.TaskUsage, err)
	}
	if _, err := s.Resume(t.Context(), run.TaskID, "continue-classified-run", detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	run, err = s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.execute(t.Context(), provider, run, limits)
	detail, err = s.Detail(run.TaskID, true)
	if err != nil || detail.Run.Status != "completed" || detail.TaskUsage.ModelRequests != 3 || detail.TaskUsage.UnreportedRequests != 2 || classifications != 1 || requests != 2 ||
		detail.TaskUsage.OutputTokens == nil || *detail.TaskUsage.OutputTokens != 4 {
		t.Fatalf("classified continuation accounting changed: run=%+v usage=%+v classifier=%d model=%d error=%v", detail.Run, detail.TaskUsage, classifications, requests, err)
	}
}
