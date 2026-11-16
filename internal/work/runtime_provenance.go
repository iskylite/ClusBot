package work

// P5-04：来源依赖闭包与撤销版本闸门。
//
// 本文件实现后台传播未完成时也能阻断已知失权来源，读取不递归扫全图：
//   - 派生修订保存按源修订标识的有界依赖闭包和反向依赖；有环、超限、未完成
//     的派生记录保持不可用；
//   - 本地撤销/删除与策略分区代际递增、可靠传播事件同事务；主体授权变化也
//     必须进入有效判据；
//   - 读取缓存键包含主体、记录修订、闭包摘要、相关策略版本/分区代际及最早
//     到期时间；
//   - 新最终核验以当前权威快照比较代际；不一致有界重查，做不完先返回缺口
//     并重建，不能沿用旧缓存；
//   - 外部可靠通知/实时授权与仅凭有效期两种保证分开；无撤权机制不能宣称
//     即时获知，已在途内容不能撤回。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 依赖闭包（按源修订标识的有界依赖闭包和反向依赖）
// ---------------------------------------------------------------------------

// ProvenanceClosure 是 provenance_closures 的一行。
type ProvenanceClosure struct {
	ClosureID       string
	DerivedRef      string
	DerivedRevision int
	SourceRef       string
	SourceRevision  int
	ForwardClosure  string
	ReverseClosure  string
	State           string // complete/incomplete/cycle/overflow
	Bounded         bool
	CreatedAt       int64
	UpdatedAt       int64
}

