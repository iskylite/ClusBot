package work

// A2+A3：只读领域任务闭环——查询某资产的验收状态。
//
// 本文件把"查询某资产的验收状态"这类明确只读意图接入 V3 执行链：
//   - 受理时建立 V3 Task（pending）+ 一个确定性 Step（ready）+ 就绪 work_unit；
//   - 本地嵌入执行单元消费：claim → 执行只读查询 → 结果落盘（result_receipt）
//     → 原子完成 step 与 work_unit；
//   - 结果是只读快照（ops_assets + acceptance_coverage + delivery_records），
//     不修改任何领域状态；幂等：同请求键返回同一任务。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// 意图识别（明确只读意图，不接模型）
// ---------------------------------------------------------------------------

// AssetAcceptanceQueryRef 从输入中识别"查询某资产验收状态"的明确意图。
// 返回资产 ObjectRef；无法识别返回空。规则：包含"验收状态"且含资产引用。
// 资产引用形如 "node-1"/"asset:xxx"/明确 object_ref（不含空格）。
var assetAcceptanceIntent = regexp.MustCompile(`^(?:请)?(?:帮我)?(?:查询|查一下|查看|查)\s*(?:资产\s+)?([\p{L}\p{N}_.:@-]+?)\s*(?:的\s*)?验收状态[。？！?!]?$`)

func AssetAcceptanceQueryRef(text string) string {
	matches := assetAcceptanceIntent.FindStringSubmatch(strings.TrimSpace(text))
	if len(matches) == 0 {
		return ""
	}
	return matches[1]
}

// ---------------------------------------------------------------------------
// 只读验收状态快照
// ---------------------------------------------------------------------------

// AssetAcceptanceStatus 是一个资产的验收状态只读快照。
type AssetAcceptanceStatus struct {
	ObjectRef string `json:"object_ref"`
	Kind      string `json:"kind"`
	Identity  string `json:"identity"`
	State     string `json:"state"`
	Revision  int    `json:"revision"`
	Frozen    bool   `json:"frozen"`
	// 验收覆盖（requirement_key → coverage_state）。
	Coverage map[string]string `json:"coverage"`
	// 交付阶段记录（按时间顺序）。
	DeliveryStages []string `json:"delivery_stages"`
	// 是否已在役。
	InService bool `json:"in_service"`
	Found     bool `json:"found"`
}

// queryAssetAcceptance 只读查询资产验收状态（不修改领域状态）。
func queryAssetAcceptance(q recordsQuerier, objectRef string) (AssetAcceptanceStatus, error) {
	status := AssetAcceptanceStatus{ObjectRef: objectRef, Coverage: map[string]string{}}
	asset, err := readAsset(q, objectRef)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil // Found=false
	}
	if err != nil {
		return status, err
	}
	status.Found = true
	status.Kind = asset.Kind
	status.Identity = asset.Identity
	status.State = asset.State
	status.InService = asset.State == AssetStateInService
	status.Revision = asset.Revision
	status.Frozen = asset.FrozenAt > 0
	// 验收覆盖。
	rows, err := q.Query(`SELECT requirement_key, coverage_state FROM acceptance_coverage WHERE object_ref=? ORDER BY requirement_key`, objectRef)
	if err != nil {
		return status, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return status, err
		}
		status.Coverage[k] = v
	}
	if err := rows.Err(); err != nil {
		return status, err
	}
	// 交付阶段。
	stageRows, err := q.Query(`SELECT stage FROM delivery_records WHERE object_ref=? ORDER BY occurred_at`, objectRef)
	if err != nil {
		return status, err
	}
	defer stageRows.Close()
	for stageRows.Next() {
		var s string
		if err := stageRows.Scan(&s); err != nil {
			return status, err
		}
		status.DeliveryStages = append(status.DeliveryStages, s)
	}
	return status, stageRows.Err()
}

// ---------------------------------------------------------------------------
// 确定性处理器（只读查询，接入 V3 执行链）
// ---------------------------------------------------------------------------

// assetAcceptanceHandlerKey 是资产验收状态查询的确定性处理器标识。
const assetAcceptanceHandlerKey = "asset-acceptance-status"

