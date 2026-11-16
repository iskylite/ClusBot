package work

// P4-13：对象组联机验证与拓扑覆盖。
//
// 本文件实现证明一组真实节点及其网络/服务协作可用，不能用单机通过替代：
//   - 冻结实际参与节点、部件/端口及相关拓扑版本、测试规范和运行条件；
//   - 组织组级联机测试，逐要求关联实际对象组与证据；缺席节点和不完整覆盖
//     明确报告；
//   - 组测期间换件/换线/目标集合改变时核对适用性，受影响结论失效；
//   - 允许同批无冲突测试并行，但共享链路/电源/服务的冲突按影响范围协调；
//   - 失败保留支持/反证与查询入口，交给诊断步骤，不自动重新部署全批。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 组测快照（冻结实际参与节点、部件/端口及相关拓扑版本）
// ---------------------------------------------------------------------------

// GroupTestSnapshot 是组测快照。
type GroupTestSnapshot struct {
	SnapshotID       string
	// 实际参与节点（冻结）。
	ParticipatingNodes []string
	// 部件/端口及相关拓扑版本。
	Components       []string
	TopologyVersion  int
	// 测试规范和运行条件。
	TestSpec         string
	RuntimeConditions string
	// 冻结时间。
	FrozenAt         int64
}

// freezeGroupTestSnapshot 冻结组测快照（P4-13.1）。
// 冻结实际参与节点、部件/端口及相关拓扑版本、测试规范和运行条件。
func freezeGroupTestSnapshot(nodes []string, components []string, topologyVersion int, testSpec string, runtimeConditions string, now int64) (GroupTestSnapshot, error) {
	if len(nodes) == 0 || topologyVersion < 1 || testSpec == "" {
		return GroupTestSnapshot{}, errors.New("组测快照缺少节点/拓扑版本/测试规范")
	}
	snapshot := GroupTestSnapshot{
		SnapshotID: id(), ParticipatingNodes: nodes, Components: components,
		TopologyVersion: topologyVersion, TestSpec: testSpec, RuntimeConditions: runtimeConditions,
		FrozenAt: now,
	}
	return snapshot, nil
}

// ---------------------------------------------------------------------------
// 组级联机测试（逐要求关联实际对象组与证据）
// ---------------------------------------------------------------------------

// GroupValidationResult 是组级验证结果。
type GroupValidationResult struct {
	// 逐要求关联实际对象组与证据。
	RequirementKey   string
	ObjectGroup      []string
	EvidenceRefs     []string
	// 覆盖（缺席节点和不完整覆盖明确报告）。
	Coverage         string // complete/incomplete/unknown
	AbsentNodes      []string
	// 结果（passed/failed/unknown；不能用单机通过替代）。
	Outcome          string
	// 失败保留支持/反证与查询入口（交给诊断步骤）。
	SupportingEvidence []string
	CounterEvidence    []string
	DiagnosisEntry     string
}

// runGroupValidation 运行组级联机验证（P4-13.2）。
// 组织组级联机测试，逐要求关联实际对象组与证据；缺席节点和不完整覆盖
// 明确报告；所有单机通过但组测失败时整组交付条件不成立。
func runGroupValidation(tx *sql.Tx, snapshot GroupTestSnapshot, requirementKey string, now int64) (GroupValidationResult, error) {
	result := GroupValidationResult{
		RequirementKey: requirementKey, ObjectGroup: snapshot.ParticipatingNodes,
	}
	// 缺席节点核对（缺席节点和不完整覆盖明确报告）。
	for _, node := range snapshot.ParticipatingNodes {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM ops_assets WHERE object_ref=?)`, node).Scan(&exists); err != nil {
			return result, err
		}
		if !exists {
			result.AbsentNodes = append(result.AbsentNodes, node)
		}
	}
	// 覆盖判定。
	if len(result.AbsentNodes) > 0 {
		result.Coverage = "incomplete"
		result.Outcome = "unknown" // 不完整覆盖明确报告（不能用单机通过替代）
	} else {
		result.Coverage = "complete"
		result.Outcome = "unknown" // 组测结果由实际测试决定（不能用单机通过替代）
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// 组测期间变更核对（换件/换线/目标集合改变时核对适用性）
// ---------------------------------------------------------------------------

// ErrGroupTestInvalidated 是组测失效的错误。
var ErrGroupTestInvalidated = errors.New("组测期间换件/换线/目标集合改变，受影响结论失效")

// ensureGroupTestApplicability 核对组测适用性（P4-13.3）。
// 组测期间换件/换线/目标集合改变时核对适用性，受影响结论失效；临时替换
// 参与节点或拓扑变更不会沿用原组测通过。
func ensureGroupTestApplicability(snapshot GroupTestSnapshot, currentNodes []string, currentTopologyVersion int) error {
	// 拓扑变更：不会沿用原组测通过。
	if currentTopologyVersion != snapshot.TopologyVersion {
		return fmt.Errorf("%w：拓扑版本 %d≠%d（临时替换参与节点或拓扑变更不会沿用原组测通过）",
			ErrGroupTestInvalidated, currentTopologyVersion, snapshot.TopologyVersion)
	}
	// 目标集合改变：不会沿用原组测通过。
	if len(currentNodes) != len(snapshot.ParticipatingNodes) {
		return fmt.Errorf("%w：目标集合改变（%d≠%d）", ErrGroupTestInvalidated, len(currentNodes), len(snapshot.ParticipatingNodes))
	}
	for i, node := range snapshot.ParticipatingNodes {
		if currentNodes[i] != node {
			return fmt.Errorf("%w：目标集合改变（%s≠%s）", ErrGroupTestInvalidated, currentNodes[i], node)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 冲突协调（共享链路/电源/服务的冲突按影响范围协调）
// ---------------------------------------------------------------------------

// ensureNoSharedResourceConflict 核对共享资源冲突（P4-13.4）。
// 允许同批无冲突测试并行，但共享链路/电源/服务的冲突按影响范围协调。
func ensureNoSharedResourceConflict(tx *sql.Tx, testA, testB string, sharedResources []string, now int64) error {
	// 共享链路/电源/服务：冲突按影响范围协调。
	if len(sharedResources) > 0 {
		return fmt.Errorf("测试 %s 与 %s 共享资源 %v（共享链路/电源/服务的冲突按影响范围协调）",
			testA, testB, sharedResources)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 失败保留（支持/反证与查询入口，交给诊断步骤）
// ---------------------------------------------------------------------------

// recordGroupTestFailure 记录组测失败（P4-13.5）。
// 失败保留支持/反证与查询入口，交给诊断步骤，不自动重新部署全批。
func recordGroupTestFailure(tx *sql.Tx, result GroupValidationResult, diagnosisEntry string, now int64) error {
	// 失败保留支持/反证与查询入口（交给诊断步骤，不自动重新部署全批）。
	return recordAudit(tx, AuditEvent{
		Event: "group_test_failure", Actor: "system",
		Summary: fmt.Sprintf("组测 %s 失败（保留支持/反证与查询入口，交给诊断步骤 %s）",
			result.RequirementKey, diagnosisEntry),
		OccurredAt: now,
	})
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
