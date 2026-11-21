package work

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestAccountBusinessCatalogSharesGrantsAndKeepsTaskReceiptsPrivate(t *testing.T) {
	ctx := t.Context()
	instance, read := taskReadFixture(t)
	legacy, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Submit(ctx, "prior-local-user", "Old unowned data", "", "", true); err != nil {
		t.Fatal(err)
	}
	if err := legacy.BindAccountBusinessStore(ctx, "user:alice"); err == nil {
		t.Fatal("unattributed legacy database was silently assigned")
	}
	open := func(name, principal string) *Store {
		t.Helper()
		s, err := OpenAccountBusinessStore(filepath.Join(t.TempDir(), name), instance)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if err := s.BindAccountBusinessStore(ctx, principal); err != nil {
			t.Fatal(err)
		}
		return s
	}
	alice, bob := open("alice.db", "user:alice"), open("bob.db", "user:bob")
	if err := alice.BindAccountBusinessStore(ctx, "user:bob"); err == nil {
		t.Fatal("personal database was rebound to another owner")
	}
	read.Arguments = map[string]any{"mode": "large"}
	skill, err := instance.DraftSkill(ctx, "ops", "Use the fixture receipt.", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.ActivateSkill(ctx, skill.SkillID, skill.Version, skill.Revision, skill.Digest); err != nil {
		t.Fatal(err)
	}
	run, err := alice.SubmitWithDelivery(ctx, "shared-tool-task", "Review the fixture", "ops", "", true, Delivery{Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.claim(ctx); err != nil {
		t.Fatal(err)
	}
	pinned, err := alice.skillForRun(ctx, run)
	if err != nil || pinned == nil || pinned.SkillID != skill.SkillID {
		t.Fatalf("shared skill not applied: %+v %v", pinned, err)
	}
	readResult, err := alice.runTaskRead(ctx, run.ID)
	if err != nil || readResult.Result == nil || readResult.Result.Reference == nil {
		t.Fatalf("shared tool not called through task: %+v %v", readResult, err)
	}
	callID := readResult.Result.Reference.ID
	if content, err := alice.ConnectionEvidence(ctx, callID); err != nil || len(content) == 0 {
		t.Fatalf("owner could not download task evidence: %d %v", len(content), err)
	}
	for _, other := range []*Store{instance, bob} {
		if _, err := other.ConnectionEvidence(ctx, callID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("another store could download task evidence: %v", err)
		}
	}
	if err := instance.SetToolPermission(ctx, read.ConnectionID, read.ToolName, read.ToolVersion, read.Revision, false); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.ConnectionEvidence(ctx, callID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked task evidence remained available: %v", err)
	}
	if _, err := bob.SubmitWithDelivery(ctx, "shared-tool-task", "Review", "", "", true, Delivery{Read: &read}); !errors.Is(err, ErrToolPermission) {
		t.Fatalf("revoked tool still accepted: %v", err)
	}
}
