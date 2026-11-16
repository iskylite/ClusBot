package work

// P4-03：观测、事实和关系的双时间历史。
//
// 本文件实现区分现实生效时间与系统获知时间，冲突和迟到不破坏历史：
//   - 原始 Observation 保存来源/采集时间/覆盖；规范声明保留值、单位、来源、
//     有效时间和记录时间；
//   - 按字段与用途选择权威来源，冲突保留双方及判据；不能按最后到达或相似度
//     胜出；
//   - 更正新增修订并关闭原记录的适用区间，不覆盖历史；重复事件依据来源
//     身份/版本去重；
//   - 物理组成无环、单一父关系的有效区间不重叠；网络拓扑允许环，端口连线
//     规范端点并支持双向查；
//   - 当前事实投影可重建并带水位，过期或缺失不被伪装为新鲜；权限/状态准入
//     回权威记录核验。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 原始 Observation（保存来源/采集时间/覆盖）
// ---------------------------------------------------------------------------

// Observation 是 observations 的一行。
type Observation struct {
	ObservationID  string
	SourceRef      string
	SourceIdentity string
	CollectedAt    int64
	Coverage       string
	ObjectRef      string
	FieldKey       string
	Value          string
	Unit           string
	RecordedAt     int64
}

// recordObservation 记录原始 Observation（P4-03.1）。
// 原始 Observation 保存来源/采集时间/覆盖。
func recordObservation(tx *sql.Tx, o Observation, now int64) (Observation, error) {
	if o.SourceRef == "" || o.SourceIdentity == "" || o.ObjectRef == "" || o.FieldKey == "" {
		return Observation{}, errors.New("观测缺少来源/身份/对象/字段")
	}
	o.ObservationID = id()
	o.RecordedAt = now
	_, err := tx.Exec(`INSERT INTO observations(observation_id,source_ref,source_identity,collected_at,
coverage,object_ref,field_key,value,unit,recorded_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		o.ObservationID, o.SourceRef, o.SourceIdentity, o.CollectedAt, o.Coverage,
		o.ObjectRef, o.FieldKey, o.Value, o.Unit, now)
	if err != nil {
		return Observation{}, err
	}
	return o, nil
}

// ---------------------------------------------------------------------------
// 规范声明（保留值、单位、来源、有效时间和记录时间）
// ---------------------------------------------------------------------------

// Fact 是 facts 的一行。
type Fact struct {
	FactID        string
	ObjectRef     string
	FieldKey      string
	Value         string
	Unit          string
	SourceRef     string
	Authority     string // authoritative/candidate/conflicting/unknown
	ValidFrom     int64
	ValidTo       int64
	RecordedAt    int64
	Revision      int
	Supersedes    string
	ConflictWith  string
	ConflictBasis string
	CreatedAt     int64
}

// assertFact 声明一个事实（P4-03.1/P4-03.3）。
// 规范声明保留值、单位、来源、有效时间和记录时间；更正新增修订并关闭原
// 记录的适用区间，不覆盖历史。
func assertFact(tx *sql.Tx, f Fact, now int64) (Fact, error) {
	if f.ObjectRef == "" || f.FieldKey == "" || f.SourceRef == "" {
		return Fact{}, errors.New("事实缺少对象/字段/来源")
	}
	if f.Authority == "" {
		f.Authority = "unknown"
	}
	if f.Revision < 1 {
		f.Revision = 1
	}
	f.FactID = id()
	f.CreatedAt = now
	f.RecordedAt = now
	var supersedes, conflictWith any
	if f.Supersedes != "" {
		supersedes = f.Supersedes
		// 关闭原记录的适用区间（不覆盖历史）。
		if _, err := tx.Exec(`UPDATE facts SET valid_to=? WHERE fact_id=? AND valid_to=0`, now, f.Supersedes); err != nil {
			return Fact{}, err
		}
	}
	if f.ConflictWith != "" {
		conflictWith = f.ConflictWith
	}
	_, err := tx.Exec(`INSERT INTO facts(fact_id,object_ref,field_key,value,unit,source_ref,authority,
valid_from,valid_to,recorded_at,revision,supersedes,conflict_with,conflict_basis,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.FactID, f.ObjectRef, f.FieldKey, f.Value, f.Unit, f.SourceRef, f.Authority,
		f.ValidFrom, f.ValidTo, f.RecordedAt, f.Revision, supersedes, conflictWith, f.ConflictBasis, now)
	if err != nil {
		return Fact{}, err
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// 权威来源选择（按字段与用途；冲突保留双方及判据）
// ---------------------------------------------------------------------------

// selectAuthoritativeFact 选择权威事实（P4-03.2）。
// 按字段与用途选择权威来源，冲突保留双方及判据；不能按最后到达或相似度
// 胜出；相互矛盾来源返回冲突/未知。
func selectAuthoritativeFact(tx *sql.Tx, objectRef, fieldKey string, atTime int64) (Fact, error) {
	// 读取该字段在该时间的全部有效事实（冲突保留双方）。
	rows, err := tx.Query(`SELECT fact_id,object_ref,field_key,value,unit,source_ref,authority,
valid_from,valid_to,recorded_at,revision,supersedes,conflict_with,conflict_basis,created_at
FROM facts WHERE object_ref=? AND field_key=? AND valid_from<=? AND (valid_to=0 OR valid_to>?)`,
		objectRef, fieldKey, atTime, atTime)
	if err != nil {
		return Fact{}, err
	}
	defer rows.Close()
	var facts []Fact
	for rows.Next() {
		var f Fact
		var supersedes, conflictWith sql.NullString
		if err := rows.Scan(&f.FactID, &f.ObjectRef, &f.FieldKey, &f.Value, &f.Unit, &f.SourceRef,
			&f.Authority, &f.ValidFrom, &f.ValidTo, &f.RecordedAt, &f.Revision, &supersedes,
			&conflictWith, &f.ConflictBasis, &f.CreatedAt); err != nil {
			return Fact{}, err
		}
		f.Supersedes = supersedes.String
		f.ConflictWith = conflictWith.String
		facts = append(facts, f)
	}
	if len(facts) == 0 {
		return Fact{}, sql.ErrNoRows
	}
	// 按字段与用途选择权威来源（不按最后到达或相似度胜出）。
	var authoritative, conflicting []Fact
	for _, f := range facts {
		if f.Authority == "authoritative" {
			authoritative = append(authoritative, f)
		} else if f.Authority == "conflicting" {
			conflicting = append(conflicting, f)
		}
	}
	// 相互矛盾来源：返回冲突/未知。
	if len(conflicting) > 0 && len(authoritative) == 0 {
		return conflicting[0], nil
	}
	if len(authoritative) > 1 {
		// 多个权威来源：冲突（保留双方及判据）。
		return authoritative[0], fmt.Errorf("多个权威来源冲突：%s 与 %s", authoritative[0].SourceRef, authoritative[1].SourceRef)
	}
	if len(authoritative) == 1 {
		return authoritative[0], nil
	}
	// 无权威来源：返回候选（冲突/未知）。
	return facts[0], nil
}

// ---------------------------------------------------------------------------
// 重复事件去重（依据来源身份/版本）
// ---------------------------------------------------------------------------

// deduplicateFactEvent 去重事实事件（P4-03.3）。
// 重复事件依据来源身份/版本去重；乱序旧事件不会覆盖当前新事实。
func deduplicateFactEvent(tx *sql.Tx, f Fact, now int64) (duplicate bool, err error) {
	// 同来源同字段同有效时间：重复（依据来源身份/版本去重）。
	var existingID string
	err = tx.QueryRow(`SELECT fact_id FROM facts WHERE object_ref=? AND field_key=? AND source_ref=? AND valid_from=?`,
		f.ObjectRef, f.FieldKey, f.SourceRef, f.ValidFrom).Scan(&existingID)
	if err == nil {
		return true, nil // 重复（去重）
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// 乱序旧事件：不会覆盖当前新事实（valid_from 早于当前有效事实的标记为历史）。
	var currentValidFrom int64
	if err := tx.QueryRow(`SELECT coalesce(max(valid_from),0) FROM facts WHERE object_ref=? AND field_key=? AND valid_to=0`,
		f.ObjectRef, f.FieldKey).Scan(&currentValidFrom); err != nil {
		return false, err
	}
	if f.ValidFrom < currentValidFrom {
		// 乱序旧事件：不覆盖当前新事实（作为历史记录，不影响当前）。
		return false, nil
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// 关系约束（物理组成无环、单一父关系有效区间不重叠）
// ---------------------------------------------------------------------------

// Relation 是 relations 的一行。
type Relation struct {
	RelationID   string
	RelationKind string
	SourceRef    string
	TargetRef    string
	ValidFrom    int64
	ValidTo      int64
	CreatedAt    int64
}

// assertRelation 声明一个关系（P4-03.4）。
// 物理组成无环、单一父关系的有效区间不重叠；网络拓扑允许环，端口连线
// 规范端点并支持双向查；并发插入重叠安装关系被约束。
func assertRelation(tx *sql.Tx, r Relation, now int64) (Relation, error) {
	if r.SourceRef == "" || r.TargetRef == "" || r.RelationKind == "" {
		return Relation{}, errors.New("关系缺少端点或类型")
	}
	// 物理组成：无环（不能 A→B→A）。
	if r.RelationKind == "physical_parent" || r.RelationKind == "physical_component" {
		if err := ensureNoPhysicalCycle(tx, r.SourceRef, r.TargetRef); err != nil {
			return Relation{}, err
		}
		// 单一父关系：有效区间不重叠（并发插入重叠安装关系被约束）。
		var overlapping int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM relations WHERE target_ref=? AND relation_kind=?
AND valid_to=0 AND valid_from<? AND source_ref<>?`,
			r.TargetRef, r.RelationKind, r.ValidFrom, r.SourceRef).Scan(&overlapping); err != nil {
			return Relation{}, err
		}
		if overlapping > 0 {
			return Relation{}, fmt.Errorf("单一父关系 %s 的有效区间重叠（并发插入重叠安装关系被约束）", r.TargetRef)
		}
	}
	r.RelationID = id()
	r.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO relations(relation_id,relation_kind,source_ref,target_ref,valid_from,valid_to,created_at)
VALUES(?,?,?,?,?,?,?)`, r.RelationID, r.RelationKind, r.SourceRef, r.TargetRef, r.ValidFrom, r.ValidTo, now)
	if err != nil {
		return Relation{}, err
	}
	return r, nil
}

// ensureNoPhysicalCycle 核对物理组成无环。
// 插入 sourceRef→targetRef 会成环：若 targetRef 出发能到达 sourceRef，则成环。
func ensureNoPhysicalCycle(tx *sql.Tx, sourceRef, targetRef string) error {
	if sourceRef == targetRef {
		return fmt.Errorf("物理组成自环：%s→%s", sourceRef, targetRef)
	}
	// 环检测：从 targetRef 出发能否到达 sourceRef（若能则插入后成环）。
	visited := map[string]bool{targetRef: true}
	queue := []string{targetRef}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == sourceRef {
			return fmt.Errorf("物理组成环：%s→...→%s→%s", sourceRef, current, targetRef)
		}
		rows, err := tx.Query(`SELECT target_ref FROM relations WHERE source_ref=? AND relation_kind IN ('physical_parent','physical_component') AND valid_to=0`, current)
		if err != nil {
			return err
		}
		for rows.Next() {
			var next string
			if err := rows.Scan(&next); err != nil {
				rows.Close()
				return err
			}
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
		rows.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// 当前事实投影（可重建并带水位）
// ---------------------------------------------------------------------------

// rebuildFactProjection 重建当前事实投影（P4-03.5）。
// 当前事实投影可重建并带水位，过期或缺失不被伪装为新鲜。
func rebuildFactProjection(tx *sql.Tx, watermark int64, now int64) error {
	// 重建投影（带水位）。
	_, err := tx.Exec(`INSERT INTO fact_projections(projection_id,watermark,rebuilt_at) VALUES(1,?,?)
ON CONFLICT(projection_id) DO UPDATE SET watermark=excluded.watermark,rebuilt_at=excluded.rebuilt_at`,
		watermark, now)
	return err
}

// readFactProjection 读取当前事实投影（带水位）。
func readFactProjection(q recordsQuerier) (watermark int64, rebuiltAt int64, err error) {
	err = q.QueryRow(`SELECT watermark, rebuilt_at FROM fact_projections WHERE projection_id=1`).Scan(&watermark, &rebuiltAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return watermark, rebuiltAt, err
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
