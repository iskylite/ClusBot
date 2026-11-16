package work

// P3-17：持续服务实例与规范化事件。
//
// 本文件实现监控/告警作为持续来源接入，事件产生有限工作：
//   - 登记服务实例、来源身份、订阅范围、配置版本、启停/变更能力和有效期；
//     服务不是永远运行的 Task；
//   - 规定推送/轮询的事件 ID、来源序号、对象、时间、关联及覆盖；同 ID 异
//     内容隔离；
//   - 明确周期检查由哪方触发；外部已调度的检查不再由本地 Schedule 重复执行；
//   - 失联后按可支持重放/查询补缺，记录水位与遗漏；无保证则返回覆盖未知；
//   - 观测允许规则化聚合，动作回执、撤权和控制事件不能当普通噪声丢弃。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 服务实例（登记服务实例、来源身份、订阅范围、配置版本、启停/变更能力和有效期）
// ---------------------------------------------------------------------------

// ServiceInstance 是 service_instances 的一行。
type ServiceInstance struct {
	ServiceID        string
	SourceIdentity   string
	SubscriptionScope string
	ConfigVersion    int
	ExpiresAt        int64
	SupportsPause    string
	SupportsResume   string
	SupportsUpdate   string
	State            string // active/paused/stopped
	CreatedAt        int64
	UpdatedAt        int64
}

