package work

import (
	"context"
	"time"
)

// AssistantInventoryEvidenceLimit bounds the inventory walk before a model request.
const AssistantInventoryEvidenceLimit = 1200

// AuthorizeAssistantInventoryExport checks the current, exact-object grants
// for an inventory summary. A read grant alone does not authorize model
// egress. Parameter-constrained grants are deliberately not interpreted by
// this entry point, so only an unconstrained grant can authorize the export.
// The decision is recorded before the caller makes the in-flight request.
func (s *Store) AuthorizeAssistantInventoryExport(ctx context.Context, principal string, refs []string, at time.Time) (bool, error) {
	if principal == "" || len(refs) == 0 || len(refs) > 2048 {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stamp := at.UnixMilli()
	seen := make(map[string]bool, len(refs))
	type decision struct{ ref, outcome, reason string }
	decisions := make([]decision, 0, len(refs))
	allowed := true
	for _, ref := range refs {
		if ref == "" || seen[ref] {
			if ref == "" {
				return false, nil
			}
			continue
		}
		seen[ref] = true
		var readable, exportable bool
		err = tx.QueryRowContext(ctx, `SELECT
			EXISTS(SELECT 1 FROM authorizations WHERE subject=? AND action='read' AND object_ref=?
				AND revoked_at=0 AND valid_from<=? AND (expires_at=0 OR expires_at>?)),
			EXISTS(SELECT 1 FROM authorizations WHERE subject=? AND action='export_model' AND object_ref=?
				AND param_constraints='{}' AND revoked_at=0 AND valid_from<=? AND (expires_at=0 OR expires_at>?))`,
			principal, ref, stamp, stamp, principal, ref, stamp, stamp).Scan(&readable, &exportable)
		if err != nil {
			return false, err
		}
		outcome, reason := "denied", "缺少当前有效的读取或模型外发授权"
		if readable && exportable {
			outcome, reason = "allowed", "有当前有效的精确对象读取和模型外发授权"
		} else {
			allowed = false
		}
		decisions = append(decisions, decision{ref: ref, outcome: outcome, reason: reason})
	}
	for _, item := range decisions {
		if _, err = tx.ExecContext(ctx, `INSERT INTO authorization_decisions
			(decision_id,subject,action,object_ref,outcome,reason,decided_at,in_flight)
			VALUES(?,?,'export_model',?,?,?,?,?)`, id(), principal, item.ref, item.outcome, item.reason, stamp, boolToInt(allowed)); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return allowed, nil
}
