package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"clusbot/internal/mcpclient"
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
		in.State = "draft"
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

const selfHealPolicyColumns = `policy_id,policy_key,version,subject,asset_scope,valid_from,valid_until,
preconditions,action_capability,verification_standard,per_node_window_ms,per_node_max_writes,
cluster_max_concurrent,state,observe_only,published_by,published_at,created_at,updated_at`

func scanSelfHealPolicy(row scanner) (SelfHealPolicy, error) {
	var p SelfHealPolicy
	var observe int
	err := row.Scan(
		&p.PolicyID, &p.PolicyKey, &p.Version, &p.Subject, &p.AssetScope, &p.ValidFrom, &p.ValidUntil,
		&p.Preconditions, &p.ActionCapability, &p.VerificationStandard, &p.PerNodeWindowMS,
		&p.PerNodeMaxWrites, &p.ClusterMaxConcurrent, &p.State, &observe, &p.PublishedBy, &p.PublishedAt,
		&p.CreatedAt, &p.UpdatedAt)
	p.ObserveOnly = observe != 0
	return p, err
}

func (s *Store) readSelfHealPolicyByKey(ctx context.Context, key string, version int) (SelfHealPolicy, error) {
	return scanSelfHealPolicy(s.db.QueryRowContext(ctx, `SELECT `+selfHealPolicyColumns+` FROM self_heal_policies WHERE policy_key=? AND version=?`, key, version))
}

func (s *Store) GetSelfHealPolicy(ctx context.Context, policyID string) (SelfHealPolicy, error) {
	return scanSelfHealPolicy(s.db.QueryRowContext(ctx, `SELECT `+selfHealPolicyColumns+` FROM self_heal_policies WHERE policy_id=?`, strings.TrimSpace(policyID)))
}

