package work

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSkillApprovalAndRevocationAreVersionBound(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const scope = "例行报告"
	first, err := s.DraftSkill(t.Context(), scope, "先列出结论，再列依据。", 0)
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := s.DraftSkill(t.Context(), scope, "先列风险。", 0)
	if err != nil {
		t.Fatal(err)
	}
	makeRun := func(key string) Run {
		t.Helper()
		if _, err := s.Submit(t.Context(), key, "整理本周数据", scope, "", true); err != nil {
			t.Fatal(err)
		}
		run, err := s.claim(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	unapproved := makeRun("skill-draft-run")
	if ref, err := s.skillForRun(t.Context(), unapproved); err != nil || ref != nil {
		t.Fatalf("draft became active: %+v, %v", ref, err)
	}
	if err = s.ActivateSkill(t.Context(), first.SkillID, first.Version, 0, "other"); !errors.Is(err, ErrState) {
		t.Fatalf("wrong proposal digest: %v", err)
	}
	if err = s.ActivateSkill(t.Context(), first.SkillID, first.Version, 0, first.Digest); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSkill(t.Context(), parallel.SkillID, parallel.Version, 1, parallel.Digest); !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("stale draft applied: %v", err)
	}
	run := makeRun("skill-active-run")
	ref, err := s.skillForRun(t.Context(), run)
	if err != nil || ref == nil || ref.Digest != first.Digest {
		t.Fatalf("approved Skill not selected: %+v, %v", ref, err)
	}
	if err = s.DisableSkill(t.Context(), first.SkillID, 1); err != nil {
		t.Fatal(err)
	}
	if err = RecordModelManifest(s.withManifestRecorder(t.Context(), run), ManifestRequest{Skills: map[string]int{first.SkillID: first.Version}}); !errors.Is(err, ErrReadCheckpoint) {
		t.Fatalf("revoked Skill was admitted: %v", err)
	}
	if err = s.ActivateSkill(t.Context(), first.SkillID, first.Version, 2, first.Digest); err != nil {
		t.Fatal(err)
	}
	if ref, err = s.skillForRun(t.Context(), run); err != nil || ref == nil || ref.Version != first.Version {
		t.Fatalf("approved version could not be reactivated: %+v, %v", ref, err)
	}
	newer, err := s.DraftSkill(t.Context(), scope, "附上证据清单。", 3)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSkill(t.Context(), newer.SkillID, newer.Version, newer.Revision, newer.Digest); err != nil {
		t.Fatal(err)
	}
	if ref, err = s.skillForRun(t.Context(), run); err != nil || ref == nil || ref.Version != first.Version {
		t.Fatalf("publishing changed the existing run binding: %+v, %v", ref, err)
	}
	manifest := func(r Run, v SkillVersion) error {
		return RecordModelManifest(s.withManifestRecorder(t.Context(), r), ManifestRequest{Skills: map[string]int{v.SkillID: v.Version}})
	}
	if err = manifest(run, first); err != nil {
		t.Fatalf("publishing blocked the existing run: %v", err)
	}
	if err = manifest(run, newer); !errors.Is(err, ErrReadCheckpoint) {
		t.Fatalf("existing run admitted an unbound version: %v", err)
	}
	newRun := makeRun("skill-new-version-run")
	if ref, err = s.skillForRun(t.Context(), newRun); err != nil || ref == nil || ref.Version != newer.Version {
		t.Fatalf("new run did not load the published version: %+v, %v", ref, err)
	}
	if err = s.ActivateSkill(t.Context(), first.SkillID, first.Version, newer.Revision+1, first.Digest); err != nil {
		t.Fatal(err)
	}
	if err = manifest(newRun, newer); !errors.Is(err, ErrReadCheckpoint) {
		t.Fatalf("rolled-back Skill was admitted: %v", err)
	}
}
