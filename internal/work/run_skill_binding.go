package work

import (
	"context"
	"database/sql"
	"errors"
)

type RunSkillBinding struct {
	RunID     string             `json:"run_id"`
	TaskID    string             `json:"task_id,omitempty"`
	RunStatus string             `json:"run_status"`
	Binding   *BoundSkillVersion `json:"binding"`
}

type BoundSkillVersion struct {
	SkillID         string `json:"skill_id"`
	Scope           string `json:"scope"`
	Version         int    `json:"version"`
	ActiveVersion   *int   `json:"active_version,omitempty"`
	SelectionSource string `json:"selection_source"`
	Source          string `json:"source"`
	Lifecycle       string `json:"lifecycle"`
	Validity        string `json:"validity"`
}

// ReadRunSkillBinding projects the execution's actual persisted binding.
// It does not select a Skill, expose any instructions, or authorize execution.
// Runs currently belong to the local authenticated session, not per-user rows.
func (s *Store) ReadRunSkillBinding(ctx context.Context, actor, runID string) (RunSkillBinding, error) {
	if actor != "user:session" {
		return RunSkillBinding{}, sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunSkillBinding{}, err
	}
	defer tx.Rollback()
	var out RunSkillBinding
	var control, goal string
	err = tx.QueryRowContext(ctx, `SELECT r.id,coalesce(r.task_id,''),r.status,r.control,coalesce(t.goal,'')
FROM runs r LEFT JOIN tasks t ON t.id=r.task_id WHERE r.id=?`, runID).
		Scan(&out.RunID, &out.TaskID, &out.RunStatus, &control, &goal)
	if err != nil {
		return RunSkillBinding{}, err
	}
	b := BoundSkillVersion{Source: "recorded_run", Lifecycle: "historical", Validity: "valid"}
	var active sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT r.skill_id,r.version,s.scope_label,s.active_version,r.binding_source
FROM run_skills r JOIN personal_skills s ON s.id=r.skill_id WHERE r.run_id=?`, runID).
		Scan(&b.SkillID, &b.Version, &b.Scope, &active, &b.SelectionSource)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return RunSkillBinding{}, err
	}
	out.Binding = &b
	if active.Valid {
		version := int(active.Int64)
		b.ActiveVersion = &version
	}
	if control == "" && (out.RunStatus == "queued" || out.RunStatus == "running") {
		b.Lifecycle = "current_run"
	}
	if err := checkRunSkillVersion(ctx, tx, runID, b.SkillID, b.Version); err != nil {
		if errors.Is(err, ErrReadCheckpoint) {
			b.Validity = "revoked"
			return out, nil
		}
		return RunSkillBinding{}, err
	}
	instructions, err := applicableSkillInstructions(tx, b.SkillID, b.Version, goal)
	switch {
	case errors.Is(err, ErrExperienceSource), errors.Is(err, ErrReadCheckpoint), errors.Is(err, ErrState), errors.Is(err, sql.ErrNoRows):
		b.Validity = "unavailable"
	case err != nil:
		return RunSkillBinding{}, err
	case instructions == "":
		b.Validity = "not_applicable"
	}
	return out, nil
}
