package work

import (
	"context"
	"database/sql"
	"errors"
)

// AttentionItem projects durable, actor-scoped records without running the
// model or creating chat messages/notifications. Kind and State are a typed
// read contract, never permission to perform a write.
type AttentionItem struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	OccurredAt  int64  `json:"occurred_at"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	ApprovalID  string `json:"approval_id,omitempty"`
	ObjectRef   string `json:"object_ref,omitempty"`
	Action      string `json:"action,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	StepID      string `json:"step_id,omitempty"`
	WaitID      string `json:"wait_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

type AttentionCursor struct {
	OccurredAt int64  `json:"occurred_at"`
	ID         string `json:"id"`
}

type AttentionPage struct {
	Items      []AttentionItem  `json:"items"`
	SnapshotAt int64            `json:"snapshot_at"`
	Next       *AttentionCursor `json:"next,omitempty"`
}

const attentionQuery = `WITH attention AS (
SELECT 'approval:'||a.approval_id AS id,'approval' AS kind,
 CASE WHEN a.decision='expired' OR (a.expires_at>0 AND a.expires_at<=:stamp)
 THEN 'expired' ELSE 'decision_required' END AS state,
 CASE WHEN a.expires_at>0 AND a.expires_at<=:stamp THEN a.expires_at
 WHEN a.decision='expired' AND a.decided_at>0 THEN a.decided_at ELSE a.created_at END AS occurred_at,
 a.expires_at AS expires_at,a.approval_id AS approval_id,a.object_ref AS object_ref,a.action AS action,
 '' AS task_id,'' AS step_id,'' AS wait_id,'' AS operation_id
FROM approvals a WHERE a.subject=:actor AND a.decision IN ('pending','expired')
UNION ALL
SELECT 'benchmark-timeout:'||c.command_id,'benchmark_wait','result_pending',c.created_at,
 0,'','','',r.task_id,b.step_id,w.wait_id,o.operation_id
FROM benchmark_runs r
JOIN benchmark_cases b ON b.task_id=r.task_id
JOIN operations o ON o.operation_id=b.operation_id AND o.task_id=b.task_id AND o.step_id=b.step_id
JOIN waits w ON w.wait_id='benchmark:'||b.step_id AND w.task_id=b.task_id AND w.step_id=b.step_id
 AND w.kind='external' AND w.match_key=o.operation_id AND w.state='open'
JOIN timers d ON d.wait_id=w.wait_id AND d.wait_version=w.wait_version AND d.state='delivered'
JOIN inbox_events i ON i.source='timer' AND i.event_id=d.inbox_event_id
 AND i.wait_id=w.wait_id AND i.state='applied' AND i.principal='system' AND i.kind='timer_due'
JOIN wait_events e ON e.source=i.source AND e.event_id=i.event_id
 AND e.wait_id=w.wait_id AND e.wait_version=w.wait_version AND e.closed_wait=0
JOIN outbox_commands c ON c.command_id=i.event_id||':follow-up'
 AND c.backend='runtime-wait' AND c.kind='wait_timeout_reconcile' AND c.target_kind='wait'
 AND c.target_id=w.wait_id AND c.target_version=w.wait_version AND c.task_id=b.task_id
JOIN outbox_receipts receipt ON receipt.command_id=c.command_id AND receipt.receiver='benchmark-timeout'
 AND receipt.payload_digest=c.payload_digest
WHERE r.actor=:actor AND NOT r.stop_requested AND c.state='acknowledged'
 AND b.phase IN ('submitting','following') AND o.state IN ('dispatched','running','reconcile')
)
SELECT id,kind,state,occurred_at,expires_at,approval_id,object_ref,action,task_id,step_id,wait_id,operation_id
FROM attention WHERE occurred_at<=:stamp
AND (:before_id='' OR occurred_at<:before_at OR (occurred_at=:before_at AND id<:before_id))
ORDER BY occurred_at DESC,id DESC LIMIT :count`

func (s *Store) ListAttention(ctx context.Context, actor string, stamp int64, limit int, before AttentionCursor) (AttentionPage, error) {
	if actor == "" || actor == "user:anonymous" {
		return AttentionPage{}, ErrAuthorizationDenied
	}
	if limit < 1 || limit > 100 || before.OccurredAt < 0 || len(before.ID) > 512 ||
		(before.OccurredAt == 0) != (before.ID == "") {
		return AttentionPage{}, errors.New("无效的待处理事项分页")
	}
	rows, err := s.db.QueryContext(ctx, attentionQuery, sql.Named("actor", actor), sql.Named("stamp", stamp),
		sql.Named("count", limit+1), sql.Named("before_id", before.ID), sql.Named("before_at", before.OccurredAt))
	if err != nil {
		return AttentionPage{}, err
	}
	defer rows.Close()
	page := AttentionPage{Items: make([]AttentionItem, 0, limit), SnapshotAt: stamp}
	for rows.Next() {
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1]
			page.Next = &AttentionCursor{OccurredAt: last.OccurredAt, ID: last.ID}
			break
		}
		var a AttentionItem
		if err := rows.Scan(&a.ID, &a.Kind, &a.State, &a.OccurredAt, &a.ExpiresAt, &a.ApprovalID, &a.ObjectRef, &a.Action,
			&a.TaskID, &a.StepID, &a.WaitID, &a.OperationID); err != nil {
			return AttentionPage{}, err
		}
		page.Items = append(page.Items, a)
	}
	if err := rows.Err(); err != nil {
		return AttentionPage{}, err
	}
	return page, nil
}
