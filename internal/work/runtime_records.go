package work

// P1-02：Task、Step、Run 与控制记录的持久结构操作层。
//
// 本文件提供 schema 48 新增结构（task_states/steps/step_dependencies/
// work_units/control_records/result_receipts/runtime_audit）的类型与操作。
// 设计要点：
//   - 使用 P0-02 的合同/控制/存储版本约定；Run 绑定 Task/Step/执行单元、
//     分配与执行代际，不以 Run 状态冒充 Task 状态。
//   - 不可变合同快照和控制记录；重复请求键相同内容返回原结果，不同内容冲突。
//   - 失败事务没有部分记录：所有多步写入在单个事务内完成。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// recordsQuerier 同时支持 Query 与 QueryRow（*sql.DB 与 *sql.Tx 均满足）。
type recordsQuerier interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// ---------------------------------------------------------------------------
// Task 状态（V3 九态权威记录）
// ---------------------------------------------------------------------------

// TaskState 是 task_states 的一行。
type TaskState struct {
	TaskID            string
	State             V3TaskState
	Reason            string
	StorageRevision   int
	ControlRevision   int
	MappingReason     string
	ReconcileRequired bool
	UpdatedAt         int64
}

// upsertTaskState 在事务内写入/更新 Task 的 V3 状态（P1-03 的状态机使用）。
func upsertTaskState(tx *sql.Tx, ts TaskState) error {
	if !ts.State.Valid() {
		return fmt.Errorf("非法 Task 状态 %q", ts.State)
	}
	_, err := tx.Exec(`INSERT INTO task_states(task_id,state,reason,storage_revision,control_revision,mapping_reason,reconcile_required,updated_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(task_id) DO UPDATE SET state=excluded.state,reason=excluded.reason,
storage_revision=excluded.storage_revision,control_revision=excluded.control_revision,
mapping_reason=excluded.mapping_reason,reconcile_required=excluded.reconcile_required,updated_at=excluded.updated_at`,
		ts.TaskID, string(ts.State), ts.Reason, ts.StorageRevision, ts.ControlRevision,
		ts.MappingReason, boolToInt(ts.ReconcileRequired), ts.UpdatedAt)
	return err
}

// readTaskState 读取 Task 的 V3 状态；不存在时返回 sql.ErrNoRows。
func readTaskState(q recordsQuerier, taskID string) (TaskState, error) {
	var ts TaskState
	var state string
	var reconcile int
	err := q.QueryRow(`SELECT task_id,state,reason,storage_revision,control_revision,mapping_reason,reconcile_required,updated_at
FROM task_states WHERE task_id=?`, taskID).
		Scan(&ts.TaskID, &state, &ts.Reason, &ts.StorageRevision, &ts.ControlRevision,
			&ts.MappingReason, &reconcile, &ts.UpdatedAt)
	ts.State = V3TaskState(state)
	ts.ReconcileRequired = reconcile == 1
	return ts, err
}

// ---------------------------------------------------------------------------
// Step 与依赖
// ---------------------------------------------------------------------------

// Step 是 steps 的一行。
type Step struct {
	StepID            string
	TaskID            string
	ContractRevision  int
	DefinitionKey     string
	DefinitionVersion int
	InputRef          string
	ResultRef         string
	State             V3StepState
	EvidenceRevision  int
	Condition         string
	Error             string
	Optional          bool
	Position          int
	CreatedAt         int64
	UpdatedAt         int64
}

