package work

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type automaticResumeProvider struct{ limits []int }

func (p *automaticResumeProvider) Stream(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error) {
	panic("expected automatic tool provider")
}
func (p *automaticResumeProvider) TextRunProvider(context.Context, bool) (Provider, string, error) {
	return p, "automatic-budget-fixture", nil
}
func (p *automaticResumeProvider) TaskToolTurn(_ context.Context, _ []Message, tools []TaskToolDefinition, _ []TaskReadRound, limit int, emit func(string) error) (TaskToolTurn, error) {
	p.limits = append(p.limits, limit)
	if len(tools) == 0 {
		return TaskToolTurn{}, ErrState
	}
	if len(p.limits) == 1 {
		if err := emit("未完成片段"); err != nil {
			return TaskToolTurn{}, err
		}
		return TaskToolTurn{}, io.ErrUnexpectedEOF
	}
	input, output := int64(20), int64(21)
	return TaskToolTurn{TaskAnswer: TaskAnswer{Usage: &TokenUsage{InputTokens: &input, OutputTokens: &output}}}, emit("完整成果")
}

func TestAutomaticToolsResumeUsesRemainingReservedBudget(t *testing.T) {
	base := t.TempDir()
	s, err := Open(filepath.Join(base, "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.OpenWorkspace(filepath.Join(base, "workspace")); err != nil {
		t.Fatal(err)
	}
	run, err := s.Submit(t.Context(), "automatic-resume-budget", "整理工作区资料", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	provider := &automaticResumeProvider{}
	limits := RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128}
	s.execute(t.Context(), provider, run, limits)
	d, err := s.Detail(run.TaskID, true)
	if err != nil || d.Run.Status != "interrupted" || d.ReadCheckpoint != "ready" {
		t.Fatalf("initial interruption: %+v %v", d, err)
	}
	resumed, err := s.Resume(t.Context(), run.TaskID, "resume-original-budget", d.Task.Revision)
	if err != nil || resumed.ID != run.ID || resumed.Generation != 1 {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	resumed, err = s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.execute(t.Context(), provider, resumed, limits)
	d, err = s.Detail(run.TaskID, true)
	if err != nil || d.Run.Status != "completed" || d.Artifact == nil || d.Artifact.Content != "完整成果" || !reflect.DeepEqual(provider.limits, []int{96, 32}) {
		t.Fatalf("continuation reserved budget twice: limits=%v detail=%+v err=%v", provider.limits, d, err)
	}
	if d.Run.Usage.ModelRequests != 2 || d.Run.Usage.UnreportedRequests != 1 {
		t.Fatalf("continuation changed request accounting: %+v", d.Run.Usage)
	}
}
