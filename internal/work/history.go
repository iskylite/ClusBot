package work

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
)

const conversationHistorySize = 100
const messageHistorySize = 50

var ErrHistoryCursor = errors.New("历史分页参数无效")

type ConversationPage struct {
	Items      []Conversation `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

type conversationCursor struct {
	CreatedAt int64  `json:"at"`
	ID        string `json:"id"`
	Archived  bool   `json:"archived"`
}

// ConversationsPage uses immutable creation time plus ID as a stable ordering.
// The cursor retains its boundary even when that conversation is deleted.
func (s *Store) ConversationsPage(ctx context.Context, archived bool, after string) (ConversationPage, error) {
	var cursor conversationCursor
	if after != "" {
		encoded, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil || json.Unmarshal(encoded, &cursor) != nil || cursor.ID == "" || cursor.Archived != archived {
			return ConversationPage{}, ErrHistoryCursor
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+conversationColumns+`,created_at FROM conversations c
WHERE archived=? AND NOT EXISTS(SELECT 1 FROM tasks WHERE conversation_id=c.id)
AND (?='' OR created_at<? OR (created_at=? AND id<?))
ORDER BY created_at DESC,id DESC LIMIT ?`, archived, after, cursor.CreatedAt, cursor.CreatedAt, cursor.ID, conversationHistorySize+1)
	if err != nil {
		return ConversationPage{}, err
	}
	defer rows.Close()
	page := ConversationPage{Items: []Conversation{}}
	var boundary conversationCursor
	for rows.Next() {
		if len(page.Items) == conversationHistorySize {
			encoded, _ := json.Marshal(boundary)
			page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
			break
		}
		var item Conversation
		var createdAt int64
		if err := rows.Scan(&item.ID, &item.Title, &item.Archived, &item.Revision, &createdAt); err != nil {
			return ConversationPage{}, err
		}
		page.Items = append(page.Items, item)
		boundary = conversationCursor{CreatedAt: createdAt, ID: item.ID, Archived: archived}
	}
	return page, rows.Err()
}

type MessagePage struct {
	Items        []Message `json:"items"`
	BeforeCursor int64     `json:"before_cursor,omitempty"`
	AfterCursor  int64     `json:"after_cursor,omitempty"`
}

// MessageHistory reads retained evidence from active and archived conversations.
// Every cursor must belong to the requested conversation, including Task sources.
func (s *Store) MessageHistory(ctx context.Context, conversationID string, before, after, around int64) (MessagePage, error) {
	if before < 0 || after < 0 || around < 0 || (before != 0 && after != 0) || (around != 0 && (before != 0 || after != 0)) {
		return MessagePage{}, ErrHistoryCursor
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessagePage{}, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT 1 FROM conversations WHERE id=?", conversationID).Scan(&exists); err != nil {
		return MessagePage{}, err
	}
	anchor := max(before, after, around)
	if anchor != 0 {
		if err = tx.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE conversation_id=? AND seq=?", conversationID, anchor).Scan(&exists); err != nil {
			return MessagePage{}, err
		}
	}
	selection := "SELECT seq,role,text,replace_materials,run_id FROM messages WHERE conversation_id=?"
	args := []any{conversationID}
	switch {
	case around != 0:
		selection += ` AND seq IN (
SELECT seq FROM (SELECT seq FROM messages WHERE conversation_id=? AND seq<=? ORDER BY seq DESC LIMIT 25)
UNION ALL SELECT seq FROM (SELECT seq FROM messages WHERE conversation_id=? AND seq>? ORDER BY seq LIMIT 25))`
		args = append(args, conversationID, around, conversationID, around)
	case before != 0:
		selection += " AND seq<? ORDER BY seq DESC LIMIT 50"
		args = append(args, before)
	case after != 0:
		selection += " AND seq>? ORDER BY seq LIMIT 50"
		args = append(args, after)
	default:
		selection += " ORDER BY seq DESC LIMIT 50"
	}
	page := MessagePage{}
	page.Items, err = queryMessages(tx, true, selection, args...)
	if err != nil {
		return MessagePage{}, err
	}
	if len(page.Items) != 0 {
		first, last := page.Items[0].ID, page.Items[len(page.Items)-1].ID
		var older, newer bool
		if err = tx.QueryRowContext(ctx, `SELECT
EXISTS(SELECT 1 FROM messages WHERE conversation_id=? AND seq<?),
EXISTS(SELECT 1 FROM messages WHERE conversation_id=? AND seq>?)`, conversationID, first, conversationID, last).Scan(&older, &newer); err != nil {
			return MessagePage{}, err
		}
		if older {
			page.BeforeCursor = first
		}
		if newer {
			page.AfterCursor = last
		}
	}
	return page, tx.Commit()
}