func (s *Store) ListSelfHealPolicies(ctx context.Context) ([]SelfHealPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selfHealPolicyColumns+` FROM self_heal_policies ORDER BY updated_at DESC, policy_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SelfHealPolicy{}
	for rows.Next() {
		p, err := scanSelfHealPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProposeIncidentSelfHeal is the observe-only entry point. The check is made
// in the same transaction as the proposal, so changing the policy between
// an HTTP preview and this call cannot turn a read into a repair.
func (s *Store) ProposeIncidentSelfHeal(ctx context.Context, subject, incidentID, policyID, objectRef string) (SelfHealExecution, error) {
	return s.executeIncidentSelfHeal(ctx, subject, incidentID, policyID, objectRef, nil, true)
}

// ExecuteIncidentSelfHeal is for the controlled runtime only. Do not call it
// from the generic HTTP, model, fault-observation or timer paths.
func (s *Store) ExecuteIncidentSelfHeal(ctx context.Context, subject, incidentID, policyID, objectRef string, checker EffectChecker) (SelfHealExecution, error) {
	return s.executeIncidentSelfHeal(ctx, subject, incidentID, policyID, objectRef, checker, false)
}

func (s *Store) executeIncidentSelfHeal(ctx context.Context, subject, incidentID, policyID, objectRef string, checker EffectChecker, observeOnly bool) (SelfHealExecution, error) {
	subject, objectRef, policyID = strings.TrimSpace(subject), strings.TrimSpace(objectRef), strings.TrimSpace(policyID)
	if subject == "" {
		return SelfHealExecution{}, ErrAuthorizationDenied
	}
	if policyID == "" || objectRef == "" {
		return SelfHealExecution{}, errors.New("policy_id 和 object_ref 不能为空")
	}

	// Binding discovery is local only. Pin the version and compare the policy
	// in the authoritative transaction before provisioning any write attempt.
	policySnapshot, err := s.GetSelfHealPolicy(ctx, policyID)
	if err != nil {
		return SelfHealExecution{}, err
	}
	var bound explicitMCPBinding
	var found bool
	var bindErr error
	if !observeOnly && !policySnapshot.ObserveOnly && checker != nil {
		bound, found, bindErr = s.resolveExplicitMCPBinding(ctx, policySnapshot.ActionCapability)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SelfHealExecution{}, err
	}
	defer tx.Rollback()
	p, err := selfHealPolicyInTx(ctx, tx, policyID)
	if err != nil {
		return SelfHealExecution{}, err
	}
	if observeOnly && !p.ObserveOnly {
		return SelfHealExecution{}, fmt.Errorf("%w：此入口只允许观察和提出建议，不执行自愈", ErrAuthorizationDenied)
	}
	if p != policySnapshot {
		return SelfHealExecution{}, fmt.Errorf("%w：自愈策略已修改，请重新核对", ErrConflict)
	}
	if p.Subject != subject {
		return SelfHealExecution{}, ErrAuthorizationDenied
	}
	stamp := now()
	ex := SelfHealExecution{ExecutionID: id(), PolicyID: p.PolicyID, ObjectRef: objectRef, CreatedAt: stamp, UpdatedAt: stamp}
	saveBeforeDispatch := func(state, reason, outcome string) (SelfHealExecution, error) {
		ex.State, ex.BlockReason, ex.VerificationOutcome = state, reason, outcome
		if _, err := tx.ExecContext(ctx, `INSERT INTO self_heal_executions(execution_id,policy_id,object_ref,operation_id,state,block_reason,verification_outcome,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`, ex.ExecutionID, p.PolicyID, objectRef, nil, state, reason, outcome, stamp, stamp); err != nil {
			return SelfHealExecution{}, err
		}
		return ex, tx.Commit()
	}
	block := func(reason string) (SelfHealExecution, error) {
		return saveBeforeDispatch("blocked", reason, "")
	}
	if reason := selfHealPolicyState(p, stamp); reason != "" {
		return block(reason)
	}
	if incidentID != "" {
		var state string
		var alreadyVerified bool
		if err := tx.QueryRowContext(ctx, `SELECT state,recovery_verified FROM incidents WHERE incident_id=?`, incidentID).Scan(&state, &alreadyVerified); err != nil {
			return SelfHealExecution{}, err
		}
		if state == "closed" || state == "resolved" || alreadyVerified {
			return block("事件已关闭或已验收，不能再次发起自愈")
		}
	}
	if p.ObserveOnly {
		return saveBeforeDispatch("proposed", "", "observe_only")
	}

	asset, reason, err := selfHealAssetAdmission(ctx, tx, p, incidentID, objectRef)
	if err != nil {
		return SelfHealExecution{}, err
	}
	if reason != "" {
		return block(reason)
	}
	if checker == nil {
		return block("没有独立健康复检器，不派发自愈写操作")
	}
	if bindErr != nil || !found {
		return block("自愈动作没有就绪、明确的 MCP 能力绑定")
	}
	idempotencyKey := "self_heal:" + ex.ExecutionID
	args, err := selfHealActionArguments(bound, objectRef, idempotencyKey)
	if err != nil {
		return block(err.Error())
	}

	// A pending/unknown operation blocks another write to this asset, even
	// across policies, versions, and rate-limit windows.
	var unresolved int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM self_heal_executions WHERE object_ref=? AND state IN ('admitted','dispatched')`, objectRef).Scan(&unresolved); err != nil {
		return SelfHealExecution{}, err
	}
	if unresolved != 0 {
		return block("目标节点仍有未核对的自愈操作，不会重发")
	}
	if held, err := targetHasLocalExclusiveHold(tx, objectRef); err != nil {
		return SelfHealExecution{}, err
	} else if held {
		return block("目标正在被其他任务占用，不能自愈")
	}
	windowStart := stamp - p.PerNodeWindowMS
	var nodeWrites int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM self_heal_executions e JOIN self_heal_policies p ON p.policy_id=e.policy_id
WHERE p.policy_key=? AND e.object_ref=? AND e.created_at>=? AND e.state IN ('admitted','dispatched','verified','failed')`,
		p.PolicyKey, objectRef, windowStart).Scan(&nodeWrites); err != nil {
		return SelfHealExecution{}, err
	}
	if nodeWrites >= p.PerNodeMaxWrites {
		return block("超出单节点滚动窗口写操作上限")
	}
	var clusterActive int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM self_heal_executions e JOIN self_heal_policies p ON p.policy_id=e.policy_id
WHERE p.policy_key=? AND e.state IN ('admitted','dispatched')`, p.PolicyKey).Scan(&clusterActive); err != nil {
		return SelfHealExecution{}, err
	}
	if clusterActive >= p.ClusterMaxConcurrent {
		return block("超出集群自愈并发上限")
	}
	if ok, reason, err := selfHealAuthorization(tx, subject, p.ActionCapability, objectRef, stamp); err != nil {
		return SelfHealExecution{}, err
	} else if !ok {
		return block(reason)
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
	hold, err := holdTarget(tx, objectRef, "self_heal", taskID, stamp)
	if err != nil {
		return SelfHealExecution{}, err
	}
	op, err := registerOperation(tx, Operation{
		TaskID:            taskID,
		CapabilityKey:     p.ActionCapability,
		CapabilityVersion: bound.Binding.Version,
		ConnectionID:      bound.Binding.ConnectionID,
		TargetRefs:        toJSON([]ticketTarget{{Ref: objectRef, Revision: asset.Revision, Epoch: asset.OwnershipEpoch}}),
		ParamsDigest:      digest(toJSON(args)),
		IdempotencyKey:    idempotencyKey,
		AuthorizationRef:  subject,
	}, stamp)
	if err != nil {
		return SelfHealExecution{}, err
	}
	disp, err := dispatchOperationCommitted(tx, op.OperationID, subject, stamp)
	if err != nil {
		return SelfHealExecution{}, err
	}
	if !disp.Dispatched {
		return SelfHealExecution{}, fmt.Errorf("%w：%s", ErrAuthorizationDenied, disp.Reason)
	}
	if _, err := transitionTaskState(tx, taskID, EventClaim, TaskGuard{Actor: subject, ExpectedStorageRevision: 1}, stamp); err != nil {
		return SelfHealExecution{}, err
	}
	ex.OperationID, ex.State = op.OperationID, "dispatched"
	if _, err := tx.ExecContext(ctx, `INSERT INTO self_heal_executions(execution_id,policy_id,object_ref,operation_id,state,block_reason,verification_outcome,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`, ex.ExecutionID, p.PolicyID, objectRef, op.OperationID, "dispatched", "", "", stamp, stamp); err != nil {
		return SelfHealExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return SelfHealExecution{}, err
	}

	settlement := selfHealSettlement{operation: RuntimeOperationReconcile, outcome: "reconcile_required：动作结果未确认"}
	if err := s.recheckSelfHealHandoff(ctx, p, bound, asset, incidentID, taskID, hold.ReservationID); err != nil {
		settlement = selfHealSettlement{operation: RuntimeOperationCancelled, outcome: "派发前已阻止，未调用提供者：" + err.Error()}
	} else {
		res, callErr := s.CallConnectionTool(ctx, bound.Binding.ConnectionID, bound.Tool.Name, bound.Binding.ConnectionRevision, args)
		settlement.receipt = selfHealActionReceipt(res, objectRef)
		switch {
		case callErr != nil && res.ID == "":
			// The MCP gateway records its call ID before the first external
			// write. A preflight denial/failed handshake cannot have executed.
			settlement.operation = RuntimeOperationCancelled
			settlement.outcome = "外部派发前被拒绝，未调用自愈工具"
		case callErr != nil:
			settlement.outcome = "reconcile_required：外部调用未返回可信回执，操作可能已执行"
		case !settlement.receipt.Completed:
			settlement.outcome = "reconcile_required：未收到目标的同步成功终态，不能确认修复"
		case ctx.Err() != nil:
			settlement.outcome = "reconcile_required：动作已完成，独立复检尚未确认"
		default:
			verified, evidence, checkErr := checker(ctx, objectRef)
			evidence = strings.TrimSpace(evidence)
			if checkErr != nil || ctx.Err() != nil || evidence == "" || len(evidence) > 8000 {
				settlement.outcome = "reconcile_required：独立健康复检无可用证据"
			} else {
				settlement.outcome, settlement.verifiedEvidence = evidence, evidence
				settlement.operation = RuntimeOperationFailed
				if verified {
					settlement.operation = RuntimeOperationSucceeded
				}
			}
		}
	}
	return s.settleSelfHeal(ctx, ex, disp.Attempt.AttemptID, taskID, incidentID, hold.ReservationID, asset, settlement)
}