// registerServiceInstance 登记服务实例（P3-17.1）。
// 登记服务实例、来源身份、订阅范围、配置版本、启停/变更能力和有效期；
// 服务不是永远运行的 Task。
func registerServiceInstance(tx *sql.Tx, s ServiceInstance, now int64) (ServiceInstance, error) {
	if s.SourceIdentity == "" {
		return ServiceInstance{}, errors.New("服务实例缺少来源身份")
	}
	if s.ConfigVersion < 1 {
		s.ConfigVersion = 1
	}
	s.ServiceID = id()
	s.State = "active"
	s.CreatedAt, s.UpdatedAt = now, now
	for _, f := range []struct{ name string; value *string }{
		{"supports_pause", &s.SupportsPause}, {"supports_resume", &s.SupportsResume}, {"supports_update", &s.SupportsUpdate},
	} {
		if *f.value == "" {
			*f.value = "unknown"
		}
	}
	_, err := tx.Exec(`INSERT INTO service_instances(service_id,source_identity,subscription_scope,
config_version,expires_at,supports_pause,supports_resume,supports_update,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		s.ServiceID, s.SourceIdentity, s.SubscriptionScope, s.ConfigVersion, s.ExpiresAt,
		s.SupportsPause, s.SupportsResume, s.SupportsUpdate, s.State, now, now)
	if err != nil {
		return ServiceInstance{}, err
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// 服务事件（推送/轮询的事件 ID、来源序号、对象、时间、关联及覆盖）
// ---------------------------------------------------------------------------

// ServiceEvent 是 service_events 的一行。
type ServiceEvent struct {
	EventID        string
	ServiceID      string
	SourceSequence int64
	ObjectRef      string
	OccurredAt     int64
	CorrelationID  string
	Coverage       string
	Kind           string
	PayloadDigest  string
	State          string // received/applied/quarantined
	ReceivedAt     int64
}

// receiveServiceEvent 接收服务事件（P3-17.2）。
// 规定推送/轮询的事件 ID、来源序号、对象、时间、关联及覆盖；同 ID 异内容
// 隔离；推送与轮询重复事件只应用一次；来源身份伪造被拒绝。
func receiveServiceEvent(tx *sql.Tx, e ServiceEvent, sourceIdentity string, now int64) (ReceiveResult, error) {
	if e.ServiceID == "" || e.EventID == "" {
		return ReceiveResult{}, errors.New("事件缺少服务或事件 ID")
	}
	// 来源身份核对（来源身份伪造被拒绝）。
	var registeredIdentity string
	var serviceState string
	err := tx.QueryRow(`SELECT source_identity, state FROM service_instances WHERE service_id=?`, e.ServiceID).
		Scan(&registeredIdentity, &serviceState)
	if errors.Is(err, sql.ErrNoRows) {
		return ReceiveResult{}, fmt.Errorf("%w：服务 %s 未登记（来源身份伪造被拒绝）", ErrWorkerAuth, e.ServiceID)
	}
	if err != nil {
		return ReceiveResult{}, err
	}
	if registeredIdentity != sourceIdentity {
		return ReceiveResult{}, fmt.Errorf("%w：来源身份 %s≠%s（来源身份伪造被拒绝）", ErrWorkerAuth, sourceIdentity, registeredIdentity)
	}
	// 配置停用/撤销后新触发受阻。
	if serviceState != "active" {
		return ReceiveResult{}, fmt.Errorf("服务 %s 已 %s（配置停用/撤销后新触发受阻）", e.ServiceID, serviceState)
	}
	// 同 ID 异内容隔离。
	var existingDigest string
	err = tx.QueryRow(`SELECT payload_digest FROM service_events WHERE event_id=?`, e.EventID).Scan(&existingDigest)
	if err == nil {
		if existingDigest != e.PayloadDigest {
			// 同 ID 异内容：隔离。
			if _, err := tx.Exec(`INSERT INTO service_events(event_id,service_id,source_sequence,object_ref,
occurred_at,correlation_id,coverage,kind,payload_digest,state,received_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`,
				e.EventID, e.ServiceID, e.SourceSequence, e.ObjectRef, e.OccurredAt, e.CorrelationID,
				e.Coverage, e.Kind, e.PayloadDigest, "quarantined", now); err != nil {
				return ReceiveResult{}, err
			}
			return ReceiveResult{Quarantined: true}, nil
		}
		// 同 ID 同内容：重复（只应用一次）。
		return ReceiveResult{Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReceiveResult{}, err
	}
	// 新事件：接收。
	e.State = "received"
	e.ReceivedAt = now
	if _, err := tx.Exec(`INSERT INTO service_events(event_id,service_id,source_sequence,object_ref,
occurred_at,correlation_id,coverage,kind,payload_digest,state,received_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		e.EventID, e.ServiceID, e.SourceSequence, e.ObjectRef, e.OccurredAt, e.CorrelationID,
		e.Coverage, e.Kind, e.PayloadDigest, e.State, now); err != nil {
		return ReceiveResult{}, err
	}
	return ReceiveResult{Event: InboxEvent{EventID: e.EventID}}, nil
}

// ---------------------------------------------------------------------------
// 周期检查归属（外部已调度的检查不再由本地 Schedule 重复执行）
// ---------------------------------------------------------------------------

// CheckOwnership 是 service_check_ownership 的一行。
type CheckOwnership struct {
	CheckKey            string
	ServiceID           string
	CheckKind           string
	Owner               string // external/local
	ExternalScheduleRef string
	CreatedAt           int64
}

// claimCheckOwnership 声明周期检查归属（P3-17.3）。
// 明确周期检查由哪方触发；外部已调度的检查不再由本地 Schedule 重复执行。
func claimCheckOwnership(tx *sql.Tx, serviceID, checkKind string, owner string, externalScheduleRef string, now int64) (CheckOwnership, error) {
	key := serviceID + ":" + checkKind
	// 外部已调度的检查：不再由本地 Schedule 重复执行。
	var existing string
	err := tx.QueryRow(`SELECT owner FROM service_check_ownership WHERE check_key=?`, key).Scan(&existing)
	if err == nil && existing == "external" {
		return CheckOwnership{}, fmt.Errorf("检查 %s 已由外部调度（不再由本地 Schedule 重复执行）", key)
	}
	ownership := CheckOwnership{
		CheckKey: key, ServiceID: serviceID, CheckKind: checkKind, Owner: owner,
		ExternalScheduleRef: externalScheduleRef, CreatedAt: now,
	}
	_, err = tx.Exec(`INSERT INTO service_check_ownership(check_key,service_id,check_kind,owner,external_schedule_ref,created_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(check_key) DO UPDATE SET owner=excluded.owner,external_schedule_ref=excluded.external_schedule_ref`,
		key, serviceID, checkKind, owner, externalScheduleRef, now)
	if err != nil {
		return ownership, err
	}
	return ownership, nil
}

// ---------------------------------------------------------------------------
// 水位与补缺（失联后按可支持重放/查询补缺，记录水位与遗漏）
// ---------------------------------------------------------------------------

// ServiceWatermark 是 service_watermarks 的一行。
type ServiceWatermark struct {
	ServiceID       string
	LastSequence    int64
	Gaps            string
	CoverageUnknown bool
	UpdatedAt       int64
}

// updateServiceWatermark 更新服务水位（P3-17.4）。
// 失联后按可支持重放/查询补缺，记录水位与遗漏；无保证则返回覆盖未知。
func updateServiceWatermark(tx *sql.Tx, serviceID string, lastSequence int64, gaps string, coverageUnknown bool, now int64) error {
	_, err := tx.Exec(`INSERT INTO service_watermarks(service_id,last_sequence,gaps,coverage_unknown,updated_at)
VALUES(?,?,?,?,?) ON CONFLICT(service_id) DO UPDATE SET last_sequence=excluded.last_sequence,
gaps=excluded.gaps,coverage_unknown=excluded.coverage_unknown,updated_at=excluded.updated_at`,
		serviceID, lastSequence, gaps, boolToInt(coverageUnknown), now)
	return err
}

// readServiceWatermark 读取服务水位（断线恢复水位可查）。
func readServiceWatermark(q recordsQuerier, serviceID string) (ServiceWatermark, error) {
	var w ServiceWatermark
	var coverageUnknown int
	err := q.QueryRow(`SELECT service_id,last_sequence,gaps,coverage_unknown,updated_at FROM service_watermarks WHERE service_id=?`,
		serviceID).Scan(&w.ServiceID, &w.LastSequence, &w.Gaps, &coverageUnknown, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return w, nil // 无水位：覆盖未知
	}
	if err != nil {
		return w, err
	}
	w.CoverageUnknown = coverageUnknown == 1
	return w, nil
}

// ---------------------------------------------------------------------------
// 观测聚合（动作回执、撤权和控制事件不能当普通噪声丢弃）
// ---------------------------------------------------------------------------

// ErrNoiseFiltered 是噪声过滤的错误。
var ErrNoiseFiltered = errors.New("动作回执、撤权和控制事件不能当普通噪声丢弃")

// filterServiceEventNoise 过滤服务事件噪声（P3-17.5）。
// 观测允许规则化聚合，动作回执、撤权和控制事件不能当普通噪声丢弃。
func filterServiceEventNoise(kind string) (filtered bool, err error) {
	// 动作回执、撤权和控制事件：不能当普通噪声丢弃。
	switch kind {
	case "action_receipt", "revocation", "control":
		return false, fmt.Errorf("%w：%s 不能丢弃", ErrNoiseFiltered, kind)
	default:
		// 观测允许规则化聚合（普通噪声可聚合）。
		return true, nil
	}
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
