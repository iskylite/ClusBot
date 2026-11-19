package work

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

const runtimeAttentionFeedSchema = `
CREATE TABLE IF NOT EXISTS attention_feed_identity (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1), epoch TEXT NOT NULL
);
INSERT OR IGNORE INTO attention_feed_identity VALUES(1, lower(hex(randomblob(16))));

CREATE TABLE IF NOT EXISTS attention_feed_events (
 actor TEXT NOT NULL,
 sequence INTEGER NOT NULL,
 proof TEXT NOT NULL DEFAULT (lower(hex(randomblob(16)))),
 event_key TEXT NOT NULL,
 item_id TEXT NOT NULL,
 kind TEXT NOT NULL,
 state TEXT NOT NULL,
 occurred_at INTEGER NOT NULL,
 PRIMARY KEY(actor,sequence),
 UNIQUE(actor,event_key)
);

CREATE TRIGGER IF NOT EXISTS approval_attention_created AFTER INSERT ON approvals
WHEN NEW.subject<>''
BEGIN
 INSERT INTO attention_feed_events(actor,sequence,event_key,item_id,kind,state,occurred_at)
 VALUES(NEW.subject,coalesce((SELECT max(sequence) FROM attention_feed_events WHERE actor=NEW.subject),0)+1,
 'approval:'||NEW.approval_id||':created','approval:'||NEW.approval_id,'approval_created',
 CASE WHEN NEW.decision='pending' THEN 'decision_required' ELSE NEW.decision END,NEW.created_at);
END;

CREATE TRIGGER IF NOT EXISTS approval_attention_decided AFTER UPDATE OF decision ON approvals
WHEN NEW.decision<>OLD.decision AND NEW.subject<>''
BEGIN
 INSERT INTO attention_feed_events(actor,sequence,event_key,item_id,kind,state,occurred_at)
 VALUES(NEW.subject,coalesce((SELECT max(sequence) FROM attention_feed_events WHERE actor=NEW.subject),0)+1,
 'approval:'||NEW.approval_id||':decision:'||NEW.decision,'approval:'||NEW.approval_id,'approval_decided',
 CASE WHEN NEW.decision='pending' THEN 'decision_required' ELSE NEW.decision END,
 CASE WHEN NEW.decided_at>0 THEN NEW.decided_at ELSE NEW.created_at END);
END;
`

func migrateAttentionFeed(tx *sql.Tx) error {
	_, err := tx.Exec(runtimeAttentionFeedSchema)
	return err
}

// enqueueAttentionFeed records an event in the same transaction as its
// authoritative state. Actor-local sequences reveal no other actor activity.
func enqueueAttentionFeed(tx *sql.Tx, actor, key, itemID, kind, state string, stamp int64) error {
	if actor == "" {
		return ErrAuthorizationDenied
	}
	_, err := tx.Exec(`INSERT INTO attention_feed_events(actor,sequence,event_key,item_id,kind,state,occurred_at)
VALUES(?,coalesce((SELECT max(sequence) FROM attention_feed_events WHERE actor=?),0)+1,?,?,?,?,?)
ON CONFLICT(actor,event_key) DO NOTHING`, actor, actor, key, itemID, kind, state, stamp)
	return err
}

var ErrAttentionCursorReset = errors.New("待处理事件游标无法续读，请重新读取快照")

type AttentionFeedEvent struct {
	Sequence   int64  `json:"sequence"`
	ItemID     string `json:"item_id"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	OccurredAt int64  `json:"occurred_at"`
}

type AttentionFeedPage struct {
	Events  []AttentionFeedEvent `json:"events"`
	Cursor  string               `json:"cursor"`
	HasMore bool                 `json:"has_more"`
}

// ReadAttentionFeed is bounded replay of committed attention changes. Empty
// cursor starts at feed inception (never implies pre-migration history).
// A valid cursor is bound to the original committed row, so an overwritten or
// restored database cannot silently reuse a different row at the same number.
func (s *Store) ReadAttentionFeed(ctx context.Context, actor, cursor string, limit int) (AttentionFeedPage, error) {
	if actor == "" || actor == "user:anonymous" {
		return AttentionFeedPage{}, ErrAuthorizationDenied
	}
	if limit < 1 || limit > 100 {
		return AttentionFeedPage{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AttentionFeedPage{}, err
	}
	defer tx.Rollback()
	var epoch string
	if err := tx.QueryRowContext(ctx, "SELECT epoch FROM attention_feed_identity WHERE singleton=1").Scan(&epoch); err != nil {
		return AttentionFeedPage{}, err
	}
	scope := digest(epoch + ":" + actor)
	proof, after := epoch, int64(0)
	if cursor != "" {
		parts := strings.SplitN(cursor, ":", 4)
		if len(cursor) > 160 || len(parts) != 3 || parts[0] != scope {
			return AttentionFeedPage{}, ErrAttentionCursorReset
		}
		after, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || after < 0 || strconv.FormatInt(after, 10) != parts[1] {
			return AttentionFeedPage{}, ErrAttentionCursorReset
		}
		if after != 0 {
			err := tx.QueryRowContext(ctx, "SELECT proof FROM attention_feed_events WHERE actor=? AND sequence=?", actor, after).Scan(&proof)
			if errors.Is(err, sql.ErrNoRows) {
				return AttentionFeedPage{}, ErrAttentionCursorReset
			}
			if err != nil {
				return AttentionFeedPage{}, err
			}
		}
		if parts[2] != proof {
			return AttentionFeedPage{}, ErrAttentionCursorReset
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,proof,item_id,kind,state,occurred_at
FROM attention_feed_events WHERE actor=? AND sequence>? ORDER BY sequence LIMIT ?`, actor, after, limit+1)
	if err != nil {
		return AttentionFeedPage{}, err
	}
	defer rows.Close()
	page := AttentionFeedPage{Events: make([]AttentionFeedEvent, 0, limit)}
	for rows.Next() {
		if len(page.Events) == limit {
			page.HasMore = true
			break
		}
		var e AttentionFeedEvent
		if err := rows.Scan(&e.Sequence, &proof, &e.ItemID, &e.Kind, &e.State, &e.OccurredAt); err != nil {
			return AttentionFeedPage{}, err
		}
		if e.Sequence != after+1 {
			return AttentionFeedPage{}, ErrAttentionCursorReset
		}
		after = e.Sequence
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return AttentionFeedPage{}, err
	}
	page.Cursor = scope + ":" + strconv.FormatInt(after, 10) + ":" + proof
	return page, nil
}