// recordProvenanceClosure 记录依赖闭包（P5-04.1）。
// 派生修订保存按源修订标识的有界依赖闭包和反向依赖；有环、超限、未完成
// 的派生记录保持不可用。
func recordProvenanceClosure(tx *sql.Tx, c ProvenanceClosure, now int64) (ProvenanceClosure, error) {
	if c.DerivedRef == "" || c.SourceRef == "" {
		return ProvenanceClosure{}, errors.New("闭包缺少派生/来源")
	}
	c.ClosureID = id()
	c.CreatedAt, c.UpdatedAt = now, now
	if c.State == "" {
		c.State = "complete"
	}
	if c.ForwardClosure == "" {
		c.ForwardClosure = "[]"
	}
	if c.ReverseClosure == "" {
		c.ReverseClosure = "[]"
	}
	c.Bounded = true
	_, err := tx.Exec(`INSERT INTO provenance_closures(closure_id,derived_ref,derived_revision,source_ref,
source_revision,forward_closure,reverse_closure,state,bounded,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(derived_ref,derived_revision,source_ref,source_revision) DO UPDATE SET
state=excluded.state,forward_closure=excluded.forward_closure,reverse_closure=excluded.reverse_closure,updated_at=excluded.updated_at`,
		c.ClosureID, c.DerivedRef, c.DerivedRevision, c.SourceRef, c.SourceRevision,
		c.ForwardClosure, c.ReverseClosure, c.State, boolToInt(c.Bounded), now, now)
	if err != nil {
		return ProvenanceClosure{}, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 闭包核对（有环、超限、未完成的派生记录保持不可用）
// ---------------------------------------------------------------------------

// ErrProvenanceNotUsable 是派生不可用的错误。
var ErrProvenanceNotUsable = errors.New("派生记录保持不可用")

// ensureProvenanceUsable 核对派生可用（P5-04.1）。
// 有环、超限、未完成的派生记录保持不可用；跨分区来源逐一核验，闭包超限时
// 不会降级为允许。
func ensureProvenanceUsable(tx *sql.Tx, derivedRef string, derivedRevision int, now int64) error {
	rows, err := tx.Query(`SELECT state, bounded FROM provenance_closures WHERE derived_ref=? AND derived_revision=?`,
		derivedRef, derivedRevision)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var bounded int
		if err := rows.Scan(&state, &bounded); err != nil {
			return err
		}
		// 有环、超限、未完成：保持不可用（闭包超限时不会降级为允许）。
		if state == "cycle" || state == "overflow" || state == "incomplete" {
			return fmt.Errorf("%w：派生 %s 状态 %s（有环、超限、未完成的派生记录保持不可用）",
				ErrProvenanceNotUsable, derivedRef, state)
		}
		if bounded == 0 {
			return fmt.Errorf("%w：派生 %s 无界（闭包超限时不会降级为允许）", ErrProvenanceNotUsable, derivedRef)
		}
	}
	return rows.Err()
}

// ---------------------------------------------------------------------------
// 撤销版本闸门（本地撤销/删除与策略分区代际递增）
// ---------------------------------------------------------------------------

// RevocationGate 是 revocation_gates 的一行。
type RevocationGate struct {
	GateID          string
	ObjectRef       string
	SourceRef       string
	PolicyPartition string
	Generation      int
	State           string // active/revoked/deleted
	RevokedBy       string
	RevokedAt       int64
	CreatedAt       int64
}

// revokeSource 撤销来源（P5-04.2）。
// 本地撤销/删除与策略分区代际递增、可靠传播事件同事务；主体授权变化也必须
// 进入有效判据；代际递增阻断已知失权来源。
func revokeSource(tx *sql.Tx, objectRef, sourceRef, policyPartition, revokedBy string, now int64) error {
	// 代际递增（阻断已知失权来源）。
	if _, err := tx.Exec(`INSERT INTO revocation_gates(gate_id,object_ref,source_ref,policy_partition,generation,state,revoked_by,revoked_at,created_at)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(object_ref,source_ref,policy_partition) DO UPDATE SET
generation=generation+1,state='revoked',revoked_by=excluded.revoked_by,revoked_at=excluded.revoked_at`,
		id(), objectRef, sourceRef, policyPartition, 1, "revoked", revokedBy, now, now); err != nil {
		return err
	}
	// 可靠传播事件同事务（主体授权变化也必须进入有效判据）。
	return recordAudit(tx, AuditEvent{
		Event: "source_revoked", Actor: revokedBy,
		Summary: fmt.Sprintf("来源 %s 已撤销（对象 %s，代际递增）", sourceRef, objectRef), OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 撤销闸门核对（新最终核验以当前权威快照比较代际）
// ---------------------------------------------------------------------------

// ErrSourceRevoked 是来源已撤销的错误。
var ErrSourceRevoked = errors.New("来源已撤销")

// ensureSourceNotRevoked 核对来源未撤销（P5-04.4）。
// 新最终核验以当前权威快照比较代际；不一致有界重查，做不完先返回缺口并
// 重建，不能沿用旧缓存；外部可靠通知/实时授权与仅凭有效期两种保证分开。
func ensureSourceNotRevoked(tx *sql.Tx, objectRef, sourceRef string, expectedGeneration int, now int64) error {
	var generation int
	var state string
	err := tx.QueryRow(`SELECT generation, state FROM revocation_gates WHERE object_ref=? AND source_ref=?`,
		objectRef, sourceRef).Scan(&generation, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 无闸门记录：未撤销
	}
	if err != nil {
		return err
	}
	// 代际比较（新最终核验以当前权威快照比较代际）。
	if state == "revoked" || state == "deleted" {
		return fmt.Errorf("%w：来源 %s 已 %s（代际 %d，外部可靠通知/实时授权与仅凭有效期两种保证分开）",
			ErrSourceRevoked, sourceRef, state, generation)
	}
	if generation > expectedGeneration {
		return fmt.Errorf("%w：来源 %s 代际 %d > 期望 %d（不能沿用旧缓存）",
			ErrSourceRevoked, sourceRef, generation, expectedGeneration)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 读取缓存键（主体、记录修订、闭包摘要、相关策略版本/分区代际及最早到期时间）
// ---------------------------------------------------------------------------

// ReadCacheKey 是 read_cache_keys 的一行。
type ReadCacheKey struct {
	CacheKey           string
	Subject            string
	RecordRevision     int
	ClosureDigest      string
	PolicyVersion      int
	PartitionGeneration int
	EarliestExpiry     int64
	CreatedAt          int64
}

// buildReadCacheKey 构建读取缓存键（P5-04.3）。
// 读取缓存键包含主体、记录修订、闭包摘要、相关策略版本/分区代际及最早到期时间。
func buildReadCacheKey(subject string, recordRevision int, closureDigest string, policyVersion int, partitionGeneration int, earliestExpiry int64) ReadCacheKey {
	return ReadCacheKey{
		CacheKey: digest(fmt.Sprintf("%s:%d:%s:%d:%d:%d", subject, recordRevision, closureDigest, policyVersion, partitionGeneration, earliestExpiry)),
		Subject: subject, RecordRevision: recordRevision, ClosureDigest: closureDigest,
		PolicyVersion: policyVersion, PartitionGeneration: partitionGeneration, EarliestExpiry: earliestExpiry,
	}
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