func selfHealPolicyInTx(ctx context.Context, tx *sql.Tx, policyID string) (SelfHealPolicy, error) {
	return scanSelfHealPolicy(tx.QueryRowContext(ctx, `SELECT `+selfHealPolicyColumns+` FROM self_heal_policies WHERE policy_id=?`, policyID))
}

func selfHealPolicyState(p SelfHealPolicy, stamp int64) string {
	if p.State != "active" {
		return fmt.Sprintf("策略状态为 %s，未激活", p.State)
	}
	if stamp < p.ValidFrom || (p.ValidUntil > 0 && stamp >= p.ValidUntil) {
		return "当前时间不在策略有效期内"
	}
	return ""
}

func selfHealAssetAdmission(ctx context.Context, tx *sql.Tx, p SelfHealPolicy, incidentID, objectRef string) (OpsAsset, string, error) {
	if incidentID == "" || !healthNodeName.MatchString(objectRef) {
		return OpsAsset{}, "自愈写操作需要关联事件及唯一的具体节点", nil
	}
	// Do not guess at textual preconditions or unsupported verification
	// standards. They are not executable guards and cannot authorize a write.
	var preconditions []string
	if json.Unmarshal([]byte(p.Preconditions), &preconditions) != nil || len(preconditions) != 0 ||
		(p.VerificationStandard != "" && p.VerificationStandard != "independent_health") {
		return OpsAsset{}, "策略包含尚不能核实的前置条件或验收标准", nil
	}
	targets := selfHealScopeNodes(p.AssetScope)
	if len(targets) == 0 {
		return OpsAsset{}, "自愈策略未明确配置资产范围", nil
	}
	allowed := false
	for _, node := range targets {
		allowed = allowed || node == objectRef
	}
	if !allowed {
		return OpsAsset{}, "目标节点不在自愈策略范围内", nil
	}
	var impact, incidentState string
	var recovered bool
	if err := tx.QueryRowContext(ctx, `SELECT impact_scope,state,recovery_verified FROM incidents WHERE incident_id=?`, incidentID).Scan(&impact, &incidentState, &recovered); err != nil {
		return OpsAsset{}, "", err
	}
	if recovered || incidentState == "resolved" || incidentState == "closed" {
		return OpsAsset{}, "事件已关闭或已验收", nil
	}
	nodes := selfHealScopeNodes(impact)
	if len(nodes) != 1 || nodes[0] != objectRef {
		return OpsAsset{}, "事件影响不明确或涉及多个节点，须人工处置", nil
	}
	asset, err := readAsset(tx, objectRef)
	if errors.Is(err, sql.ErrNoRows) {
		return OpsAsset{}, "目标没有已登记的稳定资产身份", nil
	}
	if err != nil {
		return OpsAsset{}, "", err
	}
	if asset.Identity == "" || asset.Kind != "node" || asset.State != AssetStateInService || asset.FrozenAt != 0 {
		return OpsAsset{}, "目标不是可自愈的在役节点，或已冻结", nil
	}
	var loanActive, conflicting bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM loans WHERE object_ref=? AND state='active')`, objectRef).Scan(&loanActive); err != nil {
		return OpsAsset{}, "", err
	}
	if loanActive {
		return OpsAsset{}, "目标资产处于借出状态，禁止自愈写操作", nil
	}
	// Use the real identity tables; there is no separate 'identity_conflicts'
	// table. A conflicting current asset, discovery candidate, or an identity
	// claim pointing to a different canonical object blocks the write.
	if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM ops_assets a WHERE a.identity=? COLLATE NOCASE AND a.object_ref<>?)
 OR EXISTS(SELECT 1 FROM discovery_candidates c WHERE c.state='conflict' AND
           (c.admitted_ref=? OR (c.serial<>'' AND c.serial=? COLLATE NOCASE)))
 OR EXISTS(SELECT 1 FROM identity_claims a JOIN identity_claims b
           ON a.identity_kind=b.identity_kind AND a.candidate_identity=b.candidate_identity COLLATE NOCASE
           WHERE a.confidence IN ('trusted','candidate') AND b.confidence IN ('trusted','candidate')
             AND COALESCE(NULLIF(a.unified_to,''), a.candidate_object_ref)=?
             AND COALESCE(NULLIF(b.unified_to,''), b.candidate_object_ref)<>?)`,
		asset.Identity, objectRef, objectRef, asset.Identity, objectRef, objectRef).Scan(&conflicting); err != nil {
		return OpsAsset{}, "", err
	}
	if conflicting {
		return OpsAsset{}, "目标资产存在未解决的身份冲突，禁止自愈写操作", nil
	}
	return asset, "", nil
}

