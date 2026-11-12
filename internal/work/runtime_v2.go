package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func recordInputEvent(tx *sql.Tx, message int64, source string) error {
	_, err := tx.Exec("INSERT INTO input_events VALUES(?,?,?)", message, source, now())
	return err
}

func pendingInputs(tx *sql.Tx, runID string, generation int) (map[int64]bool, error) {
	rows, err := tx.Query(`SELECT d.message_seq FROM input_deliveries d WHERE d.run_id=? AND d.generation=?
AND NOT EXISTS (SELECT 1 FROM input_dispositions p WHERE p.message_seq=d.message_seq)`, runID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[int64]bool)
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		ids[seq] = true
	}
	return ids, rows.Err()
}

// The local Run host accepts the delivery in the same transaction that claims
// the Run. A restart keeps the event pending until a decision is committed.
func bindRun(tx *sql.Tx, run Run) error {
	if run.TaskID != "" {
		if _, err := tx.Exec(`INSERT INTO assignments
SELECT ?,1,'personal-worker',? WHERE NOT EXISTS(SELECT 1 FROM assignments WHERE task_id=?)`,
			run.TaskID, now(), run.TaskID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO task_revisions
SELECT id,revision,title,goal,scope_label,acceptance_criteria,required_filename FROM tasks WHERE id=?`, run.TaskID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO run_bindings
SELECT r.id,r.execution_generation,t.id,t.revision,
(SELECT max(generation) FROM assignments WHERE task_id=t.id),?
FROM runs r LEFT JOIN tasks t ON t.id=r.task_id WHERE r.id=?`, now(), run.ID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO input_deliveries
SELECT e.message_seq,?,? FROM input_events e JOIN messages m ON m.seq=e.message_seq
WHERE m.conversation_id=? AND NOT EXISTS (SELECT 1 FROM input_dispositions d WHERE d.message_seq=e.message_seq)`,
		run.ID, run.Generation, run.ConversationID)
	return err
}

// ManifestRequest records references and digests without duplicating material
// or memory contents. The model adapter supplies these after input fitting.
type ManifestRequest struct {
	Messages   []int64           `json:"messages"`
	Excerpted  []int64           `json:"excerpted_messages,omitempty"`
	Omitted    []int64           `json:"omitted_messages,omitempty"`
	Materials  map[string]string `json:"materials,omitempty"`
	Memories   map[string]int    `json:"memories,omitempty"`
	Experience map[string]string `json:"experience,omitempty"`
	Skills     map[string]int    `json:"skills,omitempty"`
	ToolItems  []string          `json:"tool_item_digests,omitempty"`
	Digest     string            `json:"request_sha256"`
	MaxOutput  int               `json:"max_output_tokens"`
	Provider   string            `json:"provider_binding,omitempty"`
}

type manifestRecorderKey struct{}

func RecordModelManifest(ctx context.Context, manifest ManifestRequest) error {
	record, ok := ctx.Value(manifestRecorderKey{}).(func(context.Context, ManifestRequest) error)
	if !ok {
		return nil
	}
	return record(ctx, manifest)
}

func (s *Store) withManifestRecorder(ctx context.Context, run Run) context.Context {
	return context.WithValue(ctx, manifestRecorderKey{}, func(ctx context.Context, manifest ManifestRequest) error {
		payload, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if run.TaskID != "" {
			if err := checkWorkflowEvidence(tx, run.ID, run.Generation); err != nil {
				return err
			}
		}
		var skillGoal string
		if len(manifest.Skills) != 0 {
			if err := tx.QueryRowContext(ctx, "SELECT goal FROM tasks WHERE id=?", run.TaskID).Scan(&skillGoal); err != nil {
				return err
			}
		}
		for id, version := range manifest.Skills {
			if err = checkRunSkillVersion(ctx, tx, run.ID, id, version); err != nil {
				return err
			}
			effective, err := applicableSkillInstructions(tx, id, version, skillGoal)
			if errors.Is(err, ErrExperienceSource) || (err == nil && effective == "") {
				return ErrReadCheckpoint
			}
			if err != nil {
				return err
			}
		}
		for id, revision := range manifest.Memories {
			var valid bool
			if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memories WHERE id=? AND revision=?)", id, revision).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return ErrReadCheckpoint
			}
		}
		for id, hash := range manifest.Experience {
			var valid bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artifacts a JOIN tasks t ON t.id=a.task_id
JOIN conversations c ON c.id=t.conversation_id WHERE a.id=? AND a.sha256=? AND c.archived=0)`,
				id, hash).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return ErrReadCheckpoint
			}
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO context_manifests(run_id,generation,assembled_at,manifest)
SELECT b.run_id,b.generation,?,? FROM run_bindings b JOIN runs r ON r.id=b.run_id
LEFT JOIN tasks t ON t.id=b.task_id
WHERE r.id=? AND b.generation=? AND r.execution_generation=b.generation AND r.status='running' AND r.control=''
AND (t.id IS NULL OR (t.revision=b.task_revision AND b.assignment_generation=(SELECT max(generation) FROM assignments WHERE task_id=t.id)))`,
			now(), payload, run.ID, run.Generation)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrState
		}
		manifestID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		inputIDs, err := json.Marshal(manifest.Messages)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO context_inputs
SELECT ?,message_seq FROM input_deliveries WHERE run_id=? AND generation=?
AND message_seq IN (SELECT value FROM json_each(?))`, manifestID, run.ID, run.Generation, inputIDs); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func settleInputs(tx *sql.Tx, run Run, outcome string) error {
	if outcome != "completed" && outcome != "input_required" && outcome != "cancelled" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO input_dispositions
SELECT DISTINCT d.message_seq,d.run_id,d.generation,?,?
FROM input_deliveries d WHERE d.run_id=? AND d.generation=?
AND NOT EXISTS(SELECT 1 FROM input_dispositions p WHERE p.message_seq=d.message_seq)
AND (?='cancelled' OR EXISTS(SELECT 1 FROM context_inputs i JOIN context_manifests m ON m.id=i.manifest_id
WHERE i.message_seq=d.message_seq AND m.run_id=d.run_id AND m.generation=d.generation))`,
		outcome, now(), run.ID, run.Generation, outcome)
	return err
}

func cancelConversationInputs(tx *sql.Tx, conversation, run string) error {
	_, err := tx.Exec(`INSERT INTO input_dispositions
SELECT e.message_seq,r.id,r.execution_generation,'cancelled',?
FROM input_events e JOIN messages m ON m.seq=e.message_seq JOIN runs r ON r.id=?
WHERE m.conversation_id=? AND NOT EXISTS (SELECT 1 FROM input_dispositions d WHERE d.message_seq=e.message_seq)`,
		now(), run, conversation)
	return err
}