// registerAssetAcceptanceHandler 注册资产验收状态查询处理器。
// 处理器需访问 Store 做只读查询；闭包绑定，启动时注册一次。
func registerAssetAcceptanceHandler(s *Store) {
	RegisterDeterministic(assetAcceptanceHandlerKey, func(ctx context.Context, input DeterministicInput) (DeterministicResult, error) {
		objectRef := strings.TrimSpace(input.InputRef)
		if objectRef == "" {
			return DeterministicResult{}, errors.New("资产验收状态查询缺少 ObjectRef")
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return DeterministicResult{}, err
		}
		defer tx.Rollback()
		var principal string
		if err := tx.QueryRow(`SELECT principal FROM domain_query_tasks WHERE task_id=?`, input.TaskID).Scan(&principal); err != nil {
			return DeterministicResult{}, err
		}
		decision, err := decideAuthorization(tx, principal, "read", objectRef, "", now(), false)
		if err != nil {
			return DeterministicResult{}, err
		}
		if decision.Outcome != "allowed" {
			return DeterministicResult{}, ErrAuthorizationDenied
		}
		status, err := queryAssetAcceptance(tx, objectRef)
		if err != nil {
			return DeterministicResult{}, err
		}
		payload, err := json.Marshal(status)
		if err != nil {
			return DeterministicResult{}, err
		}
		summary := fmt.Sprintf("资产 %s 验收状态：%s（覆盖 %d 项，在役 %t）",
			objectRef, status.State, len(status.Coverage), status.InService)
		if !status.Found {
			summary = fmt.Sprintf("资产 %s 不存在", objectRef)
		}
		return DeterministicResult{OutputRef: string(payload), Summary: summary}, nil
	})
}

// ---------------------------------------------------------------------------
// 只读领域任务受理（建立 V3 Task + Step + 就绪 work_unit）
// ---------------------------------------------------------------------------

// AssetQueryTask 是一个资产验收状态查询任务。
type AssetQueryTask struct {
	TaskID    string
	ObjectRef string
	Duplicate bool
}

// SubmitAssetAcceptanceQuery 受理一个资产验收状态只读查询（A2+A3）。
// 建立 V3 Task（pending）+ 一个确定性 Step（ready）+ 就绪 work_unit；
// 幂等：同请求键返回同一任务。只读：不修改任何领域状态。
func (s *Store) SubmitAssetAcceptanceQuery(ctx context.Context, requestKey, objectRef, principal string) (AssetQueryTask, error) {
	if requestKey == "" || principal == "" {
		return AssetQueryTask{}, errors.New("受理缺少请求键或可信主体")
	}
	objectRef = strings.TrimSpace(objectRef)
	if objectRef == "" {
		return AssetQueryTask{}, errors.New("缺少资产 ObjectRef")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AssetQueryTask{}, err
	}
	defer tx.Rollback()
	result, err := submitAssetAcceptanceQuery(tx, requestKey, objectRef, principal, now())
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func submitAssetAcceptanceQuery(tx *sql.Tx, requestKey, objectRef, principal string, nowV int64) (AssetQueryTask, error) {
	// P0-1：对象级读权限判定（设计 §9.1：读取/搜索均须判定，未授权对象不得泄露）。
	// 未授权统一返回"不存在或无权访问"，不区分对象是否真实存在（避免泄露存在性）。
	authDecision, err := decideAuthorization(tx, principal, "read", objectRef, "", nowV, false)
	if err != nil {
		return AssetQueryTask{}, err
	}
	if authDecision.Outcome != "allowed" {
		return AssetQueryTask{}, fmt.Errorf("%w：资产 %s 不存在或无权访问", ErrAuthorizationDenied, objectRef)
	}
	// 幂等：同请求键返回同一任务。
	var existingTaskID, existingObject, existingPrincipal string
	err = tx.QueryRow(`SELECT task_id,object_ref,principal FROM domain_query_tasks WHERE request_key=?`, requestKey).
		Scan(&existingTaskID, &existingObject, &existingPrincipal)
	if err == nil {
		if existingObject != objectRef || existingPrincipal != principal {
			return AssetQueryTask{}, ErrConflict
		}
		return AssetQueryTask{TaskID: existingTaskID, ObjectRef: objectRef, Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AssetQueryTask{}, err
	}
	// 建立旧 tasks/conversations 行（V3 结构经外键引用 tasks.id；旧表保留持久记录）。
	conversationID := id()
	if _, err := tx.Exec(`INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)`,
		conversationID, "资产验收状态查询 "+objectRef, nowV); err != nil {
		return AssetQueryTask{}, err
	}
	taskID := id()
	if _, err := tx.Exec(`INSERT INTO tasks(id,conversation_id,title,goal,scope_label,status,status_reason,reason,updated_at)
VALUES(?,?,?,?,?,'active','queued','已受理',?)`,
		taskID, conversationID, "查询资产验收状态", "查询 "+objectRef+" 的验收状态", "领域查询", nowV); err != nil {
		return AssetQueryTask{}, err
	}
	// 建立 V3 Task（pending）。
	if _, err := transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: principal, Input: "资产验收状态查询 " + objectRef}, nowV); err != nil {
		return AssetQueryTask{}, err
	}
	// 建立确定性 Step（ready），输入为 ObjectRef。
	stepID := id()
	if err := insertStep(tx, Step{
		StepID: stepID, TaskID: taskID, ContractRevision: 1,
		DefinitionKey: assetAcceptanceHandlerKey, DefinitionVersion: 1,
		InputRef: objectRef, State: V3StepReady, Position: 1, CreatedAt: nowV, UpdatedAt: nowV,
	}); err != nil {
		return AssetQueryTask{}, err
	}
	// 建立就绪 work_unit（kind=deterministic）。
	if err := insertWorkUnit(tx, WorkUnit{
		UnitID: id(), TaskID: taskID, StepID: stepID, Kind: WorkUnitDeterministic,
		ReadyAt: nowV, State: WorkUnitReady, InputRef: objectRef, CreatedAt: nowV, UpdatedAt: nowV,
	}); err != nil {
		return AssetQueryTask{}, err
	}
	// 登记请求键（幂等）。
	if _, err := tx.Exec(`INSERT INTO domain_query_tasks(request_key,task_id,object_ref,principal,created_at) VALUES(?,?,?,?,?)`,
		requestKey, taskID, objectRef, principal, nowV); err != nil {
		return AssetQueryTask{}, err
	}
	if err := recordAudit(tx, AuditEvent{TaskID: taskID, StepID: stepID, Event: "domain_query_admitted", Actor: principal, Summary: "受理资产验收状态查询 " + objectRef, OccurredAt: nowV}); err != nil {
		return AssetQueryTask{}, err
	}
	return AssetQueryTask{TaskID: taskID, ObjectRef: objectRef}, nil
}

