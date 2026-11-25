package work

import (
	"errors"
	"testing"
)

func TestOperationDoesNotAliasDifferentBinding(t *testing.T) {
	s, taskID := setupOperationsStore(t, "op-binding.db")
	req := operationFixture(taskID, "O9")
	stamp := now()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	first, err := registerOperation(tx, req, stamp)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Operation){
		func(o *Operation) { o.ParamsDigest = digest("different") },
		func(o *Operation) { o.ConnectionID = "different-provider" },
		func(o *Operation) { o.TargetRefs = `[{"ref":"other","revision":1,"epoch":1}]` },
		func(o *Operation) { o.CapabilityVersion++ },
		func(o *Operation) { o.StepID = "other-step" },
	} {
		conflict := req
		change(&conflict)
		if got, err := registerOperation(tx, conflict, stamp); !errors.Is(err, ErrConflict) || got.OperationID != "" {
			t.Fatalf("different intent accepted: %v", err)
		}
	}
	replay, err := registerOperation(tx, req, stamp)
	if err != nil || replay.OperationID != first.OperationID {
		t.Fatalf("same intent replay failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
