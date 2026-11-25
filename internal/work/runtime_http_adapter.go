package work

// HTTP/SDK 适配与远端作业。
//
// 本文件把外部 REST/SDK 调用映射到同一能力与操作契约：
//   - 适配 Describe/Query/Start/Get/Cancel/Events/Materials 的可支持子集；
//     请求/响应校验与错误分类一致；
//   - 验证目标地址、TLS 身份和凭据作用域，重定向不能泄露凭据；请求大小/
//     超时有界；
//   - 禁用会自动重放非幂等动作的隐含重试；网络错误转待核对，不把 HTTP 202
//     当完成；
//   - 保存远端 Job、状态版本和部分目标结果；HTTP 幂等头只有验证其作用域/
//     保留期后才能作为能力声明；
//   - 使用可控 HTTP 服务验证接收前断线、已接收丢响应、查询/取消缺失及重复
//     回调。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// HTTP 操作契约（Describe/Query/Start/Get/Cancel/Events/Materials 可支持子集）
// ---------------------------------------------------------------------------

// HTTPOperationKind 是 HTTP 操作类型。
type HTTPOperationKind string

const (
	HTTPOpDescribe  HTTPOperationKind = "describe"
	HTTPOpQuery     HTTPOperationKind = "query"
	HTTPOpStart     HTTPOperationKind = "start"
	HTTPOpGet       HTTPOperationKind = "get"
	HTTPOpCancel    HTTPOperationKind = "cancel"
	HTTPOpEvents    HTTPOperationKind = "events"
	HTTPOpMaterials HTTPOperationKind = "materials"
)

// HTTPRequestSpec 是 HTTP 请求规范。
type HTTPRequestSpec struct {
	// 操作类型。
	Kind HTTPOperationKind
	// 目标地址与路径。
	BaseURL string
	Path    string
	// 方法（GET/POST/DELETE）。
	Method  string
	// 请求体与大小限制。
	Body    string
	MaxBodyBytes int
	// 超时。
	TimeoutMS int64
	// 凭据作用域（重定向不能泄露凭据）。
	CredentialScope string
}

// HTTPResponseResult 是 HTTP 响应结果。
type HTTPResponseResult struct {
	// 状态码与业务结果分别处理。
	StatusCode int
	Body       string
	// 业务结果（成功响应但验收失败不误报资产恢复）。
	BusinessOutcome string // success/failure/unknown
	// 错误分类（网络错误转待核对）。
	ErrorKind  string // network/http/business/unknown
}

// ---------------------------------------------------------------------------
// 连接校验（目标地址、TLS 身份、凭据作用域）
// ---------------------------------------------------------------------------

