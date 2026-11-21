package work

import (
	"context"
	"database/sql"
	"errors"
)

func migrateConversationSkillBindings(tx *sql.Tx) error {
	// Supported legacy snapshots can predate the personal Skill tables.
	// Normalize those schemas before adding the execution provenance column.
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS personal_skills (
 id TEXT PRIMARY KEY, scope_label TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL, active_version INTEGER
);
CREATE TABLE IF NOT EXISTS personal_skill_versions (
 skill_id TEXT NOT NULL REFERENCES personal_skills(id), version INTEGER NOT NULL, base_revision INTEGER NOT NULL,
 instructions TEXT NOT NULL, sha256 TEXT NOT NULL, approved_at INTEGER, PRIMARY KEY(skill_id,version)
);
CREATE TABLE IF NOT EXISTS run_skills (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE, skill_id TEXT NOT NULL, version INTEGER NOT NULL,
 FOREIGN KEY(skill_id,version) REFERENCES personal_skill_versions(skill_id,version)
)`); err != nil {
		return err
	}
	if err := addColumnIfMissing(tx, "run_skills", "binding_source",
		"ALTER TABLE run_skills ADD COLUMN binding_source TEXT NOT NULL DEFAULT 'scope_default'"); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS conversation_skill_bindings (
 conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
 skill_id TEXT NOT NULL, version INTEGER NOT NULL, selected_at INTEGER NOT NULL,
 FOREIGN KEY(skill_id,version) REFERENCES personal_skill_versions(skill_id,version)
)`)
	return err
}

type ConversationSkillBindingInput struct {
	ExpectedBindingRevision *int   `json:"expected_binding_revision,omitempty"`
	Action                  string `json:"action"`
	SkillID                 string `json:"skill_id,omitempty"`
	Version                 int    `json:"version,omitempty"`
}

type ConversationSkillSelection struct {
	BindingRevision int                       `json:"binding_revision"`
	ConversationID  string                    `json:"conversation_id"`
	Binding         *ConversationSkillBinding `json:"binding"`
}

type ConversationSkillBinding struct {
	SkillID    string `json:"skill_id"`
	Scope      string `json:"scope"`
	Version    int    `json:"version"`
	SelectedAt int64  `json:"selected_at_unix_ms"`
	Validity   string `json:"validity"`
}

