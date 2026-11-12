package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkflowWaitsForAcceptanceAndUsesBoundEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := t.Context()
	plan := WorkflowRequest{Goal: "汇总本周事项", Criteria: "汇总核验后的结果", Scope: "周报", Steps: []WorkflowStep{
		{Key: "review", Goal: "核验条目", Criteria: "指出未经核验的条目", Depends: []string{"collect"}},
		{Key: "collect", Goal: "整理条目", Criteria: "列出来源"},
		{Key: "bonus", Goal: "补充背景", Criteria: "列出可选背景", Optional: true},
	}}
	graph, err := s.CreateWorkflow(ctx, "workflow-fixture", plan)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.CreateWorkflow(ctx, "workflow-fixture", plan)
	if err != nil || retry.ParentID != graph.ParentID {
		t.Fatalf("idempotent workflow: %+v, %v", retry, err)
	}
	lookup := func(task string) Detail {
		t.Helper()
		d, err := s.Detail(task, true)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	claim := func(task string) Run {
		t.Helper()
		run, err := s.claim(ctx)
		if err != nil || run.TaskID != task {
			t.Fatalf("claimed %+v, want task %s: %v", run, task, err)
		}
		return run
	}
	noClaim := func() {
		t.Helper()
		if run, err := s.claim(ctx); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("claimed a blocked run: %+v, %v", run, err)
		}
	}
	accept := func(task string) Artifact {
		t.Helper()
		d := lookup(task)
		if d.Artifact == nil {
			t.Fatal("missing artifact")
		}
		if err := s.Accept(ctx, task, d.Artifact.ID, d.Task.Revision); err != nil {
			t.Fatal(err)
		}
		return *d.Artifact
	}
	if err = s.Control(ctx, graph.Children[2].TaskID, "stop", lookup(graph.Children[2].TaskID).Task.Revision); err != nil {
		t.Fatal(err)
	}
	collect := claim(graph.Children[1].TaskID)
	if err = s.delta(ctx, collect.ID, "条目与来源"); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(collect, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	noClaim()
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
	noClaim()
	source := accept(collect.TaskID)
	review := claim(graph.Children[0].TaskID)
	limits := RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 64}
	s.execute(ctx, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		prompt := input[len(input)-1].Text
		if !strings.Contains(prompt, source.Digest) || !strings.Contains(prompt, source.Content) {
			t.Errorf("missing accepted prerequisite: %s", prompt)
		}
		return nil, emit("核验记录")
	}), review, limits)
	noClaim()
	accept(review.TaskID)
	parent := claim(graph.ParentID)
	evidence, err := s.workflowInputs(ctx, parent)
	if err != nil || len(evidence) != 3 || evidence[0].Key != "bonus" || !evidence[0].Skipped || evidence[1].ArtifactID != source.ID {
		t.Fatalf("bound aggregation inputs: %+v, %v", evidence, err)
	}
	if err = s.delta(ctx, parent.ID, "汇总成果"); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(parent, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	old := lookup(collect.TaskID)
	goal := "补上本周缺失的条目"
	if _, err = s.EditTask(ctx, old.Task.ID, TaskEdit{Goal: &goal, Revision: old.Task.Revision}); err != nil {
		t.Fatal(err)
	}
	final := lookup(parent.TaskID)
	if final.Artifact == nil {
		t.Fatal("missing parent artifact")
	}
	if err = s.Accept(ctx, parent.TaskID, final.Artifact.ID, final.Task.Revision); !errors.Is(err, ErrWorkflowEvidence) {
		t.Fatalf("stale aggregation was accepted: %v", err)
	}
	changed := lookup(collect.TaskID)
	if _, err = s.Resume(ctx, collect.TaskID, "rebuild-workflow-source", changed.Task.Revision); err != nil {
		t.Fatal(err)
	}
	revised := claim(collect.TaskID)
	if err = s.delta(ctx, revised.ID, "修订后的来源"); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(revised, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	accept(collect.TaskID)
	if _, err = s.Submit(ctx, "rerun-workflow-summary", "重新汇总", "", parent.ConversationID, false); err != nil {
		t.Fatal(err)
	}
	// The old review used the first source version. Re-accepting the source
	// alone cannot make that review or the parent eligible again.
	noClaim()
}

func TestWorkflowRejectsCyclesBeforeCreatingWork(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.CreateWorkflow(t.Context(), "workflow-cycle", WorkflowRequest{
		Goal: "父目标", Criteria: "父验收", Steps: []WorkflowStep{
			{Key: "a", Goal: "任务 A", Criteria: "验收 A", Depends: []string{"b"}},
			{Key: "b", Goal: "任务 B", Criteria: "验收 B", Depends: []string{"a"}},
		},
	})
	if !errors.Is(err, ErrWorkflow) {
		t.Fatalf("cycle accepted: %v", err)
	}
	if run, err := s.claim(t.Context()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invalid plan created work: %+v, %v", run, err)
	}
}
