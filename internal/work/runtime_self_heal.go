package work

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type SelfHealPolicyInput struct {
	PolicyKey            string   `json:"policy_key"`
	Version              int      `json:"version"`
	Subject              string   `json:"subject"`
	AssetScope           string   `json:"asset_scope,omitempty"`
	ValidFrom            int64    `json:"valid_from,omitempty"`
	ValidUntil           int64    `json:"valid_until,omitempty"`
	Preconditions        []string `json:"preconditions,omitempty"`
	ActionCapability     string   `json:"action_capability"`
	VerificationStandard string   `json:"verification_standard,omitempty"`
	PerNodeWindowMS      int64    `json:"per_node_window_ms,omitempty"`
	PerNodeMaxWrites     int      `json:"per_node_max_writes,omitempty"`
	ClusterMaxConcurrent int      `json:"cluster_max_concurrent,omitempty"`
	State                string   `json:"state,omitempty"`
	ObserveOnly          bool     `json:"observe_only"`
	PublishedBy          string   `json:"published_by,omitempty"`
}

type SelfHealPolicy struct {
	PolicyID             string `json:"policy_id"`
	PolicyKey            string `json:"policy_key"`
	Version              int    `json:"version"`
	Subject              string `json:"subject"`
	AssetScope           string `json:"asset_scope"`
	ValidFrom            int64  `json:"valid_from"`
	ValidUntil           int64  `json:"valid_until"`
	Preconditions        string `json:"preconditions"`
	ActionCapability     string `json:"action_capability"`
	VerificationStandard string `json:"verification_standard"`
	PerNodeWindowMS      int64  `json:"per_node_window_ms"`
	PerNodeMaxWrites     int    `json:"per_node_max_writes"`
	ClusterMaxConcurrent int    `json:"cluster_max_concurrent"`
	State                string `json:"state"`
	ObserveOnly          bool   `json:"observe_only"`
	PublishedBy          string `json:"published_by"`
	PublishedAt          int64  `json:"published_at"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"updated_at"`
}

type SelfHealExecution struct {
	ExecutionID         string `json:"execution_id"`
	PolicyID            string `json:"policy_id"`
	ObjectRef           string `json:"object_ref"`
	OperationID         string `json:"operation_id,omitempty"`
	State               string `json:"state"`
	BlockReason         string `json:"block_reason,omitempty"`
	VerificationOutcome string `json:"verification_outcome,omitempty"`
	CreatedAt           int64  `json:"created_at"`
	UpdatedAt           int64  `json:"updated_at"`
}

