package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type testProvider func(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error)

func (p testProvider) Stream(ctx context.Context, messages []Message, task bool, limit int, emit func(string) error) (*TokenUsage, error) {
	return p(ctx, messages, task, limit, emit)
}

func TestBudgetStopsIncompleteRunsWithoutPublishing(t *testing.T) {
	for _, tc := range []struct {
		code   string
		limits RunLimits
	}{
		{"run_time_budget", RunLimits{ActiveTime: 150 * time.Millisecond, ModelTimeout: time.Second, OutputTokens: 64}},
		{"model_timeout", RunLimits{ActiveTime: time.Second, ModelTimeout: 150 * time.Millisecond, OutputTokens: 64}},
		{"output_budget", RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 64}},
		{"model_input_budget", RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 64}},
	} {
		t.Run(tc.code, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(filepath.Join(t.TempDir(), "budget.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			created, err := s.Submit(ctx, "budget-task", "整理周报", "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.claim(ctx)
			if err != nil {
				t.Fatal(err)
			}
			input, output := int64(21), int64(64)
			s.execute(ctx, testProvider(func(ctx context.Context, _ []Message, _ bool, limit int, emit func(string) error) (*TokenUsage, error) {
				if limit != 64 {
					t.Errorf("output limit = %d", limit)
				}
				if tc.code == "model_input_budget" {
					return nil, ErrModelRequestBudget
				}
				if err := emit("部分结果"); err != nil {
					return nil, err
				}
				if tc.code == "output_budget" {
					return &TokenUsage{InputTokens: &input, OutputTokens: &output}, ErrOutputBudget
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}), run, tc.limits)
			d, err := s.Detail(created.TaskID, true)
			if err != nil {
				t.Fatal(err)
			}
			if d.Task.Status != "interrupted" || d.Run.Status != "interrupted" || d.Run.ErrorCode != tc.code || d.Artifact != nil || len(d.Messages) != 1 {
				t.Fatalf("incomplete result was treated as a deliverable: %+v %+v", d.Task, d.Run)
			}
			if d.Run.Usage == nil || d.Run.Usage.ModelRequests != 1 || d.TaskUsage.ModelRequests != 1 {
				t.Fatalf("missing run/task usage: %+v %+v", d.Run.Usage, d.TaskUsage)
			}
			if tc.code == "output_budget" {
				if d.TaskUsage.OutputTokens == nil || *d.TaskUsage.OutputTokens != 64 || d.TaskUsage.UnreportedRequests != 0 {
					t.Fatalf("lost terminal usage on budget exhaustion: %+v", d.TaskUsage)
				}
			} else if d.TaskUsage.OutputTokens != nil || d.TaskUsage.UnreportedRequests != 1 {
				t.Fatalf("unreported tokens represented incorrectly: %+v", d.TaskUsage)
			}
		})
	}
}

func TestRecoveryAndResumeKeepCumulativeUsage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "budget.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	created, err := s.Submit(ctx, "recovery-task", "核对会议纪要", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.prepareModelRequest(ctx, first.ID, 2200*time.Millisecond, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(first, "completed", "", runAccounting{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale worker advanced recovered run: %v", err)
	}
	recovered, err := s.Detail(created.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.TaskUsage.ModelRequests != 1 || recovered.TaskUsage.ActiveMS != 2200 || !recovered.TaskUsage.TimePartial {
		t.Fatalf("lost checkpoint: %+v", recovered.TaskUsage)
	}
	if _, err = s.Resume(ctx, created.TaskID, "resume-usage", recovered.Task.Revision); err != nil {
		t.Fatal(err)
	}
	second, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input, output := int64(36), int64(19)
	s.execute(ctx, testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		return &TokenUsage{InputTokens: &input, OutputTokens: &output}, emit("# 会议纪要")
	}), second, RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 64})
	d, err := s.Detail(created.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if d.Run.ID == first.ID || d.Task.Status != "waiting_user" || d.Artifact == nil || d.Run.Usage.ModelRequests != 1 ||
		d.TaskUsage.ModelRequests != 2 || d.TaskUsage.ActiveMS < 2200 || !d.TaskUsage.TimePartial || d.TaskUsage.UnreportedRequests != 1 ||
		d.TaskUsage.InputTokens == nil || *d.TaskUsage.InputTokens != 36 || d.TaskUsage.OutputTokens == nil || *d.TaskUsage.OutputTokens != 19 {
		t.Fatalf("usage after explicit retry: %+v, latest %+v", d.TaskUsage, d.Run)
	}
}
