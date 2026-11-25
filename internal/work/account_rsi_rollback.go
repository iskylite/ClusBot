package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// RollbackSkillProposal applies only to the private active version identified
// by the same immutable digest and skill revision presented for human review.
func (s *Store) RollbackSkillProposal(ctx context.Context, proposalID string, target SkillProposalVersion, disable bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var skill string
	var version, revision int
	var active, predecessor sql.NullInt64
	var hash, body string
	err = tx.QueryRowContext(ctx, `SELECT p.skill_id,p.version,s.revision,s.active_version,p.base_version,v.sha256,v.instructions
FROM main.skill_proposals p JOIN main.account_rsi_skills k ON k.skill_id=p.skill_id
JOIN main.personal_skills s ON s.id=k.skill_id JOIN main.personal_skill_versions v ON v.skill_id=s.id AND v.version=p.version
WHERE p.id=? AND EXISTS(SELECT 1 FROM main.account_business_owner WHERE singleton=1)`, proposalID).
		Scan(&skill, &version, &revision, &active, &predecessor, &hash, &body)
	if err != nil {
		return err
	}
	if revision != target.Revision || version != target.Version || !active.Valid || int(active.Int64) != version {
		return ErrTaskRevision
	}
	if target.Digest == "" || digest(body) != hash || hash != target.Digest {
		return ErrState
	}
	var next any
	if !disable && predecessor.Valid {
		if predecessor.Int64 >= int64(version) {
			return ErrState
		}
		if err := verifySkillProposalSources(tx, skill, int(predecessor.Int64)); err != nil {
			return err
		}
		var valid bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.personal_skill_versions WHERE skill_id=? AND version=? AND approved_at IS NOT NULL)`,
			skill, predecessor.Int64).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrState
		}
		next = predecessor.Int64
	}
	if _, err = tx.ExecContext(ctx, "UPDATE main.personal_skills SET active_version=?,revision=revision+1 WHERE id=?", next, skill); err != nil {
		return err
	}
	var owner string
	err = tx.QueryRowContext(ctx, "SELECT principal_id FROM main.account_business_owner WHERE singleton=1").Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || strings.TrimSpace(owner) == "" {
		return ErrAccountDenied
	}
	if err != nil {
		return err
	}
	action, note := "private_skill_rollback", "已恢复本账号已批准的上一版本"
	if next == nil {
		action, note = "private_skill_disabled", "已停用本账号的私有方法；共享设置未改变"
	}
	if err := recordAudit(tx, AuditEvent{Actor: owner, Event: action, DetailRef: proposalID, Summary: note, OccurredAt: now()}); err != nil {
		return err
	}
	return tx.Commit()
}
