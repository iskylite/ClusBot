package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// AttentionDetail reloads a previously seen item without treating its historic
// notification/card as permission or evidence of success.
type AttentionDetail struct {
	AttentionItem
	SnapshotAt int64           `json:"snapshot_at"`
	Source     AttentionSource `json:"source"`
}

type AttentionSource struct {
	Kind             string `json:"kind"`
	Ref              string `json:"ref"`
	RecordedAt       int64  `json:"recorded_at"`
	DecisionAt       int64  `json:"decision_at,omitempty"`
	ContractRevision int    `json:"contract_revision,omitempty"`
	PolicyVersion    int    `json:"policy_version,omitempty"`
	AssetRevision    int    `json:"asset_revision,omitempty"`
	WaitVersion      int    `json:"wait_version,omitempty"`
	OperationState   string `json:"operation_state,omitempty"`
}

func (s *Store) ReadAttention(ctx context.Context, actor, itemID string, stamp int64) (AttentionDetail, error) {
	if actor == "" || actor == "user:anonymous" {
		return AttentionDetail{}, ErrAuthorizationDenied
	}
	item := AttentionDetail{AttentionItem: AttentionItem{ID: itemID}, SnapshotAt: stamp}
	kind, ref, ok := strings.Cut(itemID, ":")
	if !ok || ref == "" || len(itemID) > 512 {
		return AttentionDetail{}, sql.ErrNoRows
	}
	switch kind {
	case "approval":
		var decision string
		err := s.db.QueryRowContext(ctx, `SELECT action,object_ref,decision,created_at,expires_at,decided_at,contract_revision,policy_version,asset_revision
FROM approvals WHERE approval_id=? AND subject=? AND created_at<=?`, ref, actor, stamp).
			Scan(&item.Action, &item.ObjectRef, &decision, &item.Source.RecordedAt, &item.ExpiresAt, &item.Source.DecisionAt,
				&item.Source.ContractRevision, &item.Source.PolicyVersion, &item.Source.AssetRevision)
		if err != nil {
			return AttentionDetail{}, err
		}
		item.Kind, item.ApprovalID = "approval", ref
		item.Source.Kind, item.Source.Ref = "approval", ref
		item.OccurredAt = item.Source.RecordedAt
		switch {
		case decision == "approved" || decision == "rejected":
			item.State = decision
			if item.Source.DecisionAt > 0 {
				item.OccurredAt = item.Source.DecisionAt
			}
		case item.ExpiresAt > 0 && stamp >= item.ExpiresAt:
			item.State, item.OccurredAt = "expired", item.ExpiresAt
		case decision == "expired":
			item.State = "expired"
			if item.Source.DecisionAt > 0 {
				item.OccurredAt = item.Source.DecisionAt
			}
		default:
			item.State = "decision_required"
		}
	case "benchmark-timeout":
		var currentVersion int
		var waitState, phase string
		var stopping bool
		err := s.db.QueryRowContext(ctx, attentionBenchmarkDetailQuery, ref, actor, stamp).
			Scan(&item.TaskID, &item.StepID, &item.WaitID, &item.OperationID,
				&item.OccurredAt, &item.Source.Ref, &item.Source.RecordedAt, &item.Source.WaitVersion,
				&currentVersion, &waitState, &item.Source.OperationState, &phase, &stopping)
		if err != nil {
			return AttentionDetail{}, err
		}
		item.Kind, item.Source.Kind = "benchmark_wait", "verified_timer_receipt"
		switch {
		case currentVersion != item.Source.WaitVersion:
			item.State = "replaced"
		case item.Source.OperationState == "succeeded" || item.Source.OperationState == "failed" || item.Source.OperationState == "cancelled":
			item.State = "result_recorded"
		case waitState == "closed":
			// A closed wait is not in itself evidence that the job passed.
			item.State = "wait_closed"
		case stopping:
			item.State = "stop_requested"
		case phase == "submitting" || phase == "following":
			item.State = "result_pending"
		default:
			item.State = "no_longer_pending"
		}
	default:
		return AttentionDetail{}, sql.ErrNoRows
	}
	return item, nil
}

const attentionBenchmarkDetailQuery = `SELECT r.task_id,b.step_id,w.wait_id,o.operation_id,c.created_at,
 i.event_id,receipt.received_at,d.wait_version,w.wait_version,w.state,o.state,b.phase,r.stop_requested
FROM outbox_commands c
JOIN waits w ON w.wait_id=c.target_id AND w.task_id=c.task_id AND w.kind='external'
JOIN benchmark_cases b ON b.task_id=w.task_id AND b.step_id=w.step_id AND w.wait_id='benchmark:'||b.step_id
JOIN benchmark_runs r ON r.task_id=b.task_id
JOIN operations o ON o.operation_id=b.operation_id AND o.operation_id=w.match_key AND o.task_id=b.task_id AND o.step_id=b.step_id
JOIN timers d ON d.wait_id=w.wait_id AND d.wait_version=c.target_version AND d.state='delivered'
JOIN inbox_events i ON i.source='timer' AND i.event_id=d.inbox_event_id AND c.command_id=i.event_id||':follow-up'
 AND i.wait_id=w.wait_id AND i.state='applied' AND i.principal='system' AND i.kind='timer_due'
JOIN wait_events e ON e.source=i.source AND e.event_id=i.event_id
 AND e.wait_id=d.wait_id AND e.wait_version=d.wait_version AND e.closed_wait=0
JOIN outbox_receipts receipt ON receipt.command_id=c.command_id AND receipt.receiver='benchmark-timeout'
 AND receipt.payload_digest=c.payload_digest
WHERE c.command_id=? AND r.actor=? AND receipt.received_at<=?
 AND c.backend='runtime-wait' AND c.kind='wait_timeout_reconcile' AND c.target_kind='wait' AND c.state='acknowledged'`

// IsAttentionNotFound deliberately conflates nonexistence and foreign ownership
// to avoid using the detail endpoint as an ownership oracle.
func IsAttentionNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