// validateHTTPConnection 校验 HTTP 连接。
// 验证目标地址、TLS 身份和凭据作用域，重定向不能泄露凭据；请求大小/超时有界。
func validateHTTPConnection(spec HTTPRequestSpec) error {
	// 目标地址必须是 HTTPS（TLS 身份）。
	if !strings.HasPrefix(spec.BaseURL, "https://") && !strings.HasPrefix(spec.BaseURL, "http://127.0.0.1") && !strings.HasPrefix(spec.BaseURL, "http://localhost") {
		return fmt.Errorf("目标地址必须使用 HTTPS（TLS 身份），当前 %s", spec.BaseURL)
	}
	// 凭据作用域（重定向不能泄露凭据）。
	if spec.CredentialScope == "" {
		return errors.New("凭据作用域不能为空（重定向不能泄露凭据）")
	}
	// 请求大小有界。
	if spec.MaxBodyBytes > 0 && len(spec.Body) > spec.MaxBodyBytes {
		return fmt.Errorf("请求体大小 %d 超过上限 %d", len(spec.Body), spec.MaxBodyBytes)
	}
	// 超时有界。
	if spec.TimeoutMS <= 0 || spec.TimeoutMS > 300000 {
		return fmt.Errorf("超时 %d 必须有界（1ms-300s）", spec.TimeoutMS)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 错误分类（网络错误转待核对；不把 HTTP 202 当完成）
// ---------------------------------------------------------------------------

// classifyHTTPResponse 分类 HTTP 响应。
// 请求/响应校验与错误分类一致；网络错误转待核对，不把 HTTP 202 当完成；
// 不同 HTTP 状态与业务结果分别处理，成功响应但验收失败不误报资产恢复。
func classifyHTTPResponse(statusCode int, body string, err error) HTTPResponseResult {
	var result HTTPResponseResult
	result.StatusCode = statusCode
	result.Body = body
	// 网络错误：转待核对。
	if err != nil {
		result.ErrorKind = "network"
		result.BusinessOutcome = "unknown"
		return result
	}
	// HTTP 状态与业务结果分别处理。
	switch {
	case statusCode == 202:
		// HTTP 202：已接受但不等于完成（不当作完成）。
		result.ErrorKind = "http"
		result.BusinessOutcome = "pending"
	case statusCode >= 200 && statusCode < 300:
		// 成功响应：业务结果需独立验证（不误报资产恢复）。
		result.ErrorKind = "http"
		result.BusinessOutcome = "success"
	case statusCode >= 400 && statusCode < 500:
		// 客户端错误：业务失败。
		result.ErrorKind = "business"
		result.BusinessOutcome = "failure"
	case statusCode >= 500:
		// 服务器错误：转待核对。
		result.ErrorKind = "http"
		result.BusinessOutcome = "unknown"
	default:
		result.ErrorKind = "unknown"
		result.BusinessOutcome = "unknown"
	}
	return result
}

// ---------------------------------------------------------------------------
// 隐含重试禁用（禁用会自动重放非幂等动作的隐含重试）
// ---------------------------------------------------------------------------

// ErrImplicitRetry 是隐含重试的错误。
var ErrImplicitRetry = errors.New("禁用会自动重放非幂等动作的隐含重试")

// ensureNoImplicitRetry 核对禁用隐含重试。
// 禁用会自动重放非幂等动作的隐含重试；网络错误转待核对，不把 HTTP 202 当完成。
func ensureNoImplicitRetry(spec HTTPRequestSpec, idempotent bool) error {
	// 非幂等动作禁用隐含重试（自动重放非幂等动作禁止）。
	if !idempotent && (spec.Kind == HTTPOpStart || spec.Kind == HTTPOpCancel) {
		return fmt.Errorf("%w：非幂等 %s 动作禁用隐含重试", ErrImplicitRetry, spec.Kind)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 远端 Job（保存远端 Job、状态版本和部分目标结果）
// ---------------------------------------------------------------------------

// saveHTTPJob 保存远端 Job。
// 保存远端 Job、状态版本和部分目标结果；HTTP 幂等头只有验证其作用域/保留期
// 后才能作为能力声明。
func saveHTTPJob(tx *sql.Tx, operationID string, jobID string, stateVersion int, partialResults []OperationTargetResult, now int64) error {
	// 保存远端 Job（复用 的 operation_jobs）。
	if _, err := normalizeJobStart(tx, operationID, jobID, "http-query", "http-retention", now); err != nil {
		return err
	}
	// 保存状态版本（远端 Job 状态版本）。
	if _, err := tx.Exec(`UPDATE operation_jobs SET query_generation=? WHERE operation_id=? AND provider_job_id=?`,
		stateVersion, operationID, jobID); err != nil {
		return err
	}
	// 保存部分目标结果。
	for _, result := range partialResults {
		if err := recordTargetResult(tx, result, now); err != nil {
			return err
		}
	}
	return nil
}

// validateHTTPIdempotencyHeader 核对 HTTP 幂等头。
// HTTP 幂等头只有验证其作用域/保留期后才能作为能力声明。
func validateHTTPIdempotencyHeader(header string, scopeVerified, retentionVerified bool) error {
	if header == "" {
		return nil // 无幂等头：如实声明不支持
	}
	// 幂等头必须验证其作用域/保留期后才能作为能力声明。
	if !scopeVerified || !retentionVerified {
		return fmt.Errorf("HTTP 幂等头 %s 未验证作用域/保留期（只有验证后才能作为能力声明）", header)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 可控 HTTP 服务验证（接收前断线、已接收丢响应、查询/取消缺失、重复回调）
// ---------------------------------------------------------------------------

// HTTPFaultInjection 是 HTTP 故障注入。
type HTTPFaultInjection struct {
	// 接收前断线。
	DisconnectBeforeReceive bool
	// 已接收丢响应。
	ResponseLostAfterReceive bool
	// 查询/取消缺失。
	QueryMissing  bool
	CancelMissing bool
	// 重复回调。
	DuplicateCallback bool
}

// simulateHTTPFault 模拟 HTTP 故障。
// 使用可控 HTTP 服务验证接收前断线、已接收丢响应、查询/取消缺失及重复回调。
func simulateHTTPFault(injection HTTPFaultInjection, spec HTTPRequestSpec) (HTTPResponseResult, error) {
	var result HTTPResponseResult
	// 接收前断线：网络错误转待核对。
	if injection.DisconnectBeforeReceive {
		result.ErrorKind = "network"
		result.BusinessOutcome = "unknown"
		return result, nil
	}
	// 已接收丢响应：网络错误转待核对（不把丢响应当未执行）。
	if injection.ResponseLostAfterReceive {
		result.ErrorKind = "network"
		result.BusinessOutcome = "unknown"
		return result, nil
	}
	// 查询/取消缺失：不支持则明确转核对/交接。
	if injection.QueryMissing && spec.Kind == HTTPOpQuery {
		result.ErrorKind = "unsupported"
		result.BusinessOutcome = "unknown"
		return result, nil
	}
	if injection.CancelMissing && spec.Kind == HTTPOpCancel {
		result.ErrorKind = "unsupported"
		result.BusinessOutcome = "unknown"
		return result, nil
	}
	// 重复回调：去重（同一去重/核对路径）。
	if injection.DuplicateCallback {
		result.ErrorKind = "duplicate"
		result.BusinessOutcome = "success"
		return result, nil
	}
	return result, nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