func selfHealScopeNodes(raw string) []string {
	// Additional unsupported selectors are not safely representable as a
	// one-node incident, even if the nodes field alone appears to match.
	var scope map[string]json.RawMessage
	var nodes []string
	if json.Unmarshal([]byte(raw), &scope) != nil || len(scope) != 1 || json.Unmarshal(scope["nodes"], &nodes) != nil {
		return nil
	}
	seen := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if seen[node] || !healthNodeName.MatchString(node) {
			return nil
		}
		seen[node] = true
	}
	return nodes
}

func selfHealAuthorization(tx *sql.Tx, subject, capability, target string, stamp int64) (bool, string, error) {
	for _, check := range []struct{ action, object, scope string }{
		{capability, target, "self_heal"}, {"dispatch", capability, ""},
	} {
		decision, err := decideAuthorization(tx, subject, check.action, check.object, check.scope, stamp, false)
		if err != nil {
			return false, "", err
		}
		if decision.Outcome != "allowed" {
			return false, decision.Reason, nil
		}
	}
	return true, "", nil
}

func selfHealActionArguments(bound explicitMCPBinding, target, idem string) (map[string]any, error) {
	t := bound.Tool
	if bound.Binding.Kind != "action" || t.ReadOnly || (t.ServerReadOnly != nil && *t.ServerReadOnly) {
		return nil, errors.New("自愈动作必须是明确授权的写能力，不接受只读工具")
	}
	switch strings.ToLower(t.Name) {
	case "fault", "fault_check", "offline", "online", "ac", "update",
		"run", "tree_run", "star_run":
		return nil, errors.New("Slurm 状态切换或通用远程命令不能作为自动自愈能力")
	}
	props := toolSchemaProperties(t)
	args := map[string]any{"idempotency_key": idem}
	if _, ok := props["idempotency_key"]; !ok {
		return nil, errors.New("自愈动作未声明幂等请求标识")
	}
	for _, field := range []string{"nodes", "object_ref", "target"} {
		if _, ok := props[field]; ok {
			args[field] = target
			break
		}
	}
	if len(args) != 2 {
		return nil, errors.New("自愈动作未声明具体节点参数")
	}
	if _, ok := props["sync"]; ok {
		args["sync"] = true
	}
	if err := mcpclient.ValidateArguments(t, args); err != nil {
		return nil, errors.New("自愈动作当前参数合同无法安全满足")
	}
	return args, nil
}