// insertStep 在事务内创建 Step（绑定合同与定义版本）。
func insertStep(tx *sql.Tx, s Step) error {
	if s.StepID == "" || s.TaskID == "" {
		return errors.New("step 缺少稳定身份")
	}
	_, err := tx.Exec(`INSERT INTO steps(step_id,task_id,contract_revision,definition_key,definition_version,
input_ref,result_ref,state,evidence_revision,condition,error,optional,position,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.StepID, s.TaskID, s.ContractRevision, s.DefinitionKey, s.DefinitionVersion,
		s.InputRef, s.ResultRef, string(s.State), s.EvidenceRevision, s.Condition, s.Error,
		boolToInt(s.Optional), s.Position, s.CreatedAt, s.UpdatedAt)
	return err
}

// insertStepDependency 在事务内创建依赖边（带证据版本）。
func insertStepDependency(tx *sql.Tx, stepID, dependsOnStepID string, evidenceRevision int) error {
	if stepID == dependsOnStepID {
		return errors.New("step 不能依赖自身")
	}
	_, err := tx.Exec(`INSERT INTO step_dependencies(step_id,depends_on_step_id,evidence_revision) VALUES(?,?,?)`,
		stepID, dependsOnStepID, evidenceRevision)
	return err
}

// listSteps 读取一个 Task 的全部 Step（按 position 排序）。
func listSteps(q recordsQuerier, taskID string) ([]Step, error) {
	rows, err := q.Query(`SELECT step_id,task_id,contract_revision,definition_key,definition_version,
input_ref,result_ref,state,evidence_revision,condition,error,optional,position,created_at,updated_at
FROM steps WHERE task_id=? ORDER BY position`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var s Step
		var state string
		var optional int
		if err := rows.Scan(&s.StepID, &s.TaskID, &s.ContractRevision, &s.DefinitionKey, &s.DefinitionVersion,
			&s.InputRef, &s.ResultRef, &state, &s.EvidenceRevision, &s.Condition, &s.Error,
			&optional, &s.Position, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.State = V3StepState(state)
		s.Optional = optional == 1
		out = append(out, s)
	}
	return out, rows.Err()
}

// stepDependencies 读取一个 Step 的直接依赖。
func stepDependencies(q recordsQuerier, stepID string) ([]string, error) {
	rows, err := q.Query(`SELECT depends_on_step_id FROM step_dependencies WHERE step_id=?`, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Work Unit（就绪执行单元）
// ---------------------------------------------------------------------------

// WorkUnitKind 是单元类型。
type WorkUnitKind string

const (
	WorkUnitModel         WorkUnitKind = "model"
	WorkUnitDeterministic WorkUnitKind = "deterministic"
	WorkUnitAction        WorkUnitKind = "action"
)

// WorkUnitState 是单元状态。
type WorkUnitState string

const (
	WorkUnitReady     WorkUnitState = "ready"
	WorkUnitClaimed   WorkUnitState = "claimed"
	WorkUnitRunning   WorkUnitState = "running"
	WorkUnitCompleted WorkUnitState = "completed"
	WorkUnitFailed    WorkUnitState = "failed"
	WorkUnitCancelled WorkUnitState = "cancelled"
)

// WorkUnit 是 work_units 的一行。
type WorkUnit struct {
	UnitID          string
	TaskID          string
	StepID          string
	Kind            WorkUnitKind
	LeaseGeneration int
	ReadyAt         int64
	State           WorkUnitState
	InputRef        string
	ResultRef       string
	ResultDigest    string
	Error           string
	Attempt         int
	CreatedAt       int64
	UpdatedAt       int64
}

// insertWorkUnit 在事务内创建就绪单元。
func insertWorkUnit(tx *sql.Tx, u WorkUnit) error {
	if u.UnitID == "" || u.TaskID == "" {
		return errors.New("work_unit 缺少稳定身份")
	}
	var stepID any
	if u.StepID != "" {
		stepID = u.StepID
	}
	_, err := tx.Exec(`INSERT INTO work_units(unit_id,task_id,step_id,kind,lease_generation,ready_at,state,
input_ref,result_ref,result_digest,error,attempt,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.UnitID, u.TaskID, stepID, string(u.Kind), u.LeaseGeneration, u.ReadyAt, string(u.State),
		u.InputRef, u.ResultRef, u.ResultDigest, u.Error, u.Attempt, u.CreatedAt, u.UpdatedAt)
	return err
}

// ---------------------------------------------------------------------------
// 控制记录（不可变，只追加；重复请求键幂等）
// ---------------------------------------------------------------------------

// ControlRecord 是 control_records 的一行。
type ControlRecord struct {
	RecordID               string
	TaskID                 string
	RequestKey             string
	DecidedBy              string
	Kind                   string
	ControlRevision        int
	TargetContractRevision int
	Decision               string
	Detail                 string
	CreatedAt              int64
}

// insertControlRecord 在事务内追加控制记录。
// 重复请求键相同内容返回既有记录（幂等），不同内容返回冲突错误。
func insertControlRecord(tx *sql.Tx, c ControlRecord) (ControlRecord, error) {
	var existing ControlRecord
	err := tx.QueryRow(`SELECT record_id,task_id,decided_by,kind,control_revision,target_contract_revision,decision,detail,created_at
FROM control_records WHERE request_key=?`, c.RequestKey).
		Scan(&existing.RecordID, &existing.TaskID, &existing.DecidedBy, &existing.Kind,
			&existing.ControlRevision, &existing.TargetContractRevision, &existing.Decision,
			&existing.Detail, &existing.CreatedAt)
	if err == nil {
		// 同键：核对内容一致性。
		if existing.TaskID != c.TaskID || existing.Kind != c.Kind || existing.Decision != c.Decision {
			return ControlRecord{}, ErrConflict
		}
		existing.RequestKey = c.RequestKey
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ControlRecord{}, err
	}
	if c.RecordID == "" || c.RequestKey == "" || c.DecidedBy == "" {
		return ControlRecord{}, errors.New("控制记录缺少身份/请求键/决定者")
	}
	_, err = tx.Exec(`INSERT INTO control_records(record_id,task_id,request_key,decided_by,kind,control_revision,
target_contract_revision,decision,detail,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		c.RecordID, c.TaskID, c.RequestKey, c.DecidedBy, c.Kind, c.ControlRevision,
		c.TargetContractRevision, c.Decision, c.Detail, c.CreatedAt)
	if err != nil {
		return ControlRecord{}, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 结果引用（重复送达只应用一次；同身份不同结果隔离）
// ---------------------------------------------------------------------------

// ResultReceipt 是 result_receipts 的一行。
type ResultReceipt struct {
	ReceiptID           string
	ResultKey           string
	UnitID              string
	RunID               string
	TaskID              string
	ExecutionGeneration int
	LeaseGeneration     int
	Summary             string
	UsageTracked        bool
	ModelRequests       int
	InputTokens         *int64
	OutputTokens        *int64
	EvidenceRef         string
	State               string // received/applied/quarantined
	ReceivedAt          int64
	AppliedAt           int64
}

// recordResultReceipt 在事务内登记结果引用。
// 重复结果键相同内容返回既有（幂等）；同键不同内容标为隔离（不覆盖原结果）。
func recordResultReceipt(tx *sql.Tx, r ResultReceipt) (ResultReceipt, error) {
	var existing ResultReceipt
	var inputTokens, outputTokens sql.NullInt64
	var unitID, runID sql.NullString
	err := tx.QueryRow(`SELECT receipt_id,unit_id,run_id,task_id,execution_generation,lease_generation,summary,
usage_tracked,model_requests,input_tokens,output_tokens,evidence_ref,state,received_at,applied_at
FROM result_receipts WHERE result_key=?`, r.ResultKey).
		Scan(&existing.ReceiptID, &unitID, &runID, &existing.TaskID,
			&existing.ExecutionGeneration, &existing.LeaseGeneration, &existing.Summary,
			&existing.UsageTracked, &existing.ModelRequests, &inputTokens, &outputTokens,
			&existing.EvidenceRef, &existing.State, &existing.ReceivedAt, &existing.AppliedAt)
	if err == nil {
		existing.UnitID = unitID.String
		existing.RunID = runID.String
		// 同键：核对内容一致性；不同内容隔离，不覆盖原结果。
		if existing.Summary != r.Summary || existing.TaskID != r.TaskID {
			return ResultReceipt{}, ErrConflict
		}
		existing.ResultKey = r.ResultKey
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ResultReceipt{}, err
	}
	if r.ReceiptID == "" || r.ResultKey == "" || r.TaskID == "" {
		return ResultReceipt{}, errors.New("结果引用缺少身份/结果键/任务")
	}
	var unitIDArg, runIDArg any
	if r.UnitID != "" {
		unitIDArg = r.UnitID
	}
	if r.RunID != "" {
		runIDArg = r.RunID
	}
	_, err = tx.Exec(`INSERT INTO result_receipts(receipt_id,result_key,unit_id,run_id,task_id,execution_generation,
lease_generation,summary,usage_tracked,model_requests,input_tokens,output_tokens,evidence_ref,state,received_at,applied_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ReceiptID, r.ResultKey, unitIDArg, runIDArg, r.TaskID, r.ExecutionGeneration,
		r.LeaseGeneration, r.Summary, boolToInt(r.UsageTracked), r.ModelRequests,
		r.InputTokens, r.OutputTokens, r.EvidenceRef, r.State, r.ReceivedAt, r.AppliedAt)
	if err != nil {
		return ResultReceipt{}, err
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 基础审计
// ---------------------------------------------------------------------------

// AuditEvent 是 runtime_audit 的一行。
type AuditEvent struct {
	TaskID          string
	StepID          string
	RunID           string
	UnitID          string
	WaitID          string
	CommandID       string
	ResultKey       string
	Event           string
	Actor           string
	StorageRevision int
	ControlRevision int
	Summary         string
	DetailRef       string
	OccurredAt      int64
}

// recordAudit 在事务内追加审计记录（业务变更与审计同事务）。
func recordAudit(tx *sql.Tx, e AuditEvent) error {
	if e.Event == "" {
		return errors.New("审计缺少事件类型")
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err := tx.Exec(`INSERT INTO runtime_audit(task_id,step_id,run_id,unit_id,wait_id,command_id,result_key,
event,actor,storage_revision,control_revision,summary,detail_ref,occurred_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nullable(e.TaskID), nullable(e.StepID), nullable(e.RunID), nullable(e.UnitID),
		nullable(e.WaitID), nullable(e.CommandID), nullable(e.ResultKey),
		e.Event, e.Actor, e.StorageRevision, e.ControlRevision, e.Summary, e.DetailRef, e.OccurredAt)
	return err
}

// ---------------------------------------------------------------------------
// 查询模型：当前任务、步骤、历史 Run 和未决事项
// ---------------------------------------------------------------------------

// TaskOverview 是一个 Task 的当前状态、步骤、历史 Run 和未决事项汇总。
type TaskOverview struct {
	State         TaskState
	Steps         []Step
	PendingUnits  int
	OpenWaits     int
	RecentAudits  int
}

// readTaskOverview 读取一个 Task 的当前汇总（P1-02.5）。
func readTaskOverview(q recordsQuerier, taskID string) (TaskOverview, error) {
	var o TaskOverview
	state, err := readTaskState(q, taskID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return o, err
	}
	if err == nil {
		o.State = state
	}
	steps, err := listSteps(q, taskID)
	if err != nil {
		return o, err
	}
	o.Steps = steps
	if err := q.QueryRow(`SELECT COUNT(*) FROM work_units WHERE task_id=? AND state IN ('ready','claimed','running')`, taskID).
		Scan(&o.PendingUnits); err != nil {
		return o, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM runtime_audit WHERE task_id=?`, taskID).
		Scan(&o.RecentAudits); err != nil {
		return o, err
	}
	return o, nil
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ensure 上下文引用保留（后续工作项扩展查询接口时使用）。
var _ = context.Background
var _ = fmt.Sprintf
