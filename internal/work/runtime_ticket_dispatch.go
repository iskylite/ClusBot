package work

// B 步：测试工单系统的受控写路径（E03 首个切片）。
//
// 本文件把"创建工单"这条写路径接通到 V3 Operation 全生命周期：
//   - createTicket → registerOperation（capability=ticket.create）+ Outbox 同事务；
//   - dispatchOperationCommitted 提交后，由真实 HTTP provider 执行 POST 创建工单；
//   - 拿到 external_ticket_id/provider_job_id → normalizeJobStart 登记远端作业；
//   - 回执（回调/轮询）经 receiveOperationReceipt → applyOperationReceipt →
//     回写 Operation 与 ticket 状态（当前缺失的一环）；
//   - pollDueJobs 到期时真实 GET 工单状态，驱动上述回执链；
//   - 工单 closed 不采信：另发独立只读效果查询，喂给 verifyTicketRepair。
//
// 受控边界：最终核权（checkFinalAdmission）、派发条件更新、回执先持久化、
// 重复回调幂等、回执成功≠恢复、未知先核对——全部复用 P3 既有守卫。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 真实 HTTP provider（B1：http.Client + validate + classify，真实网络 I/O）
// ---------------------------------------------------------------------------

// TicketProvider 是工单系统的受控 HTTP 提供者。
type TicketProvider struct {
	BaseURL         string
	CredentialScope string
	Credential      string // 仅用于请求头，不落库
	client          *http.Client
}

// NewTicketProvider 创建工单 provider（校验连接规范）。
func NewTicketProvider(baseURL, credentialScope, credential string) (*TicketProvider, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || credentialScope == "" {
		return nil, errors.New("工单服务地址或凭据作用域无效")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())) {
		return nil, errors.New("工单服务须使用 HTTPS 或本机 HTTP")
	}
	p := &TicketProvider{BaseURL: strings.TrimRight(baseURL, "/"), CredentialScope: credentialScope, Credential: credential}
	p.client = &http.Client{Timeout: 30 * time.Second}
	return p, nil
}

// ticketProviderResult 是 provider 调用的规范化结果。
type ticketProviderResult struct {
	ExternalTicketID string
	ExternalState    string
	StateVersion     int
	Outcome          string // success/pending/failure/unknown
	ErrorKind        string
	Body             string
}