func readConversationSkillSelection(ctx context.Context, tx *sql.Tx, conversation string) (ConversationSkillSelection, error) {
	out := ConversationSkillSelection{ConversationID: conversation}
	revision, err := conversationBindingRevision(ctx, tx, conversation, "skill")
	if err != nil {
		return out, err
	}
	out.BindingRevision = revision
	var c ConversationSkillBinding
	var active, approved sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT b.skill_id,s.scope_label,b.version,b.selected_at,s.active_version,v.approved_at
FROM conversation_skill_bindings b JOIN personal_skills s ON s.id=b.skill_id
JOIN personal_skill_versions v ON v.skill_id=b.skill_id AND v.version=b.version WHERE b.conversation_id=?`, conversation).
		Scan(&c.SkillID, &c.Scope, &c.Version, &c.SelectedAt, &active, &approved)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return ConversationSkillSelection{}, err
	}
	out.Binding = &c
	c.Validity = "valid"
	if !approved.Valid || !active.Valid || active.Int64 < int64(c.Version) {
		c.Validity = "revoked"
		return out, nil
	}
	if err := verifySkillProposalSources(tx, c.SkillID, c.Version); errors.Is(err, ErrExperienceSource) {
		c.Validity = "unavailable"
	} else if err != nil {
		return ConversationSkillSelection{}, err
	}
	return out, nil
}

func (s *Store) ReadConversationSkillSelection(ctx context.Context, actor, conversation string) (ConversationSkillSelection, error) {
	if actor != "user:session" {
		allowed, err := ownsPersonalBusiness(s.db, actor)
		if err != nil {
			return ConversationSkillSelection{}, err
		}
		if !allowed {
			return ConversationSkillSelection{}, sql.ErrNoRows
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationSkillSelection{}, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM conversations WHERE id=?", conversation).Scan(&id); err != nil {
		return ConversationSkillSelection{}, err
	}
	return readConversationSkillSelection(ctx, tx, id)
}

// SetConversationSkillSelection is explicit configuration in the existing
// local single-session scope. It does not grant tool or deployment authority.
func (s *Store) SetConversationSkillSelection(ctx context.Context, actor, conversation string, input ConversationSkillBindingInput) (ConversationSkillSelection, error) {
	if actor != "user:session" {
		allowed, err := ownsPersonalBusiness(s.db, actor)
		if err != nil {
			return ConversationSkillSelection{}, err
		}
		if !allowed {
			return ConversationSkillSelection{}, sql.ErrNoRows
		}
	}
	if (input.Action == "bind_skill" && (input.SkillID == "" || input.Version < 1)) ||
		(input.Action == "clear_skill" && (input.SkillID != "" || input.Version != 0)) ||
		(input.Action != "bind_skill" && input.Action != "clear_skill") {
		return ConversationSkillSelection{}, ErrSkill
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationSkillSelection{}, err
	}
	defer tx.Rollback()
	// Existing queued/running conversations must retain the settings under
	// which their runs were admitted. Apply this only once the conversation is idle.
	if err := checkConversationSubmission(tx, conversation); err != nil {
		return ConversationSkillSelection{}, err
	}
	current, err := checkConversationBindingRevision(ctx, tx, conversation, "skill", input.ExpectedBindingRevision)
	if err != nil {
		return ConversationSkillSelection{}, err
	}
	var changed sql.Result
	if input.Action == "clear_skill" {
		changed, err = tx.ExecContext(ctx, "DELETE FROM conversation_skill_bindings WHERE conversation_id=?", conversation)
	} else {
		var ready bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM personal_skills s JOIN personal_skill_versions v ON v.skill_id=s.id AND v.version=s.active_version
WHERE s.id=? AND v.version=? AND v.approved_at IS NOT NULL)`, input.SkillID, input.Version).Scan(&ready); err != nil {
			return ConversationSkillSelection{}, err
		}
		if !ready {
			return ConversationSkillSelection{}, ErrState
		}
		if err := verifySkillProposalSources(tx, input.SkillID, input.Version); err != nil {
			return ConversationSkillSelection{}, err
		}
		if err := rememberSkillReference(tx, input.SkillID, input.Version); err != nil {
			return ConversationSkillSelection{}, err
		}
		changed, err = tx.ExecContext(ctx, `INSERT INTO conversation_skill_bindings VALUES(?,?,?,?)
ON CONFLICT(conversation_id) DO UPDATE SET skill_id=excluded.skill_id,version=excluded.version,selected_at=excluded.selected_at
WHERE conversation_skill_bindings.skill_id<>excluded.skill_id OR conversation_skill_bindings.version<>excluded.version`,
			conversation, input.SkillID, input.Version, now())
	}
	if err != nil {
		return ConversationSkillSelection{}, err
	}
	if err := advanceConversationBindingRevision(ctx, tx, conversation, "skill", current, input.ExpectedBindingRevision, changed); err != nil {
		return ConversationSkillSelection{}, err
	}
	if n, err := changed.RowsAffected(); err != nil {
		return ConversationSkillSelection{}, err
	} else if n != 0 {
		if err := recordAudit(tx, AuditEvent{Event: "conversation_skill_" + input.Action, Actor: actor,
			DetailRef: conversation, Summary: "会话选用设置已更新（未改变运行中的绑定或工具权限）", OccurredAt: now()}); err != nil {
			return ConversationSkillSelection{}, err
		}
	}

	out, err := readConversationSkillSelection(ctx, tx, conversation)
	if err != nil {
		return ConversationSkillSelection{}, err
	}
	return out, tx.Commit()
}