func (s *Store) UpsertSelfHealPolicy(ctx context.Context, in SelfHealPolicyInput) (SelfHealPolicy, error) {
	in.PolicyKey = strings.TrimSpace(in.PolicyKey)
	in.Subject = strings.TrimSpace(in.Subject)
	in.ActionCapability = strings.TrimSpace(in.ActionCapability)
	in.State = strings.TrimSpace(in.State)
	if in.Version < 1 {
		in.Version = 1
	}
	if in.PolicyKey == "" || in.Subject == "" || in.ActionCapability == "" {
		return SelfHealPolicy{}, errors.New("policy_key、subject 和 action_capability 不能为空")
	}
	if in.State == "" {
		in.State = "active"
	}
	switch in.State {
	case "draft", "published", "active", "suspended", "revoked":
	default:
		return SelfHealPolicy{}, fmt.Errorf("无效的自愈策略状态：%s", in.State)
	}
	if in.AssetScope == "" {
		in.AssetScope = "{}"
	}
	if in.PerNodeWindowMS <= 0 {
		in.PerNodeWindowMS = 1800000
	}
	if in.PerNodeMaxWrites <= 0 {
		in.PerNodeMaxWrites = 1
	}
	if in.ClusterMaxConcurrent <= 0 {
		in.ClusterMaxConcurrent = 5
	}
	stamp := now()
	if in.ValidFrom == 0 {
		in.ValidFrom = stamp
	}
	publishedAt := int64(0)
	if in.State == "published" || in.State == "active" {
		publishedAt = stamp
		if in.PublishedBy == "" {
			in.PublishedBy = in.Subject
		}
	}
	policyID := id()
	precondJSON := toJSON(nonNilStrings(in.Preconditions))

	_, err := s.db.ExecContext(ctx, `INSERT INTO self_heal_policies(
policy_id,policy_key,version,subject,asset_scope,valid_from,valid_until,preconditions,action_capability,
verification_standard,per_node_window_ms,per_node_max_writes,cluster_max_concurrent,state,observe_only,
published_by,published_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(policy_key,version) DO UPDATE SET
subject=excluded.subject,asset_scope=excluded.asset_scope,valid_from=excluded.valid_from,
valid_until=excluded.valid_until,preconditions=excluded.preconditions,action_capability=excluded.action_capability,
verification_standard=excluded.verification_standard,per_node_window_ms=excluded.per_node_window_ms,
per_node_max_writes=excluded.per_node_max_writes,cluster_max_concurrent=excluded.cluster_max_concurrent,
state=excluded.state,observe_only=excluded.observe_only,published_by=excluded.published_by,
published_at=excluded.published_at,updated_at=excluded.updated_at`,
		policyID, in.PolicyKey, in.Version, in.Subject, in.AssetScope, in.ValidFrom, in.ValidUntil,
		precondJSON, in.ActionCapability, in.VerificationStandard, in.PerNodeWindowMS,
		in.PerNodeMaxWrites, in.ClusterMaxConcurrent, in.State, boolToInt(in.ObserveOnly),
		in.PublishedBy, publishedAt, stamp, stamp)
	if err != nil {
		return SelfHealPolicy{}, err
	}
	return s.readSelfHealPolicyByKey(ctx, in.PolicyKey, in.Version)
}

func (s *Store) readSelfHealPolicyByKey(ctx context.Context, key string, version int) (SelfHealPolicy, error) {
	var p SelfHealPolicy
	var observe int
	err := s.db.QueryRowContext(ctx, `SELECT policy_id,policy_key,version,subject,asset_scope,valid_from,valid_until,
preconditions,action_capability,verification_standard,per_node_window_ms,per_node_max_writes,
cluster_max_concurrent,state,observe_only,published_by,published_at,created_at,updated_at
FROM self_heal_policies WHERE policy_key=? AND version=?`, key, version).Scan(
		&p.PolicyID, &p.PolicyKey, &p.Version, &p.Subject, &p.AssetScope, &p.ValidFrom, &p.ValidUntil,
		&p.Preconditions, &p.ActionCapability, &p.VerificationStandard, &p.PerNodeWindowMS,
		&p.PerNodeMaxWrites, &p.ClusterMaxConcurrent, &p.State, &observe, &p.PublishedBy,
		&p.PublishedAt, &p.CreatedAt, &p.UpdatedAt)
	p.ObserveOnly = observe != 0
	return p, err
}

func (s *Store) GetSelfHealPolicy(ctx context.Context, policyID string) (SelfHealPolicy, error) {
	var p SelfHealPolicy
	var observe int
	err := s.db.QueryRowContext(ctx, `SELECT policy_id,policy_key,version,subject,asset_scope,valid_from,valid_until,
preconditions,action_capability,verification_standard,per_node_window_ms,per_node_max_writes,
cluster_max_concurrent,state,observe_only,published_by,published_at,created_at,updated_at
FROM self_heal_policies WHERE policy_id=?`, strings.TrimSpace(policyID)).Scan(
		&p.PolicyID, &p.PolicyKey, &p.Version, &p.Subject, &p.AssetScope, &p.ValidFrom, &p.ValidUntil,
		&p.Preconditions, &p.ActionCapability, &p.VerificationStandard, &p.PerNodeWindowMS,
		&p.PerNodeMaxWrites, &p.ClusterMaxConcurrent, &p.State, &observe, &p.PublishedBy,
		&p.PublishedAt, &p.CreatedAt, &p.UpdatedAt)
	p.ObserveOnly = observe != 0
	return p, err
}

