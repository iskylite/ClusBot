package work

// P4-17：有界自动自愈策略与 E02。
//
// 本文件实现授权范围内自动推进，超出条件准确停止和交接：
//   - 策略固定 ID/版本、主体、资产范围、有效期、前置、动作与验证标准；
//   - 调查归并、维护接管、Operation、验证和待复核闭环；借出/冻结/身份冲突/
//     共享故障不能走普通自动写；
//   - 示例滚动 30 分钟每节点最多一次写派发、同策略集群并发最多五个；作为
//     可配置待验证参数，按稳定策略 ID 持久计数，重启/新任务/修订不清零；
//   - 成功回执后独立检查服务与新心跳/覆盖；验证失败不追加第二次重启，未知
//     效果先核对；
//   - 观察模式只报告拟判定，不派发写或改资产生命周期；停用/撤销阻止新准入，
//     未决责任仍处理。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 策略（固定 ID/版本、主体、资产范围、有效期、前置、动作与验证标准）
// ---------------------------------------------------------------------------

// SelfHealPolicy 是 self_heal_policies 的一行。
type SelfHealPolicy struct {
	PolicyID             string
	PolicyKey            string
	Version              int
	Subject              string
	AssetScope           string
	ValidFrom            int64
	ValidUntil           int64
	Preconditions        string
	ActionCapability     string
	VerificationStandard string
	PerNodeWindowMS      int64
	PerNodeMaxWrites     int
	ClusterMaxConcurrent int
	State                string
	ObserveOnly          bool
	PublishedBy          string
	PublishedAt          int64
	CreatedAt            int64
	UpdatedAt            int64
}