// do 执行一次受控 HTTP 调用（validate + classify，真实网络 I/O）。
func (p *TicketProvider) do(ctx context.Context, spec HTTPRequestSpec) (HTTPResponseResult, error) {
	// 连接校验（HTTPS/TLS/凭据作用域/有界）。
	if err := validateHTTPConnection(spec); err != nil {
		return HTTPResponseResult{}, err
	}
	// 单次显式调用：本方法每次新建请求、不自动重试，重投守卫由上层
	// guardOperationReplay 驱动（已派发/待核对只核对不重发）。HTTP 客户端层面
	// 不做任何隐含重放，故无需在此判定 ensureNoImplicitRetry。
	timeout := time.Duration(spec.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var bodyReader io.Reader
	if spec.Body != "" {
		bodyReader = bytes.NewBufferString(spec.Body)
	}
	req, err := http.NewRequestWithContext(ctx, spec.Method, p.BaseURL+spec.Path, bodyReader)
	if err != nil {
		return HTTPResponseResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Credential != "" {
		// 凭据仅用于请求头，作用域固定（重定向不转发）。
		req.Header.Set("Authorization", "Bearer "+p.Credential)
	}
	// A redirect could resend a write or change its target.
	client := *p.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		// 网络错误转待核对。
		return classifyHTTPResponse(0, "", err), nil
	}
	defer resp.Body.Close()
	// 响应体有界读取。
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(bodyBytes) > 1<<20 {
		return classifyHTTPResponse(0, "", errors.New("工单响应未完整读取")), nil
	}
	return classifyHTTPResponse(resp.StatusCode, string(bodyBytes), nil), nil
}

// CreateTicket 真实调用工单系统创建工单（Start）。
func (p *TicketProvider) CreateTicket(ctx context.Context, objectRef, faultEvidence, idempotencyKey string) (ticketProviderResult, error) {
	payload, _ := json.Marshal(map[string]any{
		"object_ref": objectRef, "fault_evidence": faultEvidence, "idempotency_key": idempotencyKey,
	})
	res, err := p.do(ctx, HTTPRequestSpec{
		Kind: HTTPOpStart, BaseURL: p.BaseURL, Path: "/api/v1/tickets", Method: "POST",
		Body: string(payload), MaxBodyBytes: 64 * 1024, TimeoutMS: 30000, CredentialScope: p.CredentialScope,
	})
	if err != nil {
		return ticketProviderResult{}, err
	}
	out := ticketProviderResult{Outcome: res.BusinessOutcome, ErrorKind: res.ErrorKind, Body: res.Body}
	// 解析外部工单 ID 与状态版本（成功或已接受）。
	if res.StatusCode >= 200 && res.StatusCode < 300 || res.StatusCode == 202 {
		var parsed struct {
			TicketID     string `json:"ticket_id"`
			State        string `json:"state"`
			StateVersion int    `json:"state_version"`
		}
		if json.Unmarshal([]byte(res.Body), &parsed) == nil {
			out.ExternalTicketID = parsed.TicketID
			out.ExternalState = parsed.State
			out.StateVersion = parsed.StateVersion
		}
	}
	return out, nil
}

// GetTicket 真实调用工单系统查询工单状态（Get/Query）。
func (p *TicketProvider) GetTicket(ctx context.Context, externalTicketID string) (ticketProviderResult, error) {
	return p.lookupTicket(ctx, "/api/v1/tickets/"+url.PathEscape(externalTicketID))
}

func (p *TicketProvider) lookupTicket(ctx context.Context, path string) (ticketProviderResult, error) {
	res, err := p.do(ctx, HTTPRequestSpec{
		Kind: HTTPOpGet, BaseURL: p.BaseURL, Path: path, Method: "GET",
		MaxBodyBytes: 64 * 1024, TimeoutMS: 30000, CredentialScope: p.CredentialScope,
	})
	if err != nil {
		return ticketProviderResult{}, err
	}
	out := ticketProviderResult{Outcome: res.BusinessOutcome, ErrorKind: res.ErrorKind, Body: res.Body}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		var parsed struct {
			TicketID       string `json:"ticket_id"`
			State          string `json:"state"`
			StateVersion   int    `json:"state_version"`
			EffectVerified bool   `json:"effect_verified"`
		}
		if json.Unmarshal([]byte(res.Body), &parsed) == nil {
			out.ExternalTicketID = parsed.TicketID
			out.ExternalState = parsed.State
			out.StateVersion = parsed.StateVersion
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 工单 Operation 提交（B2：createTicket → registerOperation + 派发接线）
// ---------------------------------------------------------------------------

// TicketCreateRequest 是创建工单的受控请求。
type TicketCreateRequest struct {
	RequestKey        string // 幂等键
	ObjectRef         string
	FaultEvidence     string
	Subject           string // 可信主体
	IncidentID        string
	IncidentUpdatedAt int64
	AssetIdentity     string
	AssetRevision     int
	OwnershipEpoch    int
}

// TicketCreateResult 是创建工单的结果。
type TicketCreateResult struct {
	OperationID      string `json:"operation_id"`
	TicketID         string `json:"ticket_id,omitempty"`
	ExternalTicketID string `json:"external_ticket_id,omitempty"`
	State            string `json:"state"`
	Duplicate        bool   `json:"duplicate,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Verification     string `json:"verification,omitempty"`
}

func (p *TicketProvider) identity() string { return digest(p.BaseURL + "\n" + p.CredentialScope) }

func ticketParamsDigest(objectRef, evidence string) string {
	payload, _ := json.Marshal([]string{objectRef, evidence})
	return digest(string(payload))
}

type ticketTarget struct {
	Ref      string `json:"ref"`
	Revision int    `json:"revision"`
	Epoch    int    `json:"epoch"`
}

func ticketOperationTarget(op Operation) (ticketTarget, error) {
	var targets []ticketTarget
	if op.CapabilityKey != "ticket.create" || json.Unmarshal([]byte(op.TargetRefs), &targets) != nil ||
		len(targets) != 1 || targets[0].Ref == "" {
		return ticketTarget{}, ErrState
	}
	return targets[0], nil
}

// SubmitTicketCreate 受理一个创建工单请求（B2）。
// createTicket → registerOperation（capability=ticket.create）同事务；
// 幂等：同请求键返回同一 Operation。
func (s *Store) SubmitTicketCreate(ctx context.Context, req TicketCreateRequest) (TicketCreateResult, error) {
	return s.submitTicketCreate(ctx, req, "")
}

func (s *Store) QueueTicketCreate(ctx context.Context, req TicketCreateRequest, provider *TicketProvider) (TicketCreateResult, error) {
	if provider == nil {
		return TicketCreateResult{}, ErrTicketProviderUnavailable
	}
	return s.submitTicketCreate(ctx, req, provider.identity())
}

func (s *Store) submitTicketCreate(ctx context.Context, req TicketCreateRequest, providerID string) (TicketCreateResult, error) {
	if req.RequestKey == "" || req.Subject == "" || req.ObjectRef == "" || strings.TrimSpace(req.FaultEvidence) == "" || len(req.FaultEvidence) > 8000 {
		return TicketCreateResult{}, errors.New("工单请求缺少请求键/主体/对象")
	}
	nowV := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TicketCreateResult{}, err
	}
	defer tx.Rollback()
	// 最终准入核对（授权+领域准入）：维修接管授权 + 资产可写。
	authDecision, err := decideAuthorization(tx, req.Subject, "repair", req.ObjectRef, "", nowV, false)
	if err != nil {
		return TicketCreateResult{}, err
	}
	if authDecision.Outcome != "allowed" {
		return TicketCreateResult{}, fmt.Errorf("%w：主体 %s 无权对 %s 创建工单", ErrAuthorizationDenied, req.Subject, req.ObjectRef)
	}
	if req.IncidentID != "" {
		read, err := decideAuthorization(tx, req.Subject, "read", req.ObjectRef, "", nowV, false)
		if err != nil {
			return TicketCreateResult{}, err
		}
		if read.Outcome != "allowed" {
			return TicketCreateResult{}, ErrAuthorizationDenied
		}
		dispatch, err := decideAuthorization(tx, req.Subject, "dispatch", "ticket.create", "", nowV, false)
		if err != nil {
			return TicketCreateResult{}, err
		}
		if dispatch.Outcome != "allowed" {
			return TicketCreateResult{}, fmt.Errorf("%w：缺少工单派发授权", ErrAuthorizationDenied)
		}
	}
	// 借出期间核对专项授权（P4-18.5）。
	if err := ensureLentAssetRepairAuthorized(tx, req.ObjectRef, req.Subject, nowV); err != nil {
		return TicketCreateResult{}, err
	}
	asset, err := readAsset(tx, req.ObjectRef)
	if err != nil {
		return TicketCreateResult{}, err
	}
	paramsDigest := ticketParamsDigest(req.ObjectRef, req.FaultEvidence)
	var existingID string
	err = tx.QueryRow(`SELECT operation_id FROM operations WHERE idempotency_key=?`, req.RequestKey).Scan(&existingID)
	if err == nil {
		existing, err := readOperation(tx, existingID)
		if err != nil {
			return TicketCreateResult{}, err
		}
		var boundProvider string
		if err := tx.QueryRow(`SELECT provider_id FROM ticket_dispatch_inputs WHERE operation_id=?`, existingID).Scan(&boundProvider); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return TicketCreateResult{}, ErrConflict
			}
			return TicketCreateResult{}, err
		}
		if existing.CapabilityKey != "ticket.create" || existing.AuthorizationRef != req.Subject ||
			existing.ParamsDigest != paramsDigest || boundProvider != providerID {
			return TicketCreateResult{}, ErrConflict
		}
		var boundIncident string
		err = tx.QueryRow(`SELECT incident_id FROM incident_ticket_operations WHERE operation_id=?`, existingID).Scan(&boundIncident)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return TicketCreateResult{}, err
		}
		if boundIncident != req.IncidentID {
			return TicketCreateResult{}, ErrConflict
		}
		return TicketCreateResult{OperationID: existingID, State: string(existing.State), Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TicketCreateResult{}, err
	}
	if asset.State != AssetStateMaintenance && asset.State != AssetStateLent {
		return TicketCreateResult{}, fmt.Errorf("%w：资产须已进入维护接管或借出专项维修", ErrState)
	}
	if asset.FrozenAt > 0 {
		return TicketCreateResult{}, fmt.Errorf("%w：资产已冻结", ErrState)
	}
	if req.IncidentID != "" {
		var updated int64
		var state string
		var recovered int
		if err := tx.QueryRow(`SELECT updated_at,state,recovery_verified FROM incidents WHERE incident_id=?`, req.IncidentID).Scan(&updated, &state, &recovered); err != nil {
			return TicketCreateResult{}, err
		}
		if updated != req.IncidentUpdatedAt || asset.Identity != req.AssetIdentity || asset.Revision != req.AssetRevision || asset.OwnershipEpoch != req.OwnershipEpoch {
			return TicketCreateResult{}, fmt.Errorf("%w：事件或资产状态已变化，请重新预览", ErrConflict)
		}
		if state == "resolved" || state == "closed" || recovered != 0 {
			return TicketCreateResult{}, fmt.Errorf("%w：事件已恢复或关闭", ErrState)
		}
		var exists int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tickets WHERE incident_id=? AND object_ref=?) OR EXISTS(
SELECT 1 FROM incident_ticket_operations i JOIN operations o ON o.operation_id=i.operation_id
WHERE i.incident_id=? AND i.object_ref=? AND o.state NOT IN ('failed','cancelled'))`, req.IncidentID, req.ObjectRef, req.IncidentID, req.ObjectRef).Scan(&exists); err != nil {
			return TicketCreateResult{}, err
		}
		if exists != 0 {
			return TicketCreateResult{}, fmt.Errorf("%w：事件已有该资产的工单或待核对请求", ErrConflict)
		}
	}
	taskID, err := s.ensureMaintenanceTask(tx, req, nowV)
	if err != nil {
		return TicketCreateResult{}, err
	}
	targets, _ := json.Marshal([]ticketTarget{{Ref: req.ObjectRef, Revision: asset.Revision, Epoch: asset.OwnershipEpoch}})
	op, err := registerOperation(tx, Operation{
		TaskID: taskID, CapabilityKey: "ticket.create", CapabilityVersion: 1,
		TargetRefs: string(targets), ParamsDigest: paramsDigest, AuthorizationRef: req.Subject, IdempotencyKey: req.RequestKey,
	}, nowV)
	if err != nil {
		return TicketCreateResult{}, err
	}
	if _, err := tx.Exec(`INSERT INTO ticket_dispatch_inputs(operation_id,evidence,asset_revision,ownership_epoch,provider_id)
VALUES(?,?,?,?,?)`, op.OperationID, req.FaultEvidence, asset.Revision, asset.OwnershipEpoch, providerID); err != nil {
		return TicketCreateResult{}, err
	}
	if req.IncidentID != "" {
		if _, err := tx.Exec(`INSERT INTO incident_ticket_operations(operation_id,incident_id,object_ref,subject,created_at) VALUES(?,?,?,?,?)`, op.OperationID, req.IncidentID, req.ObjectRef, req.Subject, nowV); err != nil {
			return TicketCreateResult{}, err
		}
	}
	if err := recordAudit(tx, AuditEvent{
		TaskID: op.TaskID, Event: "ticket_create_submitted", Actor: req.Subject,
		Summary: fmt.Sprintf("受理创建工单请求（对象 %s，Operation %s）", req.ObjectRef, op.OperationID), OccurredAt: nowV,
	}); err != nil {
		return TicketCreateResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return TicketCreateResult{}, err
	}
	return TicketCreateResult{OperationID: op.OperationID, State: string(op.State)}, nil
}

// ---------------------------------------------------------------------------
// 派发执行（B3：提交后真实调用 + 回执回写 Operation/ticket 状态）
// ---------------------------------------------------------------------------

// DispatchTicketCreate 派发一个工单创建 Operation（B3）。
// 提交后执行：dispatchOperationCommitted 条件更新 → 真实 HTTP POST →
// 拿到 external_ticket_id → normalizeJobStart + createTicket（本地工单记录）。
// 重复派发幂等（同 Operation 已派发则跳过真实调用）。
func (s *Store) DispatchTicketCreate(ctx context.Context, operationID, subject, faultEvidence string, provider *TicketProvider) (TicketCreateResult, error) {
	if provider == nil {
		return TicketCreateResult{}, errors.New("缺少工单 provider")
	}
	nowV := now()
	// 1. 提交派发（条件更新已登记→已派发，最终准入核对）。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TicketCreateResult{}, err
	}
	defer tx.Rollback()
	op, err := readOperation(tx, operationID)
	if err != nil {
		return TicketCreateResult{}, err
	}
	if op.State != V3OperationRegistered {
		return TicketCreateResult{OperationID: operationID, State: string(op.State), Duplicate: true}, nil
	}
	target, err := ticketOperationTarget(op)
	if err != nil {
		return TicketCreateResult{}, err
	}
	var evidence, providerID string
	if err := tx.QueryRow(`SELECT evidence,provider_id FROM ticket_dispatch_inputs WHERE operation_id=?`, operationID).Scan(&evidence, &providerID); err != nil {
		return TicketCreateResult{}, err
	}
	if subject != op.AuthorizationRef || (providerID != "" && providerID != provider.identity()) ||
		(faultEvidence != "" && faultEvidence != evidence) || ticketParamsDigest(target.Ref, evidence) != op.ParamsDigest {
		return TicketCreateResult{}, ErrConflict
	}
	repair, err := decideAuthorization(tx, subject, "repair", target.Ref, "", nowV, false)
	if err != nil {
		return TicketCreateResult{}, err
	}
	asset, err := readAsset(tx, target.Ref)
	if err != nil {
		return TicketCreateResult{}, err
	}
	if repair.Outcome != "allowed" || asset.Revision != target.Revision || asset.OwnershipEpoch != target.Epoch ||
		(asset.State != AssetStateMaintenance && asset.State != AssetStateLent) {
		return s.cancelUnsentTicket(tx, op, "维修授权或资产版本已变更", nowV)
	}
	var incidentID string
	err = tx.QueryRow(`SELECT incident_id FROM incident_ticket_operations WHERE operation_id=?`, operationID).Scan(&incidentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TicketCreateResult{}, err
	}
	if incidentID != "" {
		read, err := decideAuthorization(tx, subject, "read", target.Ref, "", nowV, false)
		if err != nil {
			return TicketCreateResult{}, err
		}
		if read.Outcome != "allowed" {
			return s.cancelUnsentTicket(tx, op, "资产读取授权已撤销", nowV)
		}
		var state string
		var recovered int
		if err := tx.QueryRow(`SELECT state,recovery_verified FROM incidents WHERE incident_id=?`, incidentID).Scan(&state, &recovered); err != nil {
			return TicketCreateResult{}, err
		}
		if state == "resolved" || state == "closed" || recovered != 0 {
			return s.cancelUnsentTicket(tx, op, "事件已恢复或关闭", nowV)
		}
	}
	dispatch, err := dispatchOperationCommitted(tx, operationID, subject, nowV)
	if err != nil {
		return TicketCreateResult{}, err
	}
	if !dispatch.Dispatched {
		return s.cancelUnsentTicket(tx, op, dispatch.Reason, nowV)
	}
	objectRef := target.Ref
	// 2. 事务提交后才执行真实 HTTP 调用（提交后执行）。
	if err := tx.Commit(); err != nil {
		return TicketCreateResult{}, err
	}
	// 3. 真实 HTTP 调用工单系统（网络 I/O 不占写事务）。
	createRes, callErr := provider.CreateTicket(ctx, objectRef, evidence, op.IdempotencyKey)
	// 4. 回写结果（Operation 状态 + 本地工单记录 + 远端 Job）。
	settleCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer stop()
	return s.settleTicketCreate(settleCtx, op, subject, evidence, createRes, callErr)
}