func (s *Store) ListSelfHealPolicies(ctx context.Context) ([]SelfHealPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT policy_id,policy_key,version,subject,asset_scope,valid_from,valid_until,
preconditions,action_capability,verification_standard,per_node_window_ms,per_node_max_writes,
cluster_max_concurrent,state,observe_only,published_by,published_at,created_at,updated_at
FROM self_heal_policies ORDER BY updated_at DESC, policy_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SelfHealPolicy{}
	for rows.Next() {
		var p SelfHealPolicy
		var observe int
		if err := rows.Scan(
			&p.PolicyID, &p.PolicyKey, &p.Version, &p.Subject, &p.AssetScope, &p.ValidFrom, &p.ValidUntil,
			&p.Preconditions, &p.ActionCapability, &p.VerificationStandard, &p.PerNodeWindowMS,
			&p.PerNodeMaxWrites, &p.ClusterMaxConcurrent, &p.State, &observe, &p.PublishedBy,
			&p.PublishedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.ObserveOnly = observe != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// ExecuteIncidentSelfHeal enforces bounded self-heal policy guards (active window, asset
// freeze/loan/conflict checks, per-node rolling rate limit, cluster concurrency, observe_only),
// dispatches a bound MCP capability if configured, and optionally runs independent EffectChecker.
func (s *Store) ExecuteIncidentSelfHeal(ctx context.Context, subject, incidentID, policyID, objectRef string, checker EffectChecker) (SelfHealExecution, error) {
	if strings.TrimSpace(subject) == "" {
		return SelfHealExecution{}, ErrAuthorizationDenied
	}
	objectRef = strings.TrimSpace(objectRef)
	if strings.TrimSpace(policyID) == "" || objectRef == "" {
		return SelfHealExecution{}, errors.New("policy_id 和 object_ref 不能为空")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SelfHealExecution{}, err
	}
	defer tx.Rollback()

	if incidentID != "" {
		if err := incidentExistsAndWritable(tx, incidentID); err != nil {
			return SelfHealExecution{}, err
		}
	}

	var p SelfHealPolicy
	var observe int
	err = tx.QueryRowContext(ctx, `SELECT policy_id,policy_key,version,subject,asset_scope,valid_from,valid_until,
preconditions,action_capability,verification_standard,per_node_window_ms,per_node_max_writes,
cluster_max_concurrent,state,observe_only,published_by,published_at,created_at,updated_at
FROM self_heal_policies WHERE policy_id=?`, policyID).Scan(
		&p.PolicyID, &p.PolicyKey, &p.Version, &p.Subject, &p.AssetScope, &p.ValidFrom, &p.ValidUntil,
		&p.Preconditions, &p.ActionCapability, &p.VerificationStandard, &p.PerNodeWindowMS,
		&p.PerNodeMaxWrites, &p.ClusterMaxConcurrent, &p.State, &observe, &p.PublishedBy,
		&p.PublishedAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return SelfHealExecution{}, err
	}
	p.ObserveOnly = observe != 0

	stamp := now()
	execID := id()
	block := func(reason string) (SelfHealExecution, error) {
		ex := SelfHealExecution{
			ExecutionID: execID,
			PolicyID:    p.PolicyID,
			ObjectRef:   objectRef,
			State:       "blocked",
			BlockReason: reason,
			CreatedAt:   stamp,
			UpdatedAt:   stamp,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO self_heal_executions(execution_id,policy_id,object_ref,operation_id,state,block_reason,verification_outcome,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`, ex.ExecutionID, ex.PolicyID, ex.ObjectRef, nil, ex.State, ex.BlockReason, "", stamp, stamp); err != nil {
			return SelfHealExecution{}, err
		}
		if err := tx.Commit(); err != nil {
			return SelfHealExecution{}, err
		}
		return ex, nil
	}

	if p.State != "active" {
		return block(fmt.Sprintf("策略状态为 %s，未激活", p.State))
	}
	if stamp < p.ValidFrom || (p.ValidUntil > 0 && stamp > p.ValidUntil) {
		return block("当前时间不在策略有效期内")
	}

	var frozenAt int64
	var assetState string
	err = tx.QueryRowContext(ctx, `SELECT state,frozen_at FROM ops_assets WHERE object_ref=?`, objectRef).Scan(&assetState, &frozenAt)
	if err == nil {
		if frozenAt != 0 || assetState == AssetStateMaintenance {
			return block("目标资产已冻结或处于维护状态")
		}
	}
	var activeLoans int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_loans WHERE object_ref=? AND state='lent'`, objectRef).Scan(&activeLoans); err == nil && activeLoans > 0 {
		return block("目标资产处于借出状态，禁止自动自愈写操作")
	}
	var openConflicts int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_conflicts WHERE object_ref=? AND state='open'`, objectRef).Scan(&openConflicts); err == nil && openConflicts > 0 {
		return block("目标资产存在未解决的身份冲突，禁止自动自愈")
	}

	windowStart := stamp - p.PerNodeWindowMS
	var nodeWrites int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM self_heal_executions WHERE policy_id=? AND object_ref=? AND created_at>=? AND state IN ('admitted','dispatched','verified','failed')`,
		p.PolicyID, objectRef, windowStart).Scan(&nodeWrites); err != nil {
		return SelfHealExecution{}, err
	}
	if nodeWrites >= p.PerNodeMaxWrites {
		return block("超出单节点滚动窗口写操作上限")
	}

	var clusterActive int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM self_heal_executions WHERE policy_id=? AND state IN ('admitted','dispatched')`,
		p.PolicyID).Scan(&clusterActive); err != nil {
		return SelfHealExecution{}, err
	}
	if clusterActive >= p.ClusterMaxConcurrent {
		return block("超出集群自愈并发上限")
	}

	if p.ObserveOnly {
		ex := SelfHealExecution{
			ExecutionID:         execID,
			PolicyID:            p.PolicyID,
			ObjectRef:           objectRef,
			State:               "proposed",
			VerificationOutcome: "observe_only",
			CreatedAt:           stamp,
			UpdatedAt:           stamp,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO self_heal_executions(execution_id,policy_id,object_ref,operation_id,state,block_reason,verification_outcome,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`, ex.ExecutionID, ex.PolicyID, ex.ObjectRef, nil, ex.State, "", ex.VerificationOutcome, stamp, stamp); err != nil {
			return SelfHealExecution{}, err
		}
		if err := tx.Commit(); err != nil {
			return SelfHealExecution{}, err
		}
		return ex, nil
	}

	authDecision, err := decideAuthorization(tx, subject, p.ActionCapability, objectRef, "self_heal", stamp, false)
	if err != nil {
		return SelfHealExecution{}, err
	}
	if authDecision.Outcome != "allowed" {
		return block(authDecision.Reason)
	}

	taskID, convID := id(), id()
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)`,
		convID, "自愈 "+p.PolicyKey, stamp); err != nil {
		return SelfHealExecution{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,conversation_id,title,goal,scope_label,status,status_reason,reason,updated_at)
VALUES(?,?,?,?,?,'active','queued','有界自动自愈',?)`,
		taskID, convID, "自愈 "+objectRef, p.ActionCapability, "self_heal", stamp); err != nil {
		return SelfHealExecution{}, err
	}
	if _, err := transitionTaskState(tx, taskID, EventCreate, TaskGuard{Actor: subject}, stamp); err != nil {
		return SelfHealExecution{}, err
	}

	op, err := registerOperation(tx, Operation{
		TaskID:           taskID,
		CapabilityKey:    p.ActionCapability,
		TargetRefs:       toJSON([]string{objectRef}),
		ParamsDigest:     digest(p.PolicyID + ":" + objectRef),
		IdempotencyKey:   "self_heal:" + execID,
		AuthorizationRef: subject,
	}, stamp)
	if err != nil {
		return SelfHealExecution{}, err
	}
	disp, err := dispatchOperationCommitted(tx, op.OperationID, subject, stamp)
	if err != nil {
		return SelfHealExecution{}, err
	}
	if !disp.Dispatched {
		return block(disp.Reason)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO self_heal_executions(execution_id,policy_id,object_ref,operation_id,state,block_reason,verification_outcome,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`, execID, p.PolicyID, objectRef, op.OperationID, "dispatched", "", "", stamp, stamp); err != nil {
		return SelfHealExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return SelfHealExecution{}, err
	}

	nextState := "dispatched"
	outcome := "dispatched"
	_, _, boundOK, callErr := s.CallBoundMCPTargetTool(ctx, p.ActionCapability, "", objectRef, map[string]any{
		"idempotency_key": "self_heal:" + execID,
	})
	if callErr != nil {
		// Unknown/lost reply during external write must enter reconcile_required, never immediate fail.
		s.markSelfHealOperationReconcile(ctx, op.OperationID)
		nextState = "dispatched"
		outcome = "reconcile_required: " + callErr.Error()
	} else {
		if !boundOK {
			outcome = "operation_dispatched"
		}
		if checker != nil {
			verified, evidence, checkErr := checker(ctx, objectRef)
			if checkErr != nil {
				s.markSelfHealOperationReconcile(ctx, op.OperationID)
				nextState = "dispatched"
				outcome = "reconcile_required: " + checkErr.Error()
			} else if verified {
				s.finishSelfHealOperation(ctx, op.OperationID, RuntimeOperationSucceeded)
				nextState = "verified"
				outcome = evidence
			} else {
				s.finishSelfHealOperation(ctx, op.OperationID, RuntimeOperationFailed)
				nextState = "failed"
				outcome = evidence
			}
		} else {
			s.finishSelfHealOperation(ctx, op.OperationID, RuntimeOperationSucceeded)
			nextState = "verified"
		}
	}

	doneStamp := now()
	if _, err := s.db.ExecContext(ctx, `UPDATE self_heal_executions SET state=?, verification_outcome=?, updated_at=? WHERE execution_id=?`,
		nextState, outcome, doneStamp, execID); err != nil {
		return SelfHealExecution{}, err
	}
	if nextState == "verified" && incidentID != "" {
		tx2, err := s.db.BeginTx(ctx, nil)
		if err == nil {
			_ = recordRecoveryVerification(tx2, incidentID, true, outcome, doneStamp)
			_ = tx2.Commit()
		}
	}
	return SelfHealExecution{
		ExecutionID:         execID,
		PolicyID:            p.PolicyID,
		ObjectRef:           objectRef,
		OperationID:         op.OperationID,
		State:               nextState,
		VerificationOutcome: outcome,
		CreatedAt:           stamp,
		UpdatedAt:           doneStamp,
	}, nil
}

func (s *Store) markSelfHealOperationReconcile(ctx context.Context, operationID string) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	stamp := now()
	_, _ = transitionOperation(tx, operationID, RuntimeOperationRunning, stamp)
	_ = mapOutcomeUnknownToReconcile(tx, operationID, stamp)
	_ = tx.Commit()
}

func (s *Store) finishSelfHealOperation(ctx context.Context, operationID string, target RuntimeOperationState) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	stamp := now()
	_, _ = transitionOperation(tx, operationID, RuntimeOperationRunning, stamp)
	_, _ = transitionOperation(tx, operationID, target, stamp)
	_ = tx.Commit()
}
