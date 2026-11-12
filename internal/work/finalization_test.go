package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunOutcomePersistenceRecoversWithoutReplay(t *testing.T) {
	for _, action := range []string{"completed", "pause", "stop", "new_generation"} {
		t.Run(action, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "work.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			created, err := s.Submit(t.Context(), "outcome", "整理已读取的资料", "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.claim(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var calls int
			input, output := int64(17), int64(23)
			s.execute(t.Context(), testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
				calls++
				if err := emit("持久化后的唯一成果"); err != nil {
					return nil, err
				}
				_, err := s.db.Exec(`CREATE TRIGGER reject_outcome BEFORE UPDATE OF status ON runs
WHEN NEW.status<>'running' BEGIN SELECT RAISE(ABORT,'outcome write unavailable'); END`)
				return &TokenUsage{InputTokens: &input, OutputTokens: &output}, err
			}), run, RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 64})
			s.finalizationMu.Lock()
			pending := s.finalizations[runFinalizationKey{run.ID, run.Generation}]
			s.finalizationMu.Unlock()
			if pending == nil || pending.inFlight {
				t.Fatal("failed completion was not handed to the persistence reconciler")
			}
			before, err := s.Detail(created.TaskID, true)
			if err != nil || before.Run.Status != "running" || before.Artifact != nil || before.Run.Usage.ModelRequests != 1 || before.Run.Usage.OutputTokens != nil {
				t.Fatalf("failed transaction leaked an outcome: %+v %v", before, err)
			}
			if _, err := s.Submit(t.Context(), "queued-during-outage", "另一项工作", "", "", true); err != nil {
				t.Fatal(err)
			}
			if _, err := s.claim(t.Context()); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("pending outcomes did not stop new claims: %v", err)
			}
			if action == "pause" || action == "stop" {
				if err := s.Control(t.Context(), created.TaskID, action, before.Task.Revision); err != nil {
					t.Fatal(err)
				}
			}
			if action == "new_generation" {
				if _, err := s.db.Exec("UPDATE runs SET execution_generation=execution_generation+1 WHERE id=?", run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec("DROP TRIGGER reject_outcome"); err != nil {
				t.Fatal(err)
			}
			s.reconcileFinalizations(t.Context())
			s.reconcileFinalizations(t.Context())
			d, err := s.Detail(created.TaskID, true)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("persistence recovery replayed model work %d times", calls)
			}
			if action == "new_generation" {
				if d.Run.Status != "running" || d.Run.Generation != 1 || d.Artifact != nil || d.TaskUsage.OutputTokens != nil {
					t.Fatalf("stale outcome advanced a new generation: %+v", d)
				}
				return
			}
			wantStatus, wantArtifacts, wantMessages := "completed", 1, 2
			if action == "pause" {
				wantStatus, wantArtifacts, wantMessages = "interrupted", 0, 1
			} else if action == "stop" {
				wantStatus, wantArtifacts, wantMessages = "cancelled", 0, 1
			}
			if d.Run.Status != wantStatus || len(d.ArtifactVersions) != wantArtifacts || len(d.Messages) != wantMessages ||
				d.TaskUsage.ModelRequests != 1 || d.TaskUsage.UnreportedRequests != 0 ||
				d.TaskUsage.InputTokens == nil || *d.TaskUsage.InputTokens != input ||
				d.TaskUsage.OutputTokens == nil || *d.TaskUsage.OutputTokens != output || d.TaskUsage.ActiveMS != pending.accounting.activeMS {
				t.Fatalf("reconciled outcome or accounting differs: %+v", d)
			}
			if err := s.finish(run, "completed", "", pending.accounting); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("late completion was not rejected: %v", err)
			}
		})
	}
}

