package work

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMaterialSnapshotSurvivesChangesAndRestart(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "work.db")
	root := filepath.Join(base, "workspace")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err = s.OpenWorkspace(root); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path := filepath.Join(root, "materials", "会议纪要.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("首版：下周评审")
	list, err := s.MaterialFiles("纪要")
	if err != nil || len(list) != 1 || list[0].Path != "materials/会议纪要.md" {
		t.Fatalf("list: %+v %v", list, err)
	}
	m, err := s.CaptureMaterial(ctx, list[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	write("新版：日期待定")
	newer, err := s.CaptureMaterial(ctx, list[0].Path)
	if err != nil || newer.ID == m.ID || newer.Digest == m.Digest {
		t.Fatalf("version: %+v %v", newer, err)
	}
	r, err := s.Submit(ctx, "material-task", "整理材料", "", "", true, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(ctx, "material-task", "整理材料", "", "", true, newer.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed material did not conflict: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.modelMessages(ctx, r.ConversationID)
	if err != nil || len(input) != 1 || len(input[0].Materials) != 1 || input[0].Materials[0].Content != "首版：下周评审" {
		t.Fatalf("run did not use original snapshot: %+v %v", input, err)
	}
	d, err := s.Detail(r.TaskID, true)
	if err != nil || len(d.Messages) != 1 || len(d.Messages[0].Materials) != 1 {
		t.Fatalf("detail: %+v %v", d, err)
	}
	ref := d.Messages[0].Materials[0]
	if ref.Digest != m.Digest || ref.Content != "" || ref.Size != len("首版：下周评审") {
		t.Fatalf("wrong detail provenance: %+v", ref)
	}
}

func TestMaterialSelectionRejectsOutsideFilesAndRollsBackInvalidReference(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := t.TempDir()
	if err = s.OpenWorkspace(root); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err = os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, link := range map[string]func(string, string) error{"symlink.md": os.Symlink, "hardlink.md": os.Link} {
		if err = link(outside, filepath.Join(root, "materials", name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"materials/../outside.md", "materials/symlink.md", "materials/hardlink.md", outside} {
		if _, err = s.CaptureMaterial(context.Background(), path); !errors.Is(err, ErrMaterial) {
			t.Fatalf("%s: %v", path, err)
		}
	}
	list, err := s.MaterialFiles("")
	if err != nil || len(list) != 0 {
		t.Fatalf("links visible: %+v %v", list, err)
	}
	if _, err = s.Submit(context.Background(), "invalid-ref", "工作", "", "", true, "unknown-material"); !errors.Is(err, ErrMaterialSelection) {
		t.Fatalf("invalid reference: %v", err)
	}
	tasks, err := s.ListTasks(context.Background(), TaskFilter{})
	if err != nil || len(tasks) != 0 {
		t.Fatalf("invalid attachment left task: %+v %v", tasks, err)
	}
}

func TestTaskMaterialReplacementPreservesHistoryAndAppliesToLaterRuns(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	save := func(path, content string) Material {
		t.Helper()
		material, err := saveMaterial(ctx, s.db, path, []byte(content), nil)
		if err != nil {
			t.Fatal(err)
		}
		return material
	}
	raw := save("materials/raw.md", "original content")
	addendum := save("materials/addendum.md", "addendum content")
	summary := save("materials/summary.md", "summary content")
	r, err := s.Submit(ctx, "first-task", "Summarize", "", "", true, raw.ID)
	if err != nil {
		t.Fatal(err)
	}
	finish := func(want Run) {
		t.Helper()
		run, err := s.claim(ctx)
		if err != nil || run.ID != want.ID {
			t.Fatalf("claim %s: %+v %v", want.ID, run, err)
		}
		if err := s.finish(run, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
	}
	assertMaterials := func(want map[string]string) {
		t.Helper()
		input, err := s.modelMessages(ctx, r.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]string)
		for _, msg := range input {
			for _, material := range msg.Materials {
				if material.Content != "" {
					got[material.ID] = material.Content
				}
			}
		}
		if !maps.Equal(got, want) {
			t.Fatalf("material input = %v, want %v", got, want)
		}
	}
	submit := func(key string, replace bool, ids ...string) Run {
		t.Helper()
		run, err := s.SubmitWithDelivery(ctx, key, "Refine summary", "", r.ConversationID, false, Delivery{ReplaceMaterials: replace}, ids...)
		if err != nil || run.TaskID != r.TaskID {
			t.Fatalf("follow-up %s: %+v %v", key, run, err)
		}
		return run
	}
	finish(r)
	appended := submit("add-material", false, addendum.ID)
	assertMaterials(map[string]string{raw.ID: "original content", addendum.ID: "addendum content"})
	finish(appended)

	replaced := submit("replace-material", true, summary.ID)
	if duplicate := submit("replace-material", true, summary.ID); duplicate.ID != replaced.ID {
		t.Fatalf("duplicate request started a new Run: %+v", duplicate)
	}
	if _, err := s.Submit(ctx, "replace-material", "Refine summary", "", r.ConversationID, false, summary.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replacement choice did not conflict: %v", err)
	}
	assertMaterials(map[string]string{summary.ID: "summary content"})
	detail, err := s.Detail(r.TaskID, true)
	if err != nil || len(detail.Messages) != 5 {
		t.Fatalf("Task detail: %+v %v", detail, err)
	}
	first, latest := detail.Messages[0], detail.Messages[4]
	if len(first.Materials) != 1 || first.Materials[0].ID != raw.ID || !latest.ReplaceMaterials || len(latest.Materials) != 1 || latest.Materials[0].ID != summary.ID {
		t.Fatalf("material provenance missing: first=%+v latest=%+v", first, latest)
	}
	finish(replaced)

	next := submit("reuse-summary", false)
	assertMaterials(map[string]string{summary.ID: "summary content"})
	finish(next)
	cleared := submit("clear-materials", true)
	assertMaterials(nil)
	finish(cleared)

	detail, err = s.Detail(r.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	goal := "Refine summary without materials"
	edited, err := s.EditTask(ctx, r.TaskID, TaskEdit{Revision: detail.Task.Revision, Goal: &goal})
	if err != nil {
		t.Fatal(err)
	}
	revised, err := s.Resume(ctx, r.TaskID, "run-revised-task", edited.Revision)
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.RunRecord(ctx, revised.ID, r.TaskID)
	if err != nil || len(record.Messages) != 1 || !record.Messages[0].ReplaceMaterials {
		t.Fatalf("replacement boundary missing from revised Run: %+v %v", record, err)
	}
	assertMaterials(nil)
}

func TestActiveMaterialsAreIndependentOfHistoryWindows(t *testing.T) {
	for _, scenario := range []string{"message_limit", "byte_limit", "replace_outside_window", "clear_outside_window", "selection_cap"} {
		t.Run(scenario, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "work.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var ids []string
			want := map[string]string{}
			for i := range MaxMaterials {
				body := strings.Repeat(string(rune('a'+i)), maxMaterialBytes)
				m, err := saveMaterial(t.Context(), s.db, fmt.Sprintf("materials/%d.md", i), []byte(body), nil)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, m.ID)
				want[m.ID] = body
			}
			run, err := s.Submit(t.Context(), "materials", "按原始材料整理", "", "", true, ids...)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "replace_outside_window" || scenario == "clear_outside_window" || scenario == "selection_cap" {
				tx, err := s.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				result, err := tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text,replace_materials) VALUES(?,?,'user','调整材料',?)", run.ConversationID, run.ID, scenario != "selection_cap")
				if err != nil {
					t.Fatal(err)
				}
				seq, _ := result.LastInsertId()
				if scenario == "clear_outside_window" {
					want = nil
				} else {
					m, err := saveMaterial(t.Context(), tx, "materials/new.md", []byte("new active material"), nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := attachMaterials(tx, seq, []string{m.ID}); err != nil {
						t.Fatal(err)
					}
					if scenario == "replace_outside_window" {
						want = map[string]string{}
					} else {
						delete(want, ids[MaxMaterials-1])
					}
					want[m.ID] = "new active material"
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			pairs, reply := 30, "历史回复"
			if scenario == "byte_limit" {
				pairs, reply = 1, strings.Repeat("x", 256*1024)
			}
			for range pairs {
				if _, err := s.db.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user','继续'),(?,?,'assistant',?)", run.ConversationID, run.ID, run.ConversationID, run.ID, reply); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user','现在依据材料回答')", run.ConversationID, run.ID); err != nil {
				t.Fatal(err)
			}
			input, err := s.modelMessages(t.Context(), run.ConversationID)
			if err != nil || len(input) == 0 || len(input) > 50 || input[0].Role != "user" {
				t.Fatalf("invalid trimmed input: %d messages, %v", len(input), err)
			}
			got := map[string]string{}
			for _, message := range input {
				for _, material := range message.Materials {
					if _, duplicate := got[material.ID]; duplicate {
						t.Fatal("active material was duplicated")
					}
					got[material.ID] = material.Content
				}
			}
			if !maps.Equal(got, want) {
				t.Fatalf("active material IDs differ: got %v, want %v", slices.Collect(maps.Keys(got)), slices.Collect(maps.Keys(want)))
			}
		})
	}
}

func TestRequiredInputOverContextLimitFailsBeforeModelRequest(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, err := s.Submit(t.Context(), "required-too-large", strings.Repeat("x", 256*1024+1), "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.modelMessages(t.Context(), run.ConversationID); !errors.Is(err, ErrModelRequestBudget) {
		t.Fatalf("oversize current request silently disappeared: %v", err)
	}
	run, err = s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.execute(t.Context(), testProvider(func(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error) {
		t.Fatal("model called without the required input")
		return nil, nil
	}), run, RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 64})
	detail, err := s.Detail(run.TaskID, true)
	if err != nil || detail.Run.ErrorCode != "model_input_budget" || detail.Task.StatusReason != "budget_exhausted" || detail.Artifact != nil || detail.Run.Usage.ModelRequests != 0 {
		t.Fatalf("input limit failure is not explicit: run=%+v task=%+v error=%v", detail.Run, detail.Task, err)
	}
}