func (s *Store) cancelUnsentTicket(tx *sql.Tx, op Operation, reason string, stamp int64) (TicketCreateResult, error) {
	if _, err := transitionOperation(tx, op.OperationID, V3OperationCancelled, stamp); err != nil {
		return TicketCreateResult{}, err
	}
	if _, err := tx.Exec(`UPDATE operations SET failure_reason=? WHERE operation_id=?`, reason, op.OperationID); err != nil {
		return TicketCreateResult{}, err
	}
	return TicketCreateResult{OperationID: op.OperationID, State: string(V3OperationCancelled), Reason: reason}, tx.Commit()
}

// settleTicketCreate 回写工单创建结果（B3）。
// Operation → running（拿到远端 Job）；拿到 external_ticket_id → createTicket。
// 网络错误转待核对（reconcile），不断言未发送。
func (s *Store) settleTicketCreate(ctx context.Context, op Operation, subject, faultEvidence string, res ticketProviderResult, callErr error) (TicketCreateResult, error) {
	nowV := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TicketCreateResult{}, err
	}
	defer tx.Rollback()
	result := TicketCreateResult{OperationID: op.OperationID}
	current, err := readOperation(tx, op.OperationID)
	if err != nil {
		return result, err
	}
	if current.State != V3OperationDispatched && current.State != V3OperationReconcile {
		result.State = string(current.State)
		return result, nil
	}
	if callErr != nil || (res.Outcome != "success" && res.Outcome != "pending") || res.ExternalTicketID == "" ||
		res.StateVersion < 1 || res.ExternalState == "" || strings.ContainsAny(res.ExternalTicketID, "/\\?#") ||
		res.ExternalTicketID == "." || res.ExternalTicketID == ".." {
		// 网络错误/未知：转待核对（不断言未发送）。
		if current.State == V3OperationDispatched {
			if _, terr := transitionOperation(tx, op.OperationID, V3OperationReconcile, nowV); terr != nil {
				return result, terr
			}
		}
		const reason = "等待按请求键核对外部工单。无法查证时请人工核对。"
		if _, err := tx.Exec(`UPDATE operations SET failure_reason=? WHERE operation_id=?`, reason, op.OperationID); err != nil {
			return result, err
		}
		result.Reason = reason
		result.State = string(V3OperationReconcile)
		errMsg := "网络错误"
		if callErr != nil {
			errMsg = callErr.Error()
		}
		if err := recordAudit(tx, AuditEvent{
			TaskID: op.TaskID, Event: "ticket_create_unknown", Actor: subject,
			Summary: fmt.Sprintf("工单创建结果未知（%s），进入待核对", errMsg), OccurredAt: nowV,
		}); err != nil {
			return result, err
		}
		return result, tx.Commit()
	}
	if current.State == V3OperationDispatched {
		if _, err := transitionOperation(tx, op.OperationID, V3OperationRunning, nowV); err != nil {
			return result, err
		}
	}
	if _, err := tx.Exec(`UPDATE operations SET failure_reason='' WHERE operation_id=?`, op.OperationID); err != nil {
		return result, err
	}
	if _, err := tx.Exec(`UPDATE ticket_dispatch_inputs SET next_check_at=0 WHERE operation_id=?`, op.OperationID); err != nil {
		return result, err
	}
	if _, err := normalizeJobStart(tx, op.OperationID, res.ExternalTicketID, "ticket:"+res.ExternalTicketID, "http-retention", nowV); err != nil {
		return result, err
	}
	// 本地工单记录（幂等：external_system+external_ticket_id 唯一）。
	target, err := ticketOperationTarget(op)
	if err != nil {
		return result, err
	}
	var incidentID string
	err = tx.QueryRow(`SELECT incident_id FROM incident_ticket_operations WHERE operation_id=?`, op.OperationID).Scan(&incidentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	ticket, err := createTicket(tx, Ticket{
		ExternalSystem: "ticket-system", ExternalTicketID: res.ExternalTicketID,
		ObjectRef: target.Ref, FaultEvidence: faultEvidence, IncidentID: incidentID,
		ExternalStateVersion: res.StateVersion, ExternalState: res.ExternalState,
	}, nowV)
	if err != nil {
		return result, err
	}
	if ticket.ObjectRef != target.Ref || ticket.IncidentID != incidentID {
		return result, ErrConflict
	}
	result.TicketID = ticket.TicketID
	result.ExternalTicketID = res.ExternalTicketID
	result.State = string(V3OperationRunning)
	if current.State == V3OperationReconcile {
		result.State = string(V3OperationReconcile)
	}
	if err := recordAudit(tx, AuditEvent{
		TaskID: op.TaskID, Event: "ticket_created", Actor: subject,
		Summary: fmt.Sprintf("工单已创建（外部 %s，本地 %s）", res.ExternalTicketID, ticket.TicketID), OccurredAt: nowV,
	}); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// ---------------------------------------------------------------------------
// 轮询闭环（B4：pollDueJobs 到期真实 Get + 回执回写）
// ---------------------------------------------------------------------------

// PollTicketJobs 轮询到期的工单远端 Job，真实 GET 状态并驱动回执回写（B4）。
// 回执经 receiveOperationReceipt → applyOperationReceipt → 回写 ticket/Operation；
// 重复回调幂等；查询失败/过期不被当作未执行证明。
func (s *Store) PollTicketJobs(ctx context.Context, subject string, provider *TicketProvider, limit int) (int, error) {
	if provider == nil {
		return 0, errors.New("缺少工单 provider")
	}
	nowV := now()
	// 1. 读取到期 running Job（复用 pollDueJobs 选取逻辑）。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(`SELECT j.job_id,j.operation_id,j.provider_job_id,j.query_generation
FROM operation_jobs j JOIN operations o ON o.operation_id=j.operation_id
JOIN ticket_dispatch_inputs i ON i.operation_id=o.operation_id
WHERE j.state='running' AND j.next_query_at<=? AND o.capability_key='ticket.create'
AND i.provider_id IN ('',?) ORDER BY j.next_query_at LIMIT ?`, nowV, provider.identity(), limit)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	type dueJob struct {
		jobID, operationID, providerJobID string
		queryGen                          int
	}
	var jobs []dueJob
	for rows.Next() {
		var j dueJob
		if err := rows.Scan(&j.jobID, &j.operationID, &j.providerJobID, &j.queryGen); err != nil {
			rows.Close()
			tx.Rollback()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	// 先推进退避（避免并发重复查询）。
	for _, j := range jobs {
		nextQuery := nowV + jobBackoff(j.queryGen)
		if _, err := tx.Exec(`UPDATE operation_jobs SET next_query_at=?, query_generation=query_generation+1, updated_at=?
WHERE job_id=? AND state='running'`, nextQuery, nowV, j.jobID); err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// 2. 逐个真实 GET（网络 I/O 不占写事务）。
	polled := 0
	for _, j := range jobs {
		res, err := provider.GetTicket(ctx, j.providerJobID)
		// 回执回写（每个 Job 独立事务）。
		if serr := s.settleTicketPoll(ctx, j.operationID, j.providerJobID, j.queryGen+1, subject, res, err); serr != nil {
			return polled, serr
		}
		polled++
	}
	return polled, nil
}

// settleTicketPoll 回写一次轮询结果（B4）。
// 回执先入 operation_receipts（先持久化），再应用：更新 ticket 外部状态/版本，
// 工单 closed → Operation → succeeded（但 ticket 仍需独立效果验证）。
func (s *Store) settleTicketPoll(ctx context.Context, operationID, externalTicketID string, queryGen int, subject string, res ticketProviderResult, callErr error) error {
	nowV := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 网络错误/未知：不推进（下次轮询重试），不当作未执行证明。
	if callErr != nil || res.Outcome != "success" || res.ExternalTicketID != externalTicketID || res.StateVersion < 1 || res.ExternalState == "" {
		if err := recordAudit(tx, AuditEvent{
			TaskID: operationID, Event: "ticket_poll_unknown", Actor: subject,
			Summary: "工单状态查询未知（下次轮询重试）", OccurredAt: nowV,
		}); err != nil {
			return err
		}
		return tx.Commit()
	}
	var currentGeneration, currentVersion int
	var jobState, ticketState string
	if err := tx.QueryRow(`SELECT j.query_generation,j.state,t.external_state_version,t.state
FROM operation_jobs j JOIN tickets t ON t.external_system='ticket-system' AND t.external_ticket_id=j.provider_job_id
WHERE j.operation_id=? AND j.provider_job_id=?`, operationID, externalTicketID).Scan(&currentGeneration, &jobState, &currentVersion, &ticketState); err != nil {
		return err
	}
	if jobState != "running" || queryGen != currentGeneration || res.StateVersion < currentVersion ||
		(res.StateVersion == currentVersion && !(ticketState == "open" && (res.ExternalState == "closed" || res.ExternalState == "resolved"))) {
		return nil
	}
	op, err := readOperation(tx, operationID)
	if err != nil {
		return err
	}
	// 回执先持久化（去重：query_generation 维度）。
	receiptDigest := digest(fmt.Sprintf("poll %s %s %d", externalTicketID, res.ExternalState, res.StateVersion))
	recv, err := receiveOperationReceipt(tx, OperationReceipt{
		OperationID: operationID, AttemptID: op.CurrentAttemptID, TargetRef: externalTicketID,
		ExternalVersion: res.StateVersion, QueryGeneration: queryGen,
		PayloadDigest: receiptDigest, Receipt: res.Body, Source: "poll",
	}, nowV)
	if err != nil {
		return err
	}
	if recv.Quarantined {
		return tx.Commit() // 未知关联隔离
	}
	// 应用回执（重复/旧代际不重复推进）。
	apply, err := applyOperationReceipt(tx, recv.Event.EventID, currentGeneration, nowV)
	if err != nil {
		return err
	}
	if !apply.Applied {
		return tx.Commit() // 重复回调幂等
	}
	// 更新本地工单外部状态/版本。
	if _, err := tx.Exec(`UPDATE tickets SET external_state=?, external_state_version=?, state=CASE WHEN ? IN ('closed','resolved') THEN 'closed' ELSE state END, updated_at=?
WHERE external_system='ticket-system' AND external_ticket_id=?`,
		res.ExternalState, res.StateVersion, res.ExternalState, nowV, externalTicketID); err != nil {
		return err
	}
	// 工单 closed → Operation → succeeded（回执成功≠恢复，ticket 仍需独立效果验证）。
	if res.ExternalState == "closed" || res.ExternalState == "resolved" {
		if _, err := transitionOperation(tx, operationID, V3OperationSucceeded, nowV); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE operation_jobs SET state='succeeded', updated_at=? WHERE operation_id=? AND provider_job_id=?`,
			nowV, operationID, externalTicketID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// 独立效果验证（B5：区别于工单自报，独立只读效果查询）
// ---------------------------------------------------------------------------

// EffectChecker 是独立效果验证器（区别于工单系统自报）。
// 工单 closed 不采信：独立查询设备实际健康/故障是否恢复。
type EffectChecker func(ctx context.Context, objectRef string) (verified bool, evidence string, err error)

// VerifyTicketClosed 工单关闭后做独立效果验证（B5）。
// 工单回执不替代设备验证：独立查询实际效果并测试，验证后回待复核；
// 未恢复继续限制和后续责任（关闭工单但故障仍在时，资产不会被误恢复）。
func (s *Store) VerifyTicketClosed(ctx context.Context, ticketID, subject string, checker EffectChecker) error {
	if checker == nil {
		return errors.New("缺少独立效果验证器")
	}
	nowV := now()
	// 读取工单（拿对象）。
	ticket, err := readTicket(s.db, ticketID)
	if err != nil {
		return err
	}
	if ticket.State != "closed" {
		return fmt.Errorf("%w：工单尚未关闭", ErrState)
	}
	if ticket.IncidentID != "" {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = incidentAssetPermission(tx, subject, ticket.ObjectRef, nowV)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	expected, err := readAsset(s.db, ticket.ObjectRef)
	if err != nil {
		return err
	}
	// 独立效果查询（不采信工单系统回执）。
	verified, evidence, err := checker(ctx, ticket.ObjectRef)
	if err != nil {
		return err
	}
	if strings.TrimSpace(evidence) == "" || len(evidence) > 8000 {
		return errors.New("独立健康服务未返回可用验证证据")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := readTicket(tx, ticketID)
	if err != nil {
		return err
	}
	if current.State != "closed" || current.ExternalStateVersion != ticket.ExternalStateVersion {
		return ErrConflict
	}
	if ticket.IncidentID != "" {
		if err := incidentAssetPermission(tx, subject, ticket.ObjectRef, now()); err != nil {
			return err
		}
	}
	asset, err := readAsset(tx, ticket.ObjectRef)
	if err != nil {
		return err
	}
	if asset.Identity != expected.Identity || asset.Revision != expected.Revision || asset.OwnershipEpoch != expected.OwnershipEpoch {
		return fmt.Errorf("%w：维修验证期间资产版本已变化", ErrConflict)
	}
	// 验证（效果验证 + 测试通过）。
	if err := verifyTicketRepair(tx, ticketID, verified, verified, nowV); err != nil {
		// 未恢复：继续限制和后续责任（事务仍提交，记录 failed 状态）。
		if aerr := recordAudit(tx, AuditEvent{
			TaskID: ticket.ObjectRef, Event: "ticket_repair_not_verified", Actor: subject,
			Summary: fmt.Sprintf("工单 %s 效果未验证（%s），资产不误恢复", ticketID, evidence), OccurredAt: nowV,
		}); aerr != nil {
			return aerr
		}
		return errors.Join(err, tx.Commit())
	}
	if asset.State == AssetStateMaintenance && asset.FrozenAt == 0 {
		if _, err := transitionAssetState(tx, AssetTransition{
			ObjectRef: asset.ObjectRef, FromState: asset.State, ToState: AssetStateRecheck,
			TriggerKind: "ticket_repair_verified", EvidenceRefs: digest(evidence), RuleRefs: "ticket-repair",
			ExpectedRevision: asset.Revision, ExpectedOwnershipEpoch: asset.OwnershipEpoch,
			Actor: subject, Responsibility: "维修验证通过，等待资产复核",
		}, nowV); err != nil && !errors.Is(err, ErrAuthorizationDenied) {
			return err
		}
	}
	postAsset, err := readAsset(tx, ticket.ObjectRef)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO ticket_health_verifications(ticket_id,object_ref,asset_identity,checked_asset_revision,asset_revision,ownership_epoch,external_state_version,evidence,verified_at)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(ticket_id) DO UPDATE SET object_ref=excluded.object_ref,asset_identity=excluded.asset_identity,
checked_asset_revision=excluded.checked_asset_revision,asset_revision=excluded.asset_revision,ownership_epoch=excluded.ownership_epoch,
external_state_version=excluded.external_state_version,evidence=excluded.evidence,verified_at=excluded.verified_at`,
		ticketID, ticket.ObjectRef, postAsset.Identity, expected.Revision, postAsset.Revision, postAsset.OwnershipEpoch, ticket.ExternalStateVersion, evidence, nowV); err != nil {
		return err
	}
	if err := recordAudit(tx, AuditEvent{
		TaskID: ticket.ObjectRef, Event: "ticket_repair_verified", Actor: subject,
		Summary: fmt.Sprintf("工单 %s 独立效果验证通过（%s）", ticketID, evidence), OccurredAt: nowV,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ensureMaintenanceTask 确保对象有一个进行中的维护接管 V3 Task（幂等：同请求键复用）。
// 工单挂到真实任务下，供最终准入的 Task 控制核对（任务未取消/暂停）。
func (s *Store) ensureMaintenanceTask(tx *sql.Tx, req TicketCreateRequest, nowV int64) (string, error) {
	// 幂等：同请求键复用既有任务。
	var existing string
	err := tx.QueryRow(`SELECT task_id FROM maintenance_tasks WHERE request_key=?`, req.RequestKey).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	// 旧 tasks/conversations 父行（V3 外键引用）。
	conversationID := id()
	if _, err := tx.Exec(`INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)`,
		conversationID, "维护接管 "+req.ObjectRef, nowV); err != nil {
		return "", err
	}
	taskID := id()
	if _, err := tx.Exec(`INSERT INTO tasks(id,conversation_id,title,goal,scope_label,status,status_reason,reason,updated_at)
VALUES(?,?,?,?,?,'active','running','维护接管',?)`,
		taskID, conversationID, "维护接管", "维修 "+req.ObjectRef+"："+req.FaultEvidence, "维修", nowV); err != nil {
		return "", err
	}
	// V3 状态：创建并置 running（维修接管进行中）。
	if _, err := transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: req.Subject, Input: "维护接管 " + req.ObjectRef}, nowV); err != nil {
		return "", err
	}
	st, _ := readTaskState(tx, taskID)
	if _, err := transitionTaskState(tx, taskID, EventClaim, TaskGuard{ExpectedStorageRevision: st.StorageRevision, Actor: req.Subject}, nowV); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO maintenance_tasks(request_key,task_id,object_ref,subject,created_at) VALUES(?,?,?,?,?)`,
		req.RequestKey, taskID, req.ObjectRef, req.Subject, nowV); err != nil {
		return "", err
	}
	return taskID, nil
}

// ErrTicketProviderUnavailable indicates that ticket dispatch has no configured connection.
var ErrTicketProviderUnavailable = errors.New("尚未配置工单提供者")

func (s *Store) ReadTicketOperation(ctx context.Context, operationID, subject string) (TicketCreateResult, error) {
	var out TicketCreateResult
	err := s.db.QueryRowContext(ctx, `SELECT o.operation_id,o.state,o.failure_reason,
coalesce(t.ticket_id,''),coalesce(t.external_ticket_id,''),coalesce(t.verification_outcome,'')
FROM operations o
LEFT JOIN operation_jobs j ON j.operation_id=o.operation_id
LEFT JOIN tickets t ON t.external_system='ticket-system' AND t.external_ticket_id=j.provider_job_id
WHERE o.operation_id=? AND o.capability_key='ticket.create' AND o.authorization_ref=?`, operationID, subject).
		Scan(&out.OperationID, &out.State, &out.Reason, &out.TicketID, &out.ExternalTicketID, &out.Verification)
	return out, err
}

// NewTicketEffectChecker queries a source independent of the ticket system.
func NewTicketEffectChecker(ticket *TicketProvider, baseURL, credential string) (EffectChecker, error) {
	source, err := NewTicketProvider(baseURL, "asset-health", credential)
	if err != nil {
		return nil, err
	}
	ticketURL, _ := url.Parse(ticket.BaseURL)
	sourceURL, _ := url.Parse(source.BaseURL)
	if strings.EqualFold(ticketURL.Host, sourceURL.Host) {
		return nil, errors.New("维修效果验证须配置独立的设备健康服务")
	}
	return func(ctx context.Context, objectRef string) (bool, string, error) {
		res, err := source.do(ctx, HTTPRequestSpec{Kind: HTTPOpGet, BaseURL: source.BaseURL,
			Path:   "/api/v1/asset-health?" + url.Values{"object_ref": {objectRef}}.Encode(),
			Method: "GET", TimeoutMS: 10000, CredentialScope: source.CredentialScope})
		if err != nil {
			return false, "", err
		}
		var observed struct {
			ObjectRef  string `json:"object_ref"`
			Healthy    *bool  `json:"healthy"`
			TestPassed *bool  `json:"test_passed"`
			Evidence   string `json:"evidence"`
		}
		if res.BusinessOutcome != "success" || json.Unmarshal([]byte(res.Body), &observed) != nil ||
			observed.ObjectRef != objectRef || observed.Healthy == nil || observed.TestPassed == nil || observed.Evidence == "" {
			return false, "", errors.New("独立健康服务未返回可用验证证据")
		}
		return *observed.Healthy && *observed.TestPassed, observed.Evidence, nil
	}, nil
}

func (s *Store) RunTicketOperations(ctx context.Context, provider *TicketProvider, checker EffectChecker) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.advanceTicketOperations(ctx, provider, checker); err != nil && ctx.Err() == nil {
				slog.Error("推进工单失败", "error", err)
			}
		}
	}
}

