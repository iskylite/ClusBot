package work

import (
	"context"
	"database/sql"
	"errors"
)

var ErrConversationBindingConflict = errors.New("会话绑定已变化或请求缺少当前绑定版本，请刷新后重试")

func migrateConversationBindingRevisions(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS conversation_binding_revisions (
 conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('skill','mcp')),
 revision INTEGER NOT NULL CHECK(revision>0),
 PRIMARY KEY(conversation_id,kind)
)`)
	if err != nil {
		return err
	}
	// Some supported sparse historical stores have no conversations table;
	// they cannot possess legal bindings. SQLite validates the parent even
	// when the source of INSERT contains no rows, so skip seeding there.
	var hasConversations bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='conversations')").Scan(&hasConversations); err != nil || !hasConversations {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO conversation_binding_revisions SELECT conversation_id,'skill',1 FROM conversation_skill_bindings;
INSERT OR IGNORE INTO conversation_binding_revisions SELECT conversation_id,'mcp',1 FROM conversation_mcp_selections;`)
	return err
}

func conversationBindingRevision(ctx context.Context, tx *sql.Tx, conversation, kind string) (int, error) {
	var revision int
	err := tx.QueryRowContext(ctx, "SELECT revision FROM conversation_binding_revisions WHERE conversation_id=? AND kind=?", conversation, kind).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

func checkConversationBindingRevision(ctx context.Context, tx *sql.Tx, conversation, kind string, expected *int) (int, error) {
	current, err := conversationBindingRevision(ctx, tx, conversation, kind)
	if err != nil {
		return 0, err
	}
	if expected != nil && *expected != current {
		return 0, ErrConversationBindingConflict
	}
	return current, nil
}

func advanceConversationBindingRevision(ctx context.Context, tx *sql.Tx, conversation, kind string, current int, expected *int, changed sql.Result) error {
	n, err := changed.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if n != 1 {
		return ErrConversationBindingConflict
	}
	// Older clients can initialize untouched state or replay a no-op, but
	// omission can never overwrite newer selections or reuse a cleared slot.
	if expected == nil && current != 0 {
		return ErrConversationBindingConflict
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO conversation_binding_revisions(conversation_id,kind,revision) VALUES(?,?,1)
ON CONFLICT(conversation_id,kind) DO UPDATE SET revision=revision+1 WHERE revision=?`, conversation, kind, current)
	if err != nil {
		return err
	}
	if count, err := res.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return ErrConversationBindingConflict
	}
	return nil
}
