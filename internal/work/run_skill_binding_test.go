package work

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSkillBindingShowsActualVersionAndRevocation(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "run-skill-binding.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor, scope, body = "user:session", "reports", "Internal content never returned"
	makeSkill := func(instructions string, revision int) SkillVersion {
		t.Helper()
		v, err := s.DraftSkill(ctx, scope, instructions, revision)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ActivateSkill(ctx, v.SkillID, v.Version, revision, v.Digest); err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := makeSkill(body, 0)
	if _, err := s.Submit(ctx, "bound-skill", "summarize", scope, "", true); err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := func() RunSkillBinding {
		t.Helper()
		out, err := s.ReadRunSkillBinding(ctx, actor, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(); got.Binding != nil || got.TaskID != run.TaskID {
		t.Fatalf("active catalog was incorrectly treated as a binding: %+v", got)
	}
	if _, err := s.ReadRunSkillBinding(ctx, "other", run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other actor can read binding: %v", err)
	}
	if ref, err := s.skillForRun(ctx, run); err != nil || ref == nil {
		t.Fatalf("stage actual binding: %+v %v", ref, err)
	}
	b := read()
	if b.Binding == nil || b.Binding.Version != first.Version || b.Binding.Lifecycle != "current_run" || b.Binding.Validity != "valid" {
		t.Fatalf("missing durable binding: %+v", b)
	}
	if encoded, err := json.Marshal(b); err != nil || strings.Contains(string(encoded), body) || strings.Contains(string(encoded), first.Digest) {
		t.Fatalf("binding leaked internals: %s %v", encoded, err)
	}
	second := makeSkill("Use another method", 1)
	if got := read().Binding; got.Version != first.Version || got.ActiveVersion == nil || *got.ActiveVersion != second.Version || got.Validity != "valid" {
		t.Fatalf("active version replaced the in-flight version: %+v", got)
	}
	if err := s.DisableSkill(ctx, first.SkillID, 2); err != nil {
		t.Fatal(err)
	}
	if got := read().Binding; got.Validity != "revoked" || got.ActiveVersion != nil || got.Version != first.Version {
		t.Fatalf("revocation erased history or left the binding valid: %+v", got)
	}
	if _, err := s.db.Exec("UPDATE runs SET status='completed' WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	if got := read().Binding; got.Lifecycle != "historical" || got.Validity != "revoked" {
		t.Fatalf("finished run advertised a live binding: %+v", got)
	}
}