func (s *Store) SubmitChatAssetAcceptance(ctx context.Context, key, principal, conversationID, text, scope, objectRef string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	stamp := now()
	fingerprint := submissionFingerprint(text, scope, conversationID, false)
	previous, err := previousSubmission(tx, key, fingerprint)
	if err != nil || previous.ID != "" {
		return previous, err
	}
	if conversationID != "" {
		if err := checkConversationSubmission(tx, conversationID); err != nil {
			return Run{}, err
		}
	}
	if _, err := admitChatInputWithFingerprint(tx, key, principal, text, fingerprint, "", stamp); err != nil {
		return Run{}, err
	}
	query, err := submitAssetAcceptanceQuery(tx, key, objectRef, principal, stamp)
	if err != nil {
		return Run{}, err
	}
	if conversationID == "" {
		conversationID = id()
		if _, err := tx.Exec(`INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)`, conversationID, title(text), stamp); err != nil {
			return Run{}, err
		}
	}
	run := Run{ID: id(), ConversationID: conversationID, Status: "completed"}
	if _, err := tx.Exec(`INSERT INTO runs(id,conversation_id,status,created_at,usage_tracked) VALUES(?,?,?,?,1)`, run.ID, conversationID, run.Status, stamp); err != nil {
		return Run{}, err
	}
	message, err := tx.Exec(`INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user',?)`, conversationID, run.ID, text)
	if err != nil {
		return Run{}, err
	}
	seq, err := message.LastInsertId()
	if err != nil {
		return Run{}, err
	}
	if err := recordInputEvent(tx, seq, key); err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec(`INSERT INTO input_dispositions VALUES(?,?,0,'completed',?)`, seq, run.ID, stamp); err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec(`INSERT INTO submissions VALUES(?,?,?)`, key, fingerprint, run.ID); err != nil {
		return Run{}, err
	}
	if err := linkAdmissionToRun(tx, key, run.ID, query.TaskID, stamp); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

// ---------------------------------------------------------------------------
// 进度与结果查询
// ---------------------------------------------------------------------------

// AssetQueryProgress 是资产验收状态查询的进度与结果。
type AssetQueryProgress struct {
	TaskID    string                 `json:"task_id"`
	State     string                 `json:"state"` // V3 任务状态
	StepState string                 `json:"step_state"`
	UnitState string                 `json:"unit_state"`
	Done      bool                   `json:"done"`
	Summary   string                 `json:"summary"`
	Result    *AssetAcceptanceStatus `json:"result"`
}

// ReadAssetAcceptanceQuery 查询一个资产验收状态查询任务的进度与结果（A3）。
// 任务完成必须有持久结果：结果来自 step.result_ref（落盘的验收状态快照）。
// 进度查询同样受对象级权限约束（P0）：principal 为空时按任务属主判定；
// 调用方须传入可信主体，未授权统一返回"不存在或无权访问"。
func (s *Store) ReadAssetAcceptanceQuery(ctx context.Context, taskID, principal string) (AssetQueryProgress, error) {
	var p AssetQueryProgress
	p.TaskID = taskID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	var objectRef, owner string
	if err := tx.QueryRow(`SELECT object_ref,principal FROM domain_query_tasks WHERE task_id=?`, taskID).Scan(&objectRef, &owner); err != nil {
		return p, err
	}
	if principal == "" {
		principal = owner
	}
	decision, err := decideAuthorization(tx, principal, "read", objectRef, "", now(), false)
	if err != nil {
		return p, err
	}
	if decision.Outcome != "allowed" {
		return p, sql.ErrNoRows
	}
	state, serr := readTaskState(tx, taskID)
	if serr != nil {
		return p, serr
	}
	p.State = string(state.State)
	// 读取该任务的 step 与 unit。
	var resultRef sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT state, coalesce(result_ref,'') FROM steps WHERE task_id=? ORDER BY position LIMIT 1`, taskID).
		Scan(&p.StepState, &resultRef)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("任务 %s 不存在或无步骤", taskID)
	}
	if err != nil {
		return p, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT state FROM work_units WHERE task_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1`, taskID).Scan(&p.UnitState); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	p.Done = p.StepState == string(V3StepCompleted) || p.StepState == string(V3StepFailed)
	// 结果落盘：从 result_receipts 读取该任务已应用的验收状态快照。
	if p.Done {
		var summary, evidenceRef string
		err := tx.QueryRowContext(ctx, `SELECT rr.summary,s.result_ref FROM steps s JOIN work_units w ON w.step_id=s.step_id
JOIN result_receipts rr ON rr.unit_id=w.unit_id AND rr.lease_generation=w.lease_generation
WHERE s.task_id=? AND rr.state='applied' ORDER BY rr.applied_at DESC LIMIT 1`, taskID).
			Scan(&summary, &evidenceRef)
		if err != nil {
			return p, err
		}
		p.Summary = summary
		if p.StepState == string(V3StepCompleted) {
			var status AssetAcceptanceStatus
			if err := json.Unmarshal([]byte(evidenceRef), &status); err != nil {
				return p, err
			}
			p.Result = &status
		}
	}
	return p, tx.Commit()
}