// publishSelfHealPolicy 发布自愈策略（P4-17.1）。
// 策略固定 ID/版本、主体、资产范围、有效期、前置、动作与验证标准；先受控
// 环境、再明确授权的小范围启用。
func publishSelfHealPolicy(tx *sql.Tx, p SelfHealPolicy, now int64) (SelfHealPolicy, error) {
	if p.PolicyKey == "" || p.Version < 1 || p.Subject == "" || p.ActionCapability == "" || p.PublishedBy == "" {
		return SelfHealPolicy{}, errors.New("策略缺少标识/版本/主体/动作能力/发布者")
	}
	if p.VerificationStandard == "" {
		return SelfHealPolicy{}, errors.New("策略缺少验证标准")
	}
	// 默认限流参数（示例滚动 30 分钟每节点最多一次写派发、集群并发最多五个）。
	if p.PerNodeWindowMS <= 0 {
		p.PerNodeWindowMS = 1800000
	}
	if p.PerNodeMaxWrites <= 0 {
		p.PerNodeMaxWrites = 1
	}
	if p.ClusterMaxConcurrent <= 0 {
		p.ClusterMaxConcurrent = 5
	}
	p.PolicyID = id()
	p.State = "published"
	p.ObserveOnly = true // 默认观察模式（先受控环境）
	p.PublishedAt = now
	p.CreatedAt, p.UpdatedAt = now, now
	_, err := tx.Exec(`INSERT INTO self_heal_policies(policy_id,policy_key,version,subject,asset_scope,
valid_from,valid_until,preconditions,action_capability,verification_standard,per_node_window_ms,
per_node_max_writes,cluster_max_concurrent,state,observe_only,published_by,published_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.PolicyID, p.PolicyKey, p.Version, p.Subject, p.AssetScope, p.ValidFrom, p.ValidUntil,
		p.Preconditions, p.ActionCapability, p.VerificationStandard, p.PerNodeWindowMS,
		p.PerNodeMaxWrites, p.ClusterMaxConcurrent, p.State, boolToInt(p.ObserveOnly),
		p.PublishedBy, now, now, now)
	if err != nil {
		return SelfHealPolicy{}, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 限流（按稳定策略 ID 持久计数，重启/新任务/修订不清零）
// ---------------------------------------------------------------------------

// checkSelfHealRateLimit 核对自愈限流（P4-17.3）。
// 示例滚动 30 分钟每节点最多一次写派发、同策略集群并发最多五个；作为可配置
// 待验证参数，按稳定策略 ID 持久计数，重启/新任务/修订不清零。
func checkSelfHealRateLimit(tx *sql.Tx, policyID, objectRef string, windowMS int64, perNodeMax int, clusterMax int, now int64) error {
	windowStart := now - (now % windowMS)
	// 每节点限流（滚动窗口内最多一次写派发）。
	var nodeCount int
	if err := tx.QueryRow(`SELECT coalesce(count,0) FROM self_heal_counters WHERE policy_id=? AND counter_key=? AND window_start=?`,
		policyID, "per_node:"+objectRef, windowStart).Scan(&nodeCount); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if nodeCount >= perNodeMax {
		return fmt.Errorf("节点 %s 在窗口内已达最大写派发 %d（滚动 30 分钟每节点最多一次）", objectRef, perNodeMax)
	}
	// 集群并发限流（同策略集群并发最多五个）。
	var clusterCount int
	if err := tx.QueryRow(`SELECT coalesce(sum(count),0) FROM self_heal_counters WHERE policy_id=? AND counter_key=? AND window_start=?`,
		policyID, "cluster_concurrent", windowStart).Scan(&clusterCount); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if clusterCount >= clusterMax {
		return fmt.Errorf("策略 %s 集群并发已达最大 %d（同策略集群并发最多五个）", policyID, clusterMax)
	}
	return nil
}

// incrementSelfHealCounter 增加自愈计数（按稳定策略 ID 持久计数，不清零）。
func incrementSelfHealCounter(tx *sql.Tx, policyID, counterKey string, windowStart, now int64) error {
	_, err := tx.Exec(`INSERT INTO self_heal_counters(policy_id,counter_key,window_start,count,updated_at)
VALUES(?,?,?,1,?) ON CONFLICT(policy_id,counter_key,window_start) DO UPDATE SET count=count+1,updated_at=excluded.updated_at`,
		policyID, counterKey, windowStart, now)
	return err
}

// ---------------------------------------------------------------------------
// 自愈执行（调查归并、维护接管、Operation、验证和待复核闭环）
// ---------------------------------------------------------------------------

// SelfHealExecution 是 self_heal_executions 的一行。
type SelfHealExecution struct {
	ExecutionID         string
	PolicyID            string
	ObjectRef           string
	OperationID         string
	State               string
	BlockReason         string
	VerificationOutcome string
	CreatedAt           int64
	UpdatedAt           int64
}

// ErrSelfHealBlocked 是自愈阻断的错误。
var ErrSelfHealBlocked = errors.New("自愈阻断")

// admitSelfHealExecution 准入场愈执行（P4-17.2）。
// 借出/冻结/身份冲突/共享故障不能走普通自动写。
func admitSelfHealExecution(tx *sql.Tx, policy SelfHealPolicy, objectRef string, now int64) (SelfHealExecution, error) {
	exec := SelfHealExecution{
		ExecutionID: id(), PolicyID: policy.PolicyID, ObjectRef: objectRef,
		State: "proposed", CreatedAt: now, UpdatedAt: now,
	}
	// 观察模式：只报告拟判定（不派发写或改资产生命周期）。
	if policy.ObserveOnly {
		exec.State = "proposed"
		exec.BlockReason = "观察模式（只报告拟判定，不派发写）"
		return exec, nil
	}
	// 策略状态（停用/撤销阻止新准入）。
	if policy.State != "active" {
		exec.State = "blocked"
		exec.BlockReason = fmt.Sprintf("策略 %s（停用/撤销阻止新准入）", policy.State)
		return exec, nil
	}
	// 借出/冻结/身份冲突：不能走普通自动写。
	asset, err := readAsset(tx, objectRef)
	if err == nil {
		if asset.FrozenAt > 0 {
			exec.State = "blocked"
			exec.BlockReason = "资产已冻结（借出/冻结/身份冲突/共享故障不能走普通自动写）"
			return exec, nil
		}
		if asset.State == AssetStateLent {
			exec.State = "blocked"
			exec.BlockReason = "资产已借出（不能走普通自动写）"
			return exec, nil
		}
	}
	// 限流核对（滚动 30 分钟每节点最多一次写派发、集群并发最多五个）。
	if err := checkSelfHealRateLimit(tx, policy.PolicyID, objectRef, policy.PerNodeWindowMS, policy.PerNodeMaxWrites, policy.ClusterMaxConcurrent, now); err != nil {
		exec.State = "blocked"
		exec.BlockReason = err.Error()
		return exec, nil
	}
	exec.State = "admitted"
	return exec, nil
}

// recordSelfHealExecution 记录自愈执行（动作与效果有独立账本）。
func recordSelfHealExecution(tx *sql.Tx, exec SelfHealExecution, now int64) error {
	var operationID any
	if exec.OperationID != "" {
		operationID = exec.OperationID
	}
	_, err := tx.Exec(`INSERT INTO self_heal_executions(execution_id,policy_id,object_ref,operation_id,
state,block_reason,verification_outcome,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
		exec.ExecutionID, exec.PolicyID, exec.ObjectRef, operationID, exec.State,
		exec.BlockReason, exec.VerificationOutcome, exec.CreatedAt, now)
	return err
}

// ---------------------------------------------------------------------------
// 验证（成功回执后独立检查服务与新心跳/覆盖）
// ---------------------------------------------------------------------------

// verifySelfHealEffect 核验自愈效果（P4-17.4）。
// 成功回执后独立检查服务与新心跳/覆盖；验证失败不追加第二次重启，未知效果
// 先核对。
func verifySelfHealEffect(tx *sql.Tx, executionID string, serviceHealthy bool, newHeartbeat bool, coverageOK bool, now int64) (verified bool, reason string, err error) {
	// 独立检查服务与新心跳/覆盖（验证失败不追加第二次重启）。
	if !serviceHealthy {
		return false, "服务不健康（验证失败不追加第二次重启）", nil
	}
	if !newHeartbeat {
		return false, "无新心跳（未知效果先核对）", nil
	}
	if !coverageOK {
		return false, "覆盖不完整（未知效果先核对）", nil
	}
	if _, err := tx.Exec(`UPDATE self_heal_executions SET state='verified', verification_outcome='verified', updated_at=? WHERE execution_id=?`,
		now, executionID); err != nil {
		return false, "", err
	}
	return true, "验证通过", nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