func TestRunOutcomeRecoveryLoopAndShutdown(t *testing.T) {
	for _, recoverLive := range []bool{true, false} {
		t.Run(map[bool]string{true: "live_recovery", false: "shutdown"}[recoverLive], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "work.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			run, err := s.Submit(t.Context(), "outcome-loop", "整理资料", "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`CREATE TRIGGER reject_outcome BEFORE UPDATE OF status ON runs
WHEN NEW.status<>'running' BEGIN SELECT RAISE(ABORT,'outcome write unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			workers := s.Start(ctx, testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
				calls.Add(1)
				return nil, emit("已生成的结果")
			}), 1, RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 64})
			deadline := time.After(3 * time.Second)
			for {
				s.finalizationMu.Lock()
				pending := s.finalizations[runFinalizationKey{run.ID, 0}]
				ready := pending != nil && !pending.inFlight
				s.finalizationMu.Unlock()
				if ready {
					break
				}
				select {
				case <-deadline:
					t.Fatal("worker kept a failed finish instead of releasing it")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if recoverLive {
				if _, err := s.db.Exec("DROP TRIGGER reject_outcome"); err != nil {
					t.Fatal(err)
				}
				for {
					d, err := s.Detail(run.TaskID, true)
					if err != nil {
						t.Fatal(err)
					}
					if d.Run.Status == "completed" {
						break
					}
					select {
					case <-deadline:
						t.Fatal("live persistence recovery did not finish the original Run")
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			cancel()
			stopped := make(chan struct{})
			go func() { workers.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("pending outcome prevented shutdown")
			}
			if calls.Load() != 1 {
				t.Fatalf("model was called %d times", calls.Load())
			}
			if !recoverLive {
				if _, err := s.db.Exec("DROP TRIGGER reject_outcome"); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Recover(); err != nil {
					t.Fatal(err)
				}
				d, err := s.Detail(run.TaskID, true)
				if err != nil || d.Run.Status != "completed" || d.Artifact == nil || d.Artifact.Content != "已生成的结果" {
					t.Fatalf("restart did not recover the persisted outcome: %+v %v", d, err)
				}
			}
		})
	}
}

func TestRunOutcomeRestartRecovery(t *testing.T) {
	for _, action := range []string{"completed", "pause", "stop", "new_generation", "question", "model_error", "receipt_unavailable"} {
		t.Run(action, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "work.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			created, err := s.Submit(t.Context(), "restart", "整理资料", "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.claim(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.prepareModelRequest(t.Context(), run.ID, 0, "整理资料"); err != nil {
				t.Fatal(err)
			}
			if err = s.delta(t.Context(), run.ID, "已保存的正文"); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(`CREATE TRIGGER reject_outcome BEFORE UPDATE OF status ON runs
WHEN NEW.status<>'running' BEGIN SELECT RAISE(ABORT,'outcome unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			if action == "receipt_unavailable" {
				if _, err = s.db.Exec(`CREATE TRIGGER reject_receipt BEFORE INSERT ON run_finalizations
BEGIN SELECT RAISE(ABORT,'receipt unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			input, output := int64(17), int64(23)
			accounting := runAccounting{tokens: &TokenUsage{InputTokens: &input, OutputTokens: &output}, activeMS: 123}
			status, failure := "completed", ""
			if action == "question" {
				accounting.question = "需要哪一周的资料？"
			} else if action == "model_error" {
				status, failure, accounting.errorCode = "interrupted", "未取得完整回复", "model_error"
			}
			s.finalizeRun(run, status, failure, accounting)
			if action == "pause" || action == "stop" {
				d, err := s.Detail(created.TaskID, true)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Control(t.Context(), created.TaskID, action, d.Task.Revision); err != nil {
					t.Fatal(err)
				}
			} else if action == "new_generation" {
				if _, err = s.db.Exec("UPDATE runs SET execution_generation=execution_generation+1 WHERE id=?", run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if action != "new_generation" && action != "receipt_unavailable" {
				if err = s.Recover(); err == nil {
					t.Fatal("startup must fail while a durable outcome cannot be finalized")
				}
				var count int
				if err = s.db.QueryRow("SELECT count(*) FROM run_finalizations").Scan(&count); err != nil || count != 1 {
					t.Fatalf("failed recovery lost its receipt: %d, %v", count, err)
				}
			}
			if _, err = s.db.Exec("DROP TRIGGER reject_outcome"); err != nil {
				t.Fatal(err)
			}
			if action == "receipt_unavailable" {
				if _, err = s.db.Exec("DROP TRIGGER reject_receipt"); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err = s.Recover(); err != nil {
					t.Fatal(err)
				}
			}
			d, err := s.Detail(created.TaskID, true)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantArtifacts, wantMessages := "completed", 1, 2
			switch action {
			case "pause", "new_generation", "model_error", "receipt_unavailable":
				wantStatus, wantArtifacts, wantMessages = "interrupted", 0, 1
			case "stop":
				wantStatus, wantArtifacts, wantMessages = "cancelled", 0, 1
			case "question":
				wantStatus, wantArtifacts, wantMessages = "input_required", 0, 2
			}
			if d.Run.Status != wantStatus || len(d.ArtifactVersions) != wantArtifacts || len(d.Messages) != wantMessages {
				t.Fatalf("incorrect recovered outcome: %+v", d)
			}
			if action == "new_generation" || action == "receipt_unavailable" {
				if d.TaskUsage.OutputTokens != nil || d.Run.ErrorCode != "process_interrupted" {
					t.Fatalf("unconfirmed/stale receipt advanced execution: %+v", d)
				}
			} else if d.TaskUsage.ModelRequests != 1 || d.TaskUsage.UnreportedRequests != 0 ||
				d.TaskUsage.InputTokens == nil || *d.TaskUsage.InputTokens != input ||
				d.TaskUsage.OutputTokens == nil || *d.TaskUsage.OutputTokens != output || d.TaskUsage.ActiveMS != 123 {
				t.Fatalf("usage was lost or counted twice: %+v", d.TaskUsage)
			}
			if action == "question" && d.Run.Response != accounting.question {
				t.Fatalf("question lost: %+v", d.Run)
			}
			if action == "model_error" && d.Run.ErrorCode != "model_error" {
				t.Fatalf("error code lost: %+v", d.Run)
			}
			if d.Artifact != nil && d.Artifact.Content != "已保存的正文" {
				t.Fatalf("response lost: %+v", d.Artifact)
			}
			var count int
			if err = s.db.QueryRow("SELECT count(*) FROM run_finalizations").Scan(&count); err != nil || count != 0 {
				t.Fatalf("receipt not settled: %d, %v", count, err)
			}
			if _, err = s.claim(t.Context()); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("recovery queued work for replay: %v", err)
			}
		})
	}
}
