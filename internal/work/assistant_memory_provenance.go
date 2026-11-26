package work

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

const assistantMemoryProvenanceLimit = 16
const AssistantMemoryExcerptLimit = 180

type AssistantMemoryProvenance struct {
	Revision         int64
	Rows             []AssistantMemoryProvenanceRow
	More             bool
	Excerpt          string
	ExcerptTruncated bool
}

type AssistantMemoryProvenanceRow struct {
	Slot, State, Title, Fingerprint string
	UsedRevision, CurrentRevision   int
	source                          int64
}

func (c AssistantMemoryProvenance) SourceSlot(slot string) (AssistantMemoryProvenanceRow, bool) {
	for _, row := range c.Rows {
		if row.Slot == slot && row.State == "local_source" {
			return row, true
		}
	}
	return AssistantMemoryProvenanceRow{}, false
}

// The original Run's reference plus the current personal catalog is the
// authority. It only reveals a source excerpt from the original conversation
// when an exact, still-valid read action requests it. There is no receipt write.
func (s *Store) ReadAssistantMemoryProvenance(ctx context.Context, actor, conversation, run string, selection *AssistantReadCardAction) (AssistantMemoryProvenance, error) {
	var out AssistantMemoryProvenance
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT c.revision FROM runs r JOIN conversations c ON c.id=r.conversation_id
WHERE r.id=? AND c.id=? AND EXISTS(SELECT 1 FROM messages reply
WHERE reply.run_id=r.id AND reply.conversation_id=c.id AND reply.role='assistant' AND length(reply.text)>0)
AND NOT `+revokedMCPReply("r.id"), run, conversation).Scan(&out.Revision); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT ref.memory_id,ref.revision,ref.title,
coalesce(m.revision,0),coalesce(m.source_deleted,0),coalesce(m.source_message,0),
coalesce(src.conversation_id,''),coalesce(src.run_id,'')
FROM run_memories ref LEFT JOIN memories m ON m.id=ref.memory_id
LEFT JOIN messages src ON src.seq=m.source_message AND src.role='user'
WHERE ref.run_id=? ORDER BY ref.rowid LIMIT ?`, run, assistantMemoryProvenanceLimit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(out.Rows) == assistantMemoryProvenanceLimit {
			out.More = true
			break
		}
		var row AssistantMemoryProvenanceRow
		var ref, title, sourceConversation, sourceRun string
		var removed bool
		if err := rows.Scan(&ref, &row.UsedRevision, &title, &row.CurrentRevision, &removed, &row.source, &sourceConversation, &sourceRun); err != nil {
			return out, err
		}
		row.Slot = strconv.Itoa(len(out.Rows) + 1)
		switch {
		case row.CurrentRevision == 0:
			row.State = "memory_removed"
		case removed || row.source != 0 && sourceConversation == "":
			row.State = "source_removed"
		case row.UsedRevision != row.CurrentRevision:
			row.State = "version_changed"
		case row.source == 0:
			row.State = "unlinked"
		case sourceConversation != conversation:
			row.State = "different_conversation"
		default:
			row.State, row.Title = "local_source", title
			row.Fingerprint = digest(toJSON([]any{ref, row.UsedRevision, row.CurrentRevision, row.source, sourceConversation, sourceRun, title}))
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if selection == nil {
		return out, nil
	}
	slot, proof, split := strings.Cut(selection.Target, ":")
	row, ok := out.SourceSlot(slot)
	if !split || !ok || selection.Actor != actor || selection.Conversation != conversation || selection.Run != run ||
		selection.Revision != out.Revision || row.Fingerprint != proof {
		return out, ErrConversationRevision
	}
	if err := tx.QueryRowContext(ctx, `SELECT substr(text,1,?) FROM messages
WHERE seq=? AND conversation_id=? AND role='user'`, AssistantMemoryExcerptLimit+1, row.source, conversation).Scan(&out.Excerpt); err != nil {
		return out, err
	}
	runes := []rune(out.Excerpt)
	if len(runes) > AssistantMemoryExcerptLimit {
		out.Excerpt = string(runes[:AssistantMemoryExcerptLimit])
		out.ExcerptTruncated = true
	}
	return out, nil
}
