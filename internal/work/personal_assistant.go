package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type PersonalAssistant struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
}

func migrateMultiuserStorage(tx *sql.Tx) error {
	if err := applyAccountSchema(tx); err != nil {
		return err
	}
	if err := migrateTeamTaskResponsibilities(tx); err != nil {
		return err
	}
	return migratePersonalAssistants(tx)
}

func migratePersonalAssistants(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS personal_assistants (
		owner_id TEXT PRIMARY KEY NOT NULL,
		assistant_id TEXT UNIQUE NOT NULL,
		created_at INTEGER NOT NULL
	)`)
	return err
}

// EnsurePersonalAssistant accepts the trusted account principal from the server,
// never an owner ID supplied by the client. Legacy conversations are separate.
func (s *Store) EnsurePersonalAssistant(ctx context.Context, actor string) (PersonalAssistant, error) {
	if owner, ok := strings.CutPrefix(actor, "user:"); !ok || owner == "" || owner == "session" || owner == "anonymous" {
		return PersonalAssistant{}, ErrAuthorizationDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PersonalAssistant{}, err
	}
	defer tx.Rollback()
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE principal_id = ? AND status = 'active')`, actor).Scan(&active); err != nil {
		return PersonalAssistant{}, err
	}
	if !active {
		return PersonalAssistant{}, ErrAuthorizationDenied
	}
	const read = `SELECT assistant_id, created_at FROM personal_assistants WHERE owner_id = ?`
	var assistant PersonalAssistant
	err = tx.QueryRowContext(ctx, read, actor).Scan(&assistant.ID, &assistant.CreatedAt)
	if !errors.Is(err, sql.ErrNoRows) {
		return assistant, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO personal_assistants(owner_id, assistant_id, created_at)
		VALUES (?, ?, ?) ON CONFLICT(owner_id) DO NOTHING`, actor, id(), now()); err != nil {
		return PersonalAssistant{}, err
	}
	if err := tx.QueryRowContext(ctx, read, actor).Scan(&assistant.ID, &assistant.CreatedAt); err != nil {
		return PersonalAssistant{}, err
	}
	if err := tx.Commit(); err != nil {
		return PersonalAssistant{}, err
	}
	return assistant, nil
}