func (s *Store) advanceTicketOperations(ctx context.Context, provider *TicketProvider, checker EffectChecker) error {
	rows, err := s.db.QueryContext(ctx, `SELECT o.operation_id,o.authorization_ref,o.idempotency_key,o.state,i.evidence
FROM operations o JOIN ticket_dispatch_inputs i ON i.operation_id=o.operation_id
WHERE o.capability_key='ticket.create' AND i.provider_id=? AND i.next_check_at<=?
AND (o.state='registered' OR (
  o.state IN ('dispatched','reconcile') AND NOT EXISTS(SELECT 1 FROM operation_jobs j WHERE j.operation_id=o.operation_id)
))
ORDER BY o.updated_at LIMIT 8`, provider.identity(), now())
	if err != nil {
		return err
	}
	type entry struct{ id, subject, key, state, evidence string }
	var pending []entry
	for rows.Next() {
		var item entry
		if err := rows.Scan(&item.id, &item.subject, &item.key, &item.state, &item.evidence); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures error
	for _, item := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if item.state == string(V3OperationRegistered) {
			if _, err := s.DispatchTicketCreate(ctx, item.id, item.subject, "", provider); err != nil {
				_, deferErr := s.db.ExecContext(ctx, `UPDATE ticket_dispatch_inputs SET next_check_at=? WHERE operation_id=?`, now()+15000, item.id)
				failures = errors.Join(failures, err, deferErr)
			}
			continue
		}
		// Check by stable request key. Unknown or missing lookup never triggers a new POST.
		if _, err := s.db.ExecContext(ctx, `UPDATE ticket_dispatch_inputs SET next_check_at=? WHERE operation_id=?`, now()+15000, item.id); err != nil {
			return err
		}
		op, err := readOperation(s.db, item.id)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		result, lookupErr := provider.lookupTicket(ctx, "/api/v1/tickets/by-request?"+url.Values{"idempotency_key": {item.key}}.Encode())
		if _, err := s.settleTicketCreate(ctx, op, item.subject, item.evidence, result, lookupErr); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	_, err = s.PollTicketJobs(ctx, "system:ticket-poller", provider, 8)
	failures = errors.Join(failures, err)
	if checker != nil {
		failures = errors.Join(failures, s.verifyClosedTicketJobs(ctx, provider, checker))
	}
	return failures
}

func (s *Store) verifyClosedTicketJobs(ctx context.Context, provider *TicketProvider, checker EffectChecker) error {
	rows, err := s.db.QueryContext(ctx, `SELECT t.ticket_id,o.authorization_ref,o.operation_id
FROM tickets t JOIN operation_jobs j ON t.external_ticket_id=j.provider_job_id
JOIN operations o ON o.operation_id=j.operation_id JOIN ticket_dispatch_inputs i ON i.operation_id=o.operation_id
WHERE t.external_system='ticket-system' AND t.state='closed' AND i.provider_id=? AND i.next_check_at<=?
ORDER BY t.updated_at LIMIT 8`, provider.identity(), now())
	if err != nil {
		return err
	}
	type entry struct{ ticket, subject, operation string }
	var pending []entry
	for rows.Next() {
		var item entry
		if err := rows.Scan(&item.ticket, &item.subject, &item.operation); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures error
	for _, item := range pending {
		if _, err := s.db.ExecContext(ctx, `UPDATE ticket_dispatch_inputs SET next_check_at=? WHERE operation_id=?`, now()+15000, item.operation); err != nil {
			return err
		}
		failures = errors.Join(failures, s.VerifyTicketClosed(ctx, item.ticket, item.subject, checker))
	}
	return failures
}
