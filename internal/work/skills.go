package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

var ErrSkill = errors.New("请选择工作范围并填写方法说明")

type SkillVersion struct {
	SkillID      string               `json:"skill_id"`
	Scope        string               `json:"scope_label"`
	Revision     int                  `json:"revision"`
	Version      int                  `json:"version"`
	BaseRevision int                  `json:"base_revision"`
	Instructions string               `json:"instructions"`
	Digest       string               `json:"sha256"`
	Active       bool                 `json:"active"`
	ApprovedAt   *int64               `json:"approved_at,omitempty"`
	Source       *SkillDocumentSource `json:"source,omitempty"`
}

func scanSkill(row scanner) (SkillVersion, error) {
	var v SkillVersion
	err := row.Scan(&v.SkillID, &v.Scope, &v.Revision, &v.Version, &v.BaseRevision, &v.Instructions,
		&v.Digest, &v.Active, &v.ApprovedAt)
	return v, err
}

const skillSelect = `SELECT s.id,s.scope_label,s.revision,v.version,v.base_revision,
v.instructions,v.sha256,coalesce(s.active_version=v.version,0),v.approved_at
FROM personal_skills s JOIN personal_skill_versions v ON v.skill_id=s.id`

func (s *Store) Skills(ctx context.Context) ([]SkillVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, skillSelect+" ORDER BY s.scope_label,v.version DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := []SkillVersion{}
	for rows.Next() {
		entry, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range versions {
		v := &versions[i]
		v.Source, err = readSkillDocumentSource(tx, v.SkillID, v.Version)
		if err != nil {
			return nil, err
		}
		if err := verifySkillProposalSources(tx, v.SkillID, v.Version); errors.Is(err, ErrExperienceSource) {
			v.Instructions = ""
		} else if err != nil {
			return nil, err
		}
	}
	return versions, tx.Commit()
}

func (s *Store) DraftSkill(ctx context.Context, scope, instructions string, revision int) (SkillVersion, error) {
	scope, instructions = strings.TrimSpace(scope), strings.TrimSpace(instructions)
	if scope == "" || instructions == "" || utf8.RuneCountInString(scope) > 40 || utf8.RuneCountInString(instructions) > 4000 {
		return SkillVersion{}, ErrSkill
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SkillVersion{}, err
	}
	defer tx.Rollback()
	version, err := draftSkillTx(tx, scope, instructions, revision, true)
	if err != nil {
		return version, err
	}
	return version, tx.Commit()
}

func draftSkillTx(tx *sql.Tx, scope, instructions string, revision int, reuse bool) (SkillVersion, error) {
	var skill string
	var current int
	err := tx.QueryRow("SELECT id,revision FROM personal_skills WHERE scope_label=?", scope).Scan(&skill, &current)
	if errors.Is(err, sql.ErrNoRows) && revision == 0 {
		skill = id()
		_, err = tx.Exec("INSERT INTO personal_skills(id,scope_label,revision) VALUES(?,?,0)", skill, scope)
	}
	if err != nil {
		return SkillVersion{}, err
	}
	if current != revision {
		return SkillVersion{}, ErrTaskRevision
	}
	hash := digest(instructions)
	if reuse {
		previous, err := scanSkill(tx.QueryRow(skillSelect+" WHERE s.id=? AND v.base_revision=? AND v.sha256=? ORDER BY v.version DESC LIMIT 1", skill, revision, hash))
		if err == nil {
			return previous, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return SkillVersion{}, err
		}
	}
	var version int
	if err = tx.QueryRow("SELECT coalesce(max(version),0)+1 FROM personal_skill_versions WHERE skill_id=?", skill).Scan(&version); err != nil {
		return SkillVersion{}, err
	}
	if _, err = tx.Exec("INSERT INTO personal_skill_versions(skill_id,version,base_revision,instructions,sha256) VALUES(?,?,?,?,?)",
		skill, version, revision, instructions, hash); err != nil {
		return SkillVersion{}, err
	}
	v, err := scanSkill(tx.QueryRow(skillSelect+" WHERE s.id=? AND v.version=?", skill, version))
	if err != nil {
		return SkillVersion{}, err
	}
	return v, nil
}

func (s *Store) ActivateSkill(ctx context.Context, skill string, version, revision int, hash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := activateSkillTx(tx, skill, version, revision, hash, "", ""); err != nil {
		return err
	}
	return tx.Commit()
}

func activateSkillTx(tx *sql.Tx, skill string, version, revision int, hash, proposal, replayID string) error {
	v, err := scanSkill(tx.QueryRow(skillSelect+" WHERE s.id=? AND v.version=?", skill, version))
	if err != nil {
		return err
	}
	if v.Digest != hash || digest(v.Instructions) != hash {
		return ErrState
	}
	var proposalID string
	if err := tx.QueryRow("SELECT coalesce((SELECT id FROM skill_proposals WHERE skill_id=? AND version=?),'')", skill, version).Scan(&proposalID); err != nil {
		return err
	}
	if (proposal != "" && proposal != proposalID) || (proposalID != "" && v.ApprovedAt == nil && proposal != proposalID) {
		return ErrSkillProposalConfirmation
	}
	if proposal != "" && revision != v.BaseRevision {
		return ErrTaskRevision
	}
	if err := verifySkillProposalSources(tx, skill, version); err != nil {
		return err
	}
	retry := v.Active && v.Revision == revision+1
	if !retry && (v.Revision != revision || (v.ApprovedAt == nil && v.BaseRevision != revision)) {
		return ErrTaskRevision
	}
	if proposal != "" {
		if err := requirePassedSkillReplay(tx, proposal, replayID, SkillProposalVersion{version, revision, hash}); err != nil {
			return err
		}
	}
	if retry {
		return nil
	}
	if _, err = tx.Exec("UPDATE personal_skill_versions SET approved_at=coalesce(approved_at,?) WHERE skill_id=? AND version=?", now(), skill, version); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE personal_skills SET active_version=?,revision=revision+1 WHERE id=?", version, skill); err != nil {
		return err
	}
	return nil
}

