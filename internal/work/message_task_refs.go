package work

import (
	"context"
	"database/sql"
)

type MessageTaskRef struct {
	TaskID string `json:"task_id"`
}

type MessageTaskRefView struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
}

func migrateMessageTaskRefs(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS message_task_refs (
 message_seq INTEGER PRIMARY KEY REFERENCES messages(seq) ON DELETE CASCADE,
 task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 actor TEXT NOT NULL
)`)
	return err
}

// The only dynamic part is an internally selected SQL parameter/column for
// the viewer. Never infer benchmark ownership from a historical actor name.
func visibleMessageTask(viewer string) string {
	return `t.conversation_id=c.id AND c.archived=0 AND NOT EXISTS(
 SELECT 1 FROM benchmark_runs b WHERE b.task_id=t.id AND b.actor<>` + viewer + `)`
}

func checkMessageTaskRef(tx *sql.Tx, actor, conversation string, ref *MessageTaskRef) error {
	if ref == nil {
		return nil
	}
	if conversation == "" || ref.TaskID == "" {
		return sql.ErrNoRows
	}
	if actor != "user:session" {
		allowed, err := ownsPersonalBusiness(tx, actor)
		if err != nil {
			return err
		}
		if !allowed {
			return sql.ErrNoRows
		}
	}
	var id string
	return tx.QueryRow(`SELECT t.id FROM tasks t JOIN conversations c ON c.id=t.conversation_id
WHERE c.id=? AND t.id=? AND `+visibleMessageTask("?"), conversation, ref.TaskID, actor).Scan(&id)
}

// MessageTaskReferenceOptions applies the same identity and visibility rules
// as submission, so the composer can distinguish a link from a text mention.
func (s *Store) MessageTaskReferenceOptions(ctx context.Context, actor, conversation string) (map[string]bool, error) {
	options := map[string]bool{}
	if conversation == "" {
		return options, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if actor != "user:session" {
		allowed, err := ownsPersonalBusiness(tx, actor)
		if err != nil || !allowed {
			return options, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.id FROM tasks t JOIN conversations c ON c.id=t.conversation_id
		WHERE c.id=? AND `+visibleMessageTask("?"), conversation, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		options[id] = true
	}
	return options, rows.Err()
}

// enrichMessageTaskRefs is only for authenticated message display. Model
// history and routing continue to read the undecorated message records.
func enrichMessageTaskRefs(tx *sql.Tx, conversation string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	rows, err := tx.Query(`SELECT m.seq,t.id,t.title FROM message_task_refs ref
JOIN messages m ON m.seq=ref.message_seq AND m.role='user'
JOIN tasks t ON t.id=ref.task_id JOIN conversations c ON c.id=m.conversation_id
WHERE c.id=? AND (ref.actor='user:session' OR EXISTS(SELECT 1 FROM account_business_owner owner WHERE owner.principal_id=ref.actor)) AND m.seq IN (SELECT value FROM json_each(?))
AND `+visibleMessageTask("ref.actor"), conversation, toJSON(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	refs := make(map[int64]*MessageTaskRefView)
	for rows.Next() {
		var seq int64
		var ref MessageTaskRefView
		if err := rows.Scan(&seq, &ref.TaskID, &ref.Title); err != nil {
			return err
		}
		refs[seq] = &ref
	}
	for i := range messages {
		if ref := refs[messages[i].ID]; ref != nil {
			messages[i].TaskRef = ref
		}
	}
	return rows.Err()
}