func (s *Store) recheckSelfHealHandoff(ctx context.Context, p SelfHealPolicy, snapshot explicitMCPBinding, expected OpsAsset, incidentID, taskID, holdID string) error {
	current, found, err := s.resolveExplicitMCPBinding(ctx, p.ActionCapability)
	if err != nil {
		return err
	}
	if !found || current.Binding != snapshot.Binding || current.Tool.Version != snapshot.Tool.Version {
		return fmt.Errorf("%w：自愈动作能力绑定已变化", ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	policy, err := selfHealPolicyInTx(ctx, tx, p.PolicyID)
	if err != nil {
		return err
	}
	stamp := now()
	if policy != p || selfHealPolicyState(policy, stamp) != "" {
		return fmt.Errorf("%w：自愈策略已变化或失效", ErrConflict)
	}
	asset, reason, err := selfHealAssetAdmission(ctx, tx, p, incidentID, expected.ObjectRef)
	if err != nil {
		return err
	}
	if reason != "" || !sameSelfHealAsset(asset, expected) {
		return fmt.Errorf("%w：自愈目标、事件或资产权限已变化", ErrConflict)
	}
	state, err := readTaskState(tx, taskID)
	if err != nil {
		return err
	}
	if state.State != RuntimeTaskRunning {
		return fmt.Errorf("%w：自愈任务已停止接收新派发", ErrState)
	}
	var held bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM quota_reservations
WHERE reservation_id=? AND task_id=? AND target_ref=? AND scope='target_hold' AND exclusive=1 AND state IN ('reserved','settled'))`, holdID, taskID, expected.ObjectRef).Scan(&held); err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("%w：自愈目标排他占用已变化", ErrConflict)
	}
	ok, _, err := selfHealAuthorization(tx, p.Subject, p.ActionCapability, expected.ObjectRef, stamp)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAuthorizationDenied
	}
	return tx.Commit()
}

func sameSelfHealAsset(a, expected OpsAsset) bool {
	return a.ObjectRef == expected.ObjectRef && a.Identity == expected.Identity && a.Revision == expected.Revision &&
		a.OwnershipEpoch == expected.OwnershipEpoch && a.State == expected.State && a.FrozenAt == expected.FrozenAt
}

type selfHealReceipt struct {
	CallID    string `json:"connection_call_id,omitempty"`
	TaskID    string `json:"provider_task_id,omitempty"`
	State     string `json:"state,omitempty"`
	Completed bool   `json:"completed"`
}

func selfHealActionReceipt(res ConnectionResult, target string) selfHealReceipt {
	r := selfHealReceipt{CallID: res.ID}
	var p struct {
		TaskID       string `json:"task_id"`
		Mode         string `json:"mode"`
		Terminal     *bool  `json:"terminal"`
		State        string `json:"state"`
		ManifestErr  string `json:"manifest_error"`
		SummaryLost  bool   `json:"summary_omitted"`
		Deduplicated bool   `json:"deduplicated"`
		Partial      bool   `json:"partial_results"`
		Total        *int   `json:"total"`
		Succeeded    *int   `json:"succeeded"`
		Failed       int    `json:"failed"`
		Canceled     int    `json:"canceled"`
		Skipped      int    `json:"skipped"`
		Rows         []struct {
			Node  string `json:"node"`
			Code  *int   `json:"code"`
			State string `json:"state"`
			Data  struct {
				CLISummary bool   `json:"cli_summary"`
				TaskID     string `json:"cli_task_id"`
				Error      string `json:"error"`
				Truncated  bool   `json:"truncated"`
			} `json:"data"`
		} `json:"command_results"`
	}
	if json.Unmarshal(res.Structured, &p) != nil || !mcpProviderTaskID.MatchString(p.TaskID) {
		return r
	}
	r.TaskID = p.TaskID
	switch p.State {
	case "COMPLETE", "FAILED", "CANCELED", "PENDING", "RUNNING", "READY":
		r.State = p.State
	}
	if res.SourceUnavailable || res.Truncated || res.IsError || res.ValidationError != "" ||
		p.Mode != "sync" || p.Terminal == nil || !*p.Terminal || p.State != "COMPLETE" ||
		p.Deduplicated || p.ManifestErr != "" || p.SummaryLost || p.Partial || p.Failed != 0 || p.Canceled != 0 || p.Skipped != 0 ||
		p.Total == nil || *p.Total != 1 || p.Succeeded == nil || *p.Succeeded != 1 {
		return r
	}
	hits := 0
	for _, row := range p.Rows {
		if row.Data.CLISummary && row.Node == "" {
			continue
		}
		if row.Node != target || row.Data.CLISummary || row.Data.TaskID != p.TaskID || row.Code == nil ||
			*row.Code != 0 || row.State != "succeeded" || row.Data.Error != "" || row.Data.Truncated {
			return r
		}
		hits++
	}
	r.Completed = hits == 1
	return r
}

type selfHealSettlement struct {
	operation        RuntimeOperationState
	outcome          string
	verifiedEvidence string
	receipt          selfHealReceipt
}

func (s *Store) settleSelfHeal(ctx context.Context, ex SelfHealExecution, attemptID, taskID, incidentID, holdID string, expected OpsAsset, result selfHealSettlement) (SelfHealExecution, error) {
	// Request cancellation does not cancel local bookkeeping of an already
	// issued write. Persist the unknown outcome without retrying the effect.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SelfHealExecution{}, err
	}
	defer tx.Rollback()
	if result.verifiedEvidence != "" {
		current, err := readAsset(tx, ex.ObjectRef)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return SelfHealExecution{}, err
		}
		var scope, state string
		if err := tx.QueryRowContext(ctx, `SELECT impact_scope,state FROM incidents WHERE incident_id=?`, incidentID).Scan(&scope, &state); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return SelfHealExecution{}, err
		}
		nodes := selfHealScopeNodes(scope)
		if !sameSelfHealAsset(current, expected) || state == "closed" || state == "resolved" ||
			len(nodes) != 1 || nodes[0] != ex.ObjectRef {
			result.operation, result.verifiedEvidence = RuntimeOperationReconcile, ""
			result.outcome = "reconcile_required：复检期间资产身份、代际或事件范围已变化"
		}
	}
	stamp := now()
	if _, err := transitionOperation(tx, ex.OperationID, result.operation, stamp); err != nil {
		return SelfHealExecution{}, err
	}
	ex.State, ex.VerificationOutcome, ex.UpdatedAt = "dispatched", result.outcome, stamp
	attemptOutcome, targetState, reason, evidenceRef, completed := "unknown", "unknown", result.outcome, "", int64(0)
	switch result.operation {
	case RuntimeOperationSucceeded:
		ex.State, attemptOutcome, targetState, reason, completed = "verified", "success", "succeeded", "", stamp
	case RuntimeOperationFailed:
		ex.State, attemptOutcome, targetState, completed = "failed", "failure", "failed", stamp
	case RuntimeOperationCancelled:
		ex.State, ex.BlockReason, attemptOutcome, targetState, completed = "blocked", result.outcome, "failure", "failed", stamp
	}
	if result.verifiedEvidence != "" {
		evidenceRef = digest(result.verifiedEvidence)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operation_attempts SET outcome=?,receipt=?,evidence_refs=?,completed_at=? WHERE attempt_id=? AND operation_id=?`,
		attemptOutcome, toJSON(result.receipt), toJSON(nonNilStrings(selfHealEvidenceRefs(evidenceRef))), completed, attemptID, ex.OperationID); err != nil {
		return SelfHealExecution{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET completed_at=?,failure_reason=?,effect_refs=? WHERE operation_id=?`,
		completed, reason, toJSON(nonNilStrings(selfHealEvidenceRefs(evidenceRef))), ex.OperationID); err != nil {
		return SelfHealExecution{}, err
	}
	if err := recordTargetResult(tx, OperationTargetResult{OperationID: ex.OperationID, TargetRef: ex.ObjectRef, State: targetState, EvidenceRef: evidenceRef, Detail: result.outcome}, stamp); err != nil {
		return SelfHealExecution{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE self_heal_executions SET state=?,block_reason=?,verification_outcome=?,updated_at=? WHERE execution_id=?`,
		ex.State, ex.BlockReason, ex.VerificationOutcome, stamp, ex.ExecutionID); err != nil {
		return SelfHealExecution{}, err
	}
	if result.operation != RuntimeOperationReconcile {
		released, err := tx.ExecContext(ctx, `UPDATE quota_reservations SET state='released',effect_verified=1
WHERE reservation_id=? AND task_id=? AND target_ref=? AND scope='target_hold' AND state IN ('reserved','settled')`, holdID, taskID, ex.ObjectRef)
		if err != nil {
			return SelfHealExecution{}, err
		}
		if count, err := released.RowsAffected(); err != nil || count != 1 {
			return SelfHealExecution{}, fmt.Errorf("%w：自愈目标占用已变化", ErrConflict)
		}
	}
	if err := settleSelfHealTask(tx, taskID, result.operation, stamp); err != nil {
		return SelfHealExecution{}, err
	}
	// An independently verified node repair is not approval of the whole
	// incident. Incident acceptance remains a separate user-visible decision.
	return ex, tx.Commit()
}

func selfHealEvidenceRefs(ref string) []string {
	if ref == "" {
		return nil
	}
	return []string{ref}
}

func settleSelfHealTask(tx *sql.Tx, taskID string, operation RuntimeOperationState, stamp int64) error {
	task, err := readTaskState(tx, taskID)
	if err != nil {
		return err
	}
	if operation == RuntimeOperationReconcile {
		task.ReconcileRequired = true
		task.StorageRevision++
		task.UpdatedAt = stamp
		return upsertTaskState(tx, task)
	}
	// User pause/cancel may have won after dispatch. Record the provider
	// evidence but never move the user's task past their control decision.
	if task.State != RuntimeTaskRunning {
		return nil
	}
	guard := TaskGuard{Actor: "self_heal", ExpectedStorageRevision: task.StorageRevision}
	if operation == RuntimeOperationSucceeded {
		if _, err := transitionTaskState(tx, taskID, EventStepsComplete, guard, stamp); err != nil {
			return err
		}
		guard.ExpectedStorageRevision++
		guard.AcceptanceEvidence = true
		if _, err := transitionTaskState(tx, taskID, EventAcceptancePass, guard, stamp); err != nil {
			return err
		}
	} else {
		guard.FinalState = RuntimeTaskFailed
		event := EventFail
		if operation == RuntimeOperationCancelled {
			guard.FinalState, event = RuntimeTaskCancelled, EventCancel
		}
		if _, err := transitionTaskState(tx, taskID, event, guard, stamp); err != nil {
			return err
		}
	}
	guard.ExpectedStorageRevision++
	_, err = transitionTaskState(tx, taskID, EventFinalized, guard, stamp)
	return err
}
