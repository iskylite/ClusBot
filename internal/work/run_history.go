package work

import (
	"context"
	"encoding/json"
	"strings"
)

const runPageSize = 20

type RunListItem struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	CreatedAt    int64  `json:"created_at"`
	InputExcerpt string `json:"input_excerpt,omitempty"`
}

type RunPage struct {
	Items      []RunListItem `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type RunRecord struct {
	Run
	CreatedAt      int64         `json:"created_at"`
	EventAfter     int64         `json:"event_after"`
	Messages       []Message     `json:"messages"`
	InheritedInput *Message      `json:"inherited_input,omitempty"`
	Reads          []TaskRead    `json:"reads,omitempty"`
	MemoryRefs     []MemoryRef   `json:"memory_refs,omitempty"`
	Artifacts      []Artifact    `json:"artifacts,omitempty"`
	Publications   []Publication `json:"publications,omitempty"`
	Contexts       []RunContext  `json:"contexts,omitempty"`
}

type RunContext struct {
	Generation           int             `json:"execution_generation"`
	TaskRevision         *int            `json:"task_revision,omitempty"`
	AssignmentGeneration *int            `json:"assignment_generation,omitempty"`
	WorkerRole           string          `json:"worker_role,omitempty"`
	RecordedAt           int64           `json:"recorded_at"`
	PreviousRevision     int64           `json:"previous_revision"`
	ControlRevision      int             `json:"control_revision"`
	Manifest             json.RawMessage `json:"manifest"`
	ModelReceipt         *ModelReceipt   `json:"model_receipt,omitempty"`
}

func (s *Store) TaskRuns(ctx context.Context, taskID, after string) (RunPage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunPage{}, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE id=?", taskID).Scan(&exists); err != nil {
		return RunPage{}, err
	}
	var before int64
	if after != "" {
		if err = tx.QueryRowContext(ctx, "SELECT rowid FROM runs WHERE task_id=? AND id=?", taskID, after).Scan(&before); err != nil {
			return RunPage{}, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.id,r.status,r.created_at,coalesce((SELECT substr(text,1,100)
FROM messages WHERE run_id=r.id AND role='user' ORDER BY seq DESC LIMIT 1),'')
FROM runs r WHERE r.task_id=? AND (?=0 OR r.rowid<?) ORDER BY r.rowid DESC LIMIT ?`, taskID, before, before, runPageSize+1)
	if err != nil {
		return RunPage{}, err
	}
	defer rows.Close()
	page := RunPage{Items: make([]RunListItem, 0, runPageSize)}
	for rows.Next() {
		if len(page.Items) == runPageSize {
			page.NextCursor = page.Items[len(page.Items)-1].ID
			break
		}
		var entry RunListItem
		if err = rows.Scan(&entry.ID, &entry.Status, &entry.CreatedAt, &entry.InputExcerpt); err != nil {
			return RunPage{}, err
		}
		line, _, _ := strings.Cut(strings.TrimSpace(entry.InputExcerpt), "\n")
		entry.InputExcerpt = strings.TrimSpace(line)
		page.Items = append(page.Items, entry)
	}
	return page, rows.Err()
}

func (s *Store) RunRecord(ctx context.Context, runID, taskID string) (RunRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunRecord{}, err
	}
	defer tx.Rollback()
	var record RunRecord
	record.Run, err = scanRun(tx.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE id=? AND (?='' OR task_id=?)", runID, taskID, taskID))
	if err != nil {
		return RunRecord{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT created_at,coalesce((SELECT max(seq) FROM run_events WHERE run_id=r.id),0)
FROM runs r WHERE r.id=?`, runID).Scan(&record.CreatedAt, &record.EventAfter); err != nil {
		return RunRecord{}, err
	}
	record.RouterIntents, err = routerIntentReceipt(tx, runID)
	if err != nil {
		return RunRecord{}, err
	}
	record.Messages, err = queryMessages(tx, true, "SELECT seq,role,text,replace_materials,run_id FROM messages WHERE run_id=?", runID)
	if err != nil {
		return RunRecord{}, err
	}
	if record.TaskID != "" && (len(record.Messages) == 0 || record.Messages[0].Role != "user") {
		// A fresh Task retry can reuse the last input without inserting a message.
		previous, err := queryMessages(tx, false, `SELECT seq,role,text,replace_materials,run_id FROM messages WHERE conversation_id=? AND role='user'
AND run_id IN (SELECT id FROM runs WHERE conversation_id=? AND memory_command=0 AND rowid<(SELECT rowid FROM runs WHERE id=?))
ORDER BY seq DESC LIMIT 1`, record.ConversationID, record.ConversationID, runID)
		if err != nil {
			return RunRecord{}, err
		}
		if len(previous) > 0 {
			record.InheritedInput = &previous[0]
		}
	}
	if record.Reads, err = listTaskReadEvidence(ctx, tx, runID); err != nil {
		return RunRecord{}, err
	}
	if record.MemoryRefs, err = readMemoryRefs(tx, runID); err != nil {
		return RunRecord{}, err
	}
	record.Contexts, err = scanRunRecords(ctx, tx, `SELECT m.generation,b.task_revision,b.assignment_generation,
coalesce(a.worker_role,''),m.assembled_at,m.previous_revision,m.control_revision,m.manifest,coalesce(mr.protocol,''),coalesce(mr.state,''),coalesce(mr.reported_model,'')
FROM context_manifests m JOIN run_bindings b ON b.run_id=m.run_id AND b.generation=m.generation
LEFT JOIN assignments a ON a.task_id=b.task_id AND a.generation=b.assignment_generation
LEFT JOIN model_receipts mr ON mr.manifest_id=m.id
WHERE m.run_id=? ORDER BY m.id DESC LIMIT 32`, runID, func(row scanner) (RunContext, error) {
		var entry RunContext
		var payload string
		var receipt ModelReceipt
		err := row.Scan(&entry.Generation, &entry.TaskRevision, &entry.AssignmentGeneration, &entry.WorkerRole,
			&entry.RecordedAt, &entry.PreviousRevision, &entry.ControlRevision, &payload, &receipt.Protocol, &receipt.State, &receipt.ReportedModel)
		entry.Manifest = json.RawMessage(payload)
		if receipt.State != "" {
			entry.ModelReceipt = &receipt
		}
		return entry, err
	})
	if err != nil {
		return RunRecord{}, err
	}
	if record.TaskID != "" {
		if record.Artifacts, err = listArtifacts(tx, record.TaskID, "", "", runID, 1); err != nil {
			return RunRecord{}, err
		}
		record.Publications, err = scanRunRecords(ctx, tx, "SELECT "+publicationColumns+" FROM publications WHERE run_id=? ORDER BY rowid", runID, scanPublication)
		if err != nil {
			return RunRecord{}, err
		}

	}
	return record, tx.Commit()
}

func scanRunRecords[T any](ctx context.Context, q taskReadQuerier, query, runID string, scan func(scanner) (T, error)) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []T
	for rows.Next() {
		record, err := scan(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
