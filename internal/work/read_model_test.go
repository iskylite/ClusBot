package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
)

type taskReadModelFixture struct {
	testProvider
	plan     func(context.Context, []Message, int) (TaskReadCall, *TokenUsage, error)
	complete func(context.Context, []Message, TaskReadCall, *TaskRead, int, func(string) error) (*TokenUsage, error)
}

type checkpointModelFixture struct {
	testProvider
	turn func(context.Context, []Message, []TaskRead, []TaskReadRound, int, func(string) error) (TaskReadTurn, *TokenUsage, error)
}

func (p checkpointModelFixture) MultiTaskReadModel(context.Context) (MultiTaskReadModel, error) {
	return p, nil
}

func (p checkpointModelFixture) ReadCheckpointIdentity() (string, string) {
	return "checkpoint-fixture", "responses/v1"
}

func (p checkpointModelFixture) TaskReadTurn(ctx context.Context, input []Message, sources []TaskRead, history []TaskReadRound, limit int, emit func(string) error) (TaskReadTurn, *TokenUsage, error) {
	return p.turn(ctx, input, sources, history, limit, emit)
}

func TestMultiReadRecoveryKeepsCompletedAndUnknownResultsSeparate(t *testing.T) {
	ctx := context.Background()
	store, read := taskReadFixture(t)
	run, err := store.SubmitWithDelivery(ctx, "multi-read-recover", "核对多个状态", "", "", true, Delivery{Reads: []TaskReadRequest{read, read, read}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	limits := RunLimits{ActiveTime: 5 * time.Second, ModelTimeout: time.Second, OutputTokens: 64}
	input, err := store.modelMessages(ctx, run.ConversationID)
	if err != nil || len(input) != 1 {
		t.Fatalf("original input: %+v, %v", input, err)
	}
	if _, err = store.prepareReadCheckpoint(ctx, run, input, &input[0], input[0].Text, "checkpoint-fixture", limits, false); err != nil {
		t.Fatal(err)
	}
	if err = store.reserveReadRequest(ctx, run.ID, 16, 250*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	turn := TaskReadTurn{Protocol: "responses/v1", Output: json.RawMessage(`[]`), Calls: []TaskReadInvocation{
		{Position: 1, CallID: "call_1"}, {Position: 4, CallID: "out_of_range"},
	}}
	if err = store.saveReadRound(ctx, run.ID, 1, turn, nil); !errors.Is(err, ErrReadModelCall) {
		t.Fatalf("invalid batch unexpectedly saved: %v", err)
	}
	turn.Output = json.RawMessage(`[{"type":"function_call","name":"read_selected_source_1","call_id":"call_1","arguments":"{}"},{"type":"function_call","name":"read_selected_source_2","call_id":"call_2","arguments":"{}"},{"type":"function_call","name":"read_selected_source_3","call_id":"call_3","arguments":"{}"}]`)
	turn.Calls[1] = TaskReadInvocation{Position: 2, CallID: "call_2"}
	turn.Calls = append(turn.Calls, TaskReadInvocation{Position: 3, CallID: "call_3"})
	in, out := int64(16), int64(5)
	if err = store.saveReadRound(ctx, run.ID, 1, turn, &TokenUsage{InputTokens: &in, OutputTokens: &out}); err != nil {
		t.Fatalf("invalid batch left a partial checkpoint: %v", err)
	}
	first, err := store.runTaskReadAt(ctx, run.ID, 1)
	if err != nil || first.Status != "completed" || first.Result == nil {
		t.Fatalf("first read: %+v, %v", first, err)
	}
	if err = store.reserveReadRequest(ctx, run.ID, 16, 0); !errors.Is(err, ErrReadCheckpoint) {
		t.Fatalf("continued before all batch results were durable: %v", err)
	}
	if _, err = store.db.Exec("UPDATE run_reads SET status='checking' WHERE run_id=? AND position=2", run.ID); err != nil {
		t.Fatal(err)
	}
	tool := mcpclient.Tool{Name: read.ToolName, Version: read.ToolVersion}
	if err = store.dispatchConnectionCall(ctx, id(), read.ConnectionID, tool, read.Revision, run.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err = store.Recover(); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(run.TaskID, true)
	if err != nil || len(detail.Reads) != 3 || detail.UnknownReadRunID != run.ID || detail.Artifact != nil || detail.Run.Usage.ModelRequests != 1 {
		t.Fatalf("recovery detail: %+v, %v", detail, err)
	}
	if detail.Reads[0].Status != "completed" || detail.Reads[0].Result == nil || detail.Reads[0].Result.Text != first.Result.Text || detail.Reads[1].Status != "unknown" || detail.Reads[2].Status != "queued" {
		t.Fatalf("recovery lost source states: %+v", detail.Reads)
	}
	snapshot, err := store.Overview(ctx, OverviewFilter{})
	if err != nil || len(snapshot.Groups["waiting"]) != 1 || snapshot.Groups["waiting"][0].ID != run.TaskID || len(snapshot.Groups["active"]) != 0 {
		t.Fatalf("unconfirmed read is not exclusively in the decision group: %+v, %v", snapshot.Groups, err)
	}
	if _, err = store.Submit(ctx, "multi-read-blocked", "根据现有结果说明", "", run.ConversationID, true); !errors.Is(err, ErrReadUnconfirmed) {
		t.Fatalf("unknown source did not block the Task: %v", err)
	}
	if _, err = store.Resume(ctx, run.TaskID, "resume-unknown", detail.Task.Revision); !errors.Is(err, ErrReadUnconfirmed) {
		t.Fatalf("unknown source did not block resumption: %v", err)
	}
	if err = store.AcknowledgeRead(ctx, run.TaskID, run.ID, detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Overview(ctx, OverviewFilter{})
	if err != nil || len(snapshot.Groups["waiting"]) != 0 || len(snapshot.Groups["active"]) != 1 || snapshot.Groups["active"][0].ID != run.TaskID {
		t.Fatalf("acknowledged read did not return to the active group: %+v, %v", snapshot.Groups, err)
	}
	detail, err = store.Detail(run.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("UPDATE run_read_checkpoints SET version=99 WHERE run_id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Resume(ctx, run.TaskID, "resume-invalid", detail.Task.Revision); !errors.Is(err, ErrReadCheckpoint) {
		t.Fatalf("unrecognized checkpoint version accepted: %v", err)
	}
	if _, err = store.db.Exec("UPDATE run_read_checkpoints SET version=? WHERE run_id=?", readCheckpointVersion, run.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.Resume(ctx, run.TaskID, "resume-multi-read", detail.Task.Revision)
	if err != nil || resumed.ID != run.ID || resumed.Generation != 1 || resumed.Usage.ActiveMS < 250 || resumed.Usage.ModelRequests != 1 {
		t.Fatalf("wrong continuation identity or usage: %+v, %v", resumed, err)
	}
	retry, err := store.Resume(ctx, run.TaskID, "resume-multi-read", detail.Task.Revision)
	if err != nil || retry.ID != run.ID || retry.Generation != 1 {
		t.Fatalf("repeated resume changed generation: %+v, %v", retry, err)
	}
	resumed, err = store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.delta(ctx, run.ID, "过期输出"); !errors.Is(err, ErrState) {
		t.Fatalf("old generation appended output: %v", err)
	}
	if _, err = store.runTaskReadAt(ctx, run.ID, 3); !errors.Is(err, ErrState) {
		t.Fatalf("old generation started pending source: %v", err)
	}
	if err = store.finish(run, "completed", "", runAccounting{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old generation completed resumed Run: %v", err)
	}
	model := checkpointModelFixture{turn: func(_ context.Context, _ []Message, remaining []TaskRead, history []TaskReadRound, limit int, emit func(string) error) (TaskReadTurn, *TokenUsage, error) {
		if len(history) != 1 || len(history[0].Results) != 3 || history[0].Results[0].ReceivedAt != first.ReceivedAt || history[0].Results[1].Status != "unknown" {
			t.Fatalf("missing earlier model round: %+v", history)
		}
		if len(remaining) != 0 || history[0].Results[2].Status != "completed" || limit != 59 || emit == nil {
			t.Fatalf("unexpected answer checkpoint: %+v, %d", history, limit)
		}
		return TaskReadTurn{}, &TokenUsage{InputTokens: &in, OutputTokens: &out}, emit("已使用两项结果，第二项未取得")
	}}
	store.execute(ctx, model, resumed, limits)
	detail, err = store.Detail(run.TaskID, true)
	if err != nil || detail.Run.ID != run.ID || detail.Run.Status != "completed" || detail.Run.Usage.ModelRequests != 2 || detail.Artifact == nil || detail.Artifact.Content != "已使用两项结果，第二项未取得" {
		t.Fatalf("continuation did not deliver in original Run: %+v, %v", detail, err)
	}
	var calls int
	if err = store.db.QueryRow("SELECT count(*) FROM connection_calls").Scan(&calls); err != nil || calls != 3 {
		t.Fatalf("completed or unknown source repeated: %d calls, %v", calls, err)
	}
}

func (p taskReadModelFixture) TaskReadModel(context.Context) (TaskReadModel, error) { return p, nil }

func (p taskReadModelFixture) PlanTaskRead(ctx context.Context, input []Message, limit int) (TaskReadCall, *TokenUsage, error) {
	return p.plan(ctx, input, limit)
}

func (p taskReadModelFixture) CompleteTaskRead(ctx context.Context, input []Message, call TaskReadCall, read *TaskRead, limit int, emit func(string) error) (TaskAnswer, error) {
	usage, err := p.complete(ctx, input, call, read, limit, emit)
	return TaskAnswer{Usage: usage}, err
}

func TestNativeReadDoesNotContinueWithoutDurableEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, trigger, status         string
		reads, modelCalls, unreported int
	}{
		{
			name: "intent commit failure", status: "not_run", modelCalls: 1,
			trigger: `CREATE TRIGGER reject_intent BEFORE INSERT ON run_read_model_calls
BEGIN SELECT RAISE(ABORT,'intent write failed'); END`,
		},
		{
			name: "result commit failure", status: "unknown", reads: 1, modelCalls: 1,
			trigger: `CREATE TRIGGER reject_result BEFORE UPDATE OF status ON run_reads WHEN NEW.status='completed'
BEGIN SELECT RAISE(ABORT,'result write failed'); END`,
		},
		{name: "model disconnect after result", status: "completed", reads: 1, modelCalls: 2, unreported: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, read := taskReadFixture(t)
			run, err := store.SubmitWithDelivery(ctx, "native-read-task", "读取状态并整理", "", "", true, Delivery{Read: &read})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.claim(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.trigger != "" {
				if _, err = store.db.Exec(tc.trigger); err != nil {
					t.Fatal(err)
				}
			}
			plan := TaskReadCall{
				Protocol: "responses/v1", CallID: "call_fixture",
				Output: json.RawMessage(`[{"type":"function_call","call_id":"call_fixture","name":"read_selected_source","arguments":"{}"}]`),
			}
			plans, replies := 0, 0
			model := taskReadModelFixture{
				testProvider: func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
					if len(input) == 0 || input[len(input)-1].Read == nil || input[len(input)-1].Read.Status != "reused" {
						t.Fatal("recovery did not use the saved read")
					}
					return nil, emit("已根据原读取结果整理")
				},
				plan: func(_ context.Context, _ []Message, limit int) (TaskReadCall, *TokenUsage, error) {
					plans++
					if limit != 32 {
						t.Fatalf("plan limit = %d", limit)
					}
					in, out := int64(12), int64(5)
					return plan, &TokenUsage{InputTokens: &in, OutputTokens: &out}, nil
				},
				complete: func(ctx context.Context, _ []Message, call TaskReadCall, _ *TaskRead, limit int, _ func(string) error) (*TokenUsage, error) {
					replies++
					read, err := store.taskRead(ctx, run.ID)
					if err != nil || read == nil || read.Status != "completed" || read.Result == nil {
						t.Fatalf("model continuation started without a saved result: %+v, %v", read, err)
					}
					if call.CallID != plan.CallID || call.Protocol != plan.Protocol || string(call.Output) != string(plan.Output) || limit != 32 {
						t.Fatalf("unexpected replay: %+v, limit %d", call, limit)
					}
					return nil, io.ErrUnexpectedEOF
				},
			}
			limits := RunLimits{ActiveTime: 5 * time.Second, ModelTimeout: time.Second, OutputTokens: 64}
			store.execute(ctx, model, run, limits)
			detail, err := store.Detail(run.TaskID, true)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Read == nil || detail.Read.Status != tc.status || detail.Artifact != nil || plans != 1 || replies != tc.modelCalls-1 {
				t.Fatalf("read %+v, artifact %+v, requests %d + %d", detail.Read, detail.Artifact, plans, replies)
			}
			usage := detail.Run.Usage
			if usage.ModelRequests != tc.modelCalls || usage.UnreportedRequests != tc.unreported || usage.OutputTokens == nil || *usage.OutputTokens != 5 {
				t.Fatalf("unexpected usage: %+v", usage)
			}
			if tc.status == "completed" {
				if _, err = store.Resume(ctx, run.TaskID, "resume-native-read", detail.Task.Revision); err != nil {
					t.Fatal(err)
				}
				next, err := store.claim(ctx)
				if err != nil {
					t.Fatal(err)
				}
				store.execute(ctx, model, next, limits)
				reused, err := store.Detail(run.TaskID, true)
				if err != nil || reused.Read.Status != "reused" || reused.Artifact == nil || plans != 1 || replies != 1 {
					t.Fatalf("unexpected recovery: %+v, %v", reused, err)
				}
			}
			var calls int
			if err = store.db.QueryRow("SELECT count(*) FROM connection_calls").Scan(&calls); err != nil || calls != tc.reads {
				t.Fatalf("expected %d external calls, got %d: %v", tc.reads, calls, err)
			}
		})
	}
}
