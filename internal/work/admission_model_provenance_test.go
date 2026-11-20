package work

import (
	"path/filepath"
	"testing"
)

func TestRouterModelProvenanceMigrationPreservesOldClassification(t *testing.T) {
	name := filepath.Join(t.TempDir(), "admission-model.sqlite")
	s, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := s.AdmitChat(ctx, "older-router-receipt", "user:owner", "", "old input", "", false, Delivery{}, nil, true); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE submission_classifications DROP COLUMN router_model_provenance`,
		`INSERT INTO submission_classifications(request_key,fingerprint,conversation_id,state,requests,output_used,created_at)
SELECT event_id,payload_digest,'','completed',1,0,received_at FROM inbox_events WHERE event_id='older-router-receipt'`,
		`PRAGMA user_version=123`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owner, err := s.ReadOwnedAdmission(ctx, "older-router-receipt", "user:owner")
	if err != nil || owner.ClassificationState != "completed" || owner.RouterModel != nil {
		t.Fatalf("legacy classification gained invented model evidence: %+v %v", owner, err)
	}
	for _, principal := range []string{"user:other", ""} {
		if _, err := s.ReadOwnedAdmission(ctx, "older-router-receipt", principal); err == nil {
			t.Fatalf("cross-principal receipt read accepted: %q", principal)
		}
	}
}
