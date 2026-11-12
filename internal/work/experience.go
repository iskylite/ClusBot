package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type ExperienceRef struct {
	ArtifactID string `json:"artifact_id"`
	Digest     string `json:"sha256"`
	Revision   int    `json:"acceptance_revision"`
	Title      string `json:"title"`
	Example    string `json:"example"`
}

// An accepted artifact can guide the form of similar work in the same named
// scope. Evidence from earlier tasks never becomes evidence about current facts.
func (s *Store) routeTaskExperience(ctx context.Context, run Run) (*ExperienceRef, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var source, hash sql.NullString
	var revision sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT artifact_id,artifact_digest,acceptance_revision
FROM experience_routes WHERE run_id=?`, run.ID).Scan(&source, &hash, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		if run.Generation != 0 {
			return nil, nil
		}
		source, hash, revision, err = selectExperience(ctx, tx, run.TaskID)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO experience_routes VALUES(?,1,?,?,?,?)",
			run.ID, source, hash, revision, now()); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if !source.Valid {
		return nil, tx.Commit()
	}
	ref, err := loadExperience(ctx, tx, source.String, hash.String, revision.Int64)
	if err != nil {
		return nil, err
	}
	return ref, tx.Commit()
}

func loadExperience(ctx context.Context, tx *sql.Tx, source, hash string, revision int64) (*ExperienceRef, error) {
	var example, title string
	err := tx.QueryRowContext(ctx, `SELECT a.content,t.title FROM artifacts a
JOIN tasks t ON t.id=a.task_id
JOIN task_acceptances x ON x.artifact_id=a.id AND x.task_id=t.id AND x.sha256=a.sha256
JOIN conversations c ON c.id=t.conversation_id
WHERE a.id=? AND a.sha256=? AND x.revision=? AND c.archived=0`,
		source, hash, revision).Scan(&example, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReadCheckpoint
	}
	if err != nil {
		return nil, err
	}
	if digest(example) != hash {
		return nil, ErrReadCheckpoint
	}
	excerpt := []rune(example)
	ref := &ExperienceRef{ArtifactID: source, Digest: hash, Revision: int(revision), Title: title,
		Example: string(excerpt[:min(len(excerpt), 800)])}
	return ref, nil
}

func selectExperience(ctx context.Context, tx *sql.Tx, taskID string) (sql.NullString, sql.NullString, sql.NullInt64, error) {
	var scope, goal string
	if err := tx.QueryRowContext(ctx, "SELECT scope_label,goal FROM tasks WHERE id=?", taskID).Scan(&scope, &goal); err != nil {
		return sql.NullString{}, sql.NullString{}, sql.NullInt64{}, err
	}
	return selectExperienceForGoal(ctx, tx, taskID, scope, goal, "[]")
}

func selectExperienceForGoal(ctx context.Context, tx *sql.Tx, taskID, scope, goal, excludedTasks string) (sql.NullString, sql.NullString, sql.NullInt64, error) {
	var source, hash sql.NullString
	var revision sql.NullInt64
	if scope == "" || len([]rune(strings.TrimSpace(goal))) < 4 {
		return source, hash, revision, nil
	}
	err := tx.QueryRowContext(ctx, `SELECT a.id,a.sha256,x.revision FROM tasks earlier
JOIN conversations c ON c.id=earlier.conversation_id
JOIN task_acceptances x ON x.task_id=earlier.id
JOIN artifacts a ON a.id=x.artifact_id AND a.sha256=x.sha256
JOIN run_bindings b ON b.run_id=a.run_id
JOIN task_revisions v ON v.task_id=earlier.id AND v.revision=b.task_revision
WHERE earlier.id<>? AND earlier.scope_label=? AND v.scope_label=earlier.scope_label
AND lower(trim(v.goal))=lower(trim(?))
AND c.archived=0
AND earlier.id NOT IN (SELECT value FROM json_each(?))
ORDER BY x.accepted_at DESC,x.revision DESC,a.id LIMIT 1`, taskID, scope, goal, excludedTasks).Scan(&source, &hash, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return source, hash, revision, err
}