func (s *Store) DisableSkill(ctx context.Context, skill string, revision int) error {
	res, err := s.db.ExecContext(ctx, "UPDATE personal_skills SET active_version=NULL,revision=revision+1 WHERE id=? AND revision=?", skill, revision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrTaskRevision
	}
	return err
}

func (s *Store) skillForRun(ctx context.Context, run Run) (*SkillVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if run.Generation == 0 {
		boundary, err := readRerunInputBoundary(tx, run.ID)
		if err != nil {
			return nil, err
		}
		if boundary != nil {
			// Keep the source Run's binding (including an absent binding).
			// Current authorization is still checked below before use.
			if _, err = tx.Exec(`INSERT OR IGNORE INTO run_skills(run_id,skill_id,version,binding_source)
SELECT ?,skill_id,version,binding_source FROM run_skills WHERE run_id=?`, run.ID, boundary.SourceRunID); err != nil {
				return nil, err
			}
		} else {
			// A matching explicit choice wins. Select only for the actual run's
			// task scope; no free text, tool grants or chat-only run can apply it.
			var skill string
			var version int
			var source string
			err = tx.QueryRow(`SELECT b.skill_id,b.version,'conversation_selection'
FROM runs r JOIN tasks t ON t.id=r.task_id
JOIN conversation_skill_bindings b ON b.conversation_id=r.conversation_id
JOIN personal_skills s ON s.id=b.skill_id AND s.scope_label=t.scope_label
WHERE r.id=? UNION ALL
SELECT s.id,s.active_version,'scope_default' FROM personal_skills s JOIN tasks t ON t.scope_label=s.scope_label
WHERE t.id=? AND s.active_version IS NOT NULL LIMIT 1`, run.ID, run.TaskID).Scan(&skill, &version, &source)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				if err := rememberSkillReference(tx, skill, version); err != nil {
					return nil, err
				}
				if _, err = tx.Exec("INSERT OR IGNORE INTO run_skills(run_id,skill_id,version,binding_source) VALUES(?,?,?,?)",
					run.ID, skill, version, source); err != nil {
					return nil, err
				}
			}
		}
	}
	v, err := scanSkill(tx.QueryRow(skillSelect+" JOIN run_skills r ON r.skill_id=s.id AND r.version=v.version WHERE r.run_id=?", run.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if err := checkRunSkillVersion(ctx, tx, run.ID, v.SkillID, v.Version); err != nil {
		return nil, err
	}
	if digest(v.Instructions) != v.Digest {
		return nil, ErrReadCheckpoint
	}
	var goal string
	if err := tx.QueryRow("SELECT goal FROM tasks WHERE id=?", run.TaskID).Scan(&goal); err != nil {
		return nil, err
	}
	effective, err := applicableSkillInstructions(tx, v.SkillID, v.Version, goal)
	if errors.Is(err, ErrExperienceSource) {
		return nil, ErrReadCheckpoint
	}
	if err != nil {
		return nil, err
	}
	if effective == "" {
		return nil, nil
	}
	v.Instructions, v.Digest = effective, digest(effective)
	return &v, tx.Commit()
}

func checkRunSkillVersion(ctx context.Context, tx *sql.Tx, runID, skill string, version int) error {
	// Publishing a newer version preserves an in-flight binding. Disabling
	// the Skill or rolling back below the bound version revokes that binding.
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM run_skills r JOIN personal_skills s ON s.id=r.skill_id
JOIN personal_skill_versions v ON v.skill_id=r.skill_id AND v.version=r.version
WHERE r.run_id=? AND r.skill_id=? AND r.version=? AND v.approved_at IS NOT NULL
AND s.active_version>=r.version)`, runID, skill, version).Scan(&allowed)
	if err == nil && !allowed {
		return ErrReadCheckpoint
	}
	if err == nil {
		var source string
		if err := tx.QueryRowContext(ctx, "SELECT binding_source FROM run_skills WHERE run_id=?", runID).Scan(&source); err != nil {
			return err
		}
		if source == "conversation_selection" {
			if err := verifySkillProposalSources(tx, skill, version); errors.Is(err, ErrExperienceSource) {
				return ErrReadCheckpoint
			} else if err != nil {
				return err
			}
		}
	}
	return err
}