// ---------------------------------------------------------------------------
// 启动接入（注册处理器 + 启动本地嵌入执行器）
// ---------------------------------------------------------------------------

// EnableAssetAcceptanceQuery 注册资产验收状态查询处理器并启动本地嵌入执行器（A2+A3）。
// 返回执行器；调用方用 go executor.Run(ctx, interval) 启动消费循环。
// 重启后 claimed/running 单元由 RecoverV3 置回 ready，执行器可重新领取。
func (s *Store) EnableAssetAcceptanceQuery(ctx context.Context) (*EmbeddedExecutor, error) {
	registerAssetAcceptanceHandler(s)
	return s.StartEmbeddedExecutor(ctx)
}

// ---------------------------------------------------------------------------
// 测试种子（登记资产与验收覆盖，供 HTTP/集成测试）
// ---------------------------------------------------------------------------

// SeedAssetForTest 登记一个资产及验收覆盖（仅测试与本地演示用）。
func (s *Store) SeedAssetForTest(objectRef, state string) error {
	nowV := now()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := upsertAsset(tx, OpsAsset{ObjectRef: objectRef, Kind: "node", Identity: objectRef, State: state, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, nowV); err != nil {
		return err
	}
	if _, err := recordAcceptanceCoverage(tx, AcceptanceCoverage{RequirementKey: "delivery-ready", ObjectRef: objectRef, CoverageState: "covered"}, nowV); err != nil {
		return err
	}
	// 测试种子同时授予 user:session 读取权限（A2 只读查询对象级鉴权）。
	if _, err := grantAuthorization(tx, Authorization{Subject: "user:session", Action: "read", ObjectRef: objectRef, ValidFrom: nowV, Owner: "ops"}, nowV); err != nil {
		return err
	}
	return tx.Commit()
}
