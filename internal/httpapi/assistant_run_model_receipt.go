package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"clusbot/internal/work"
)

// Opening an already recorded reply's details is a read, not a model action.
// The only authority on reload is the original persisted Run in the account's
// own store. No chat tool response, command, or model input is re-executed.
func (s Server) assistantConversationModelReceipt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor, session := s.principal(r), s.assistantSession(r)
	conversation, run := r.PathValue("id"), r.PathValue("run")
	message, err := strconv.ParseInt(r.PathValue("message"), 10, 64)
	if err != nil || message < 1 || r.PathValue("message") != strconv.FormatInt(message, 10) || !validID(run) || !validConversationID(conversation) {
		fail(w, http.StatusNotFound, "run_receipt_missing", "找不到这条回复的模型记录。")
		return
	}
	checkSession := func() bool {
		err := s.verifyA2UIAccount(r.Context(), session, actor, false)
		if errors.Is(err, work.ErrAccountDenied) {
			fail(w, http.StatusUnauthorized, "account_session_ended", "登录已失效，请重新登录。")
		} else if err != nil {
			writeError(w, err)
		}
		return err == nil
	}
	if !checkSession() {
		return
	}
	projection, err := s.Store.ReadRunModelReceipt(r.Context(), run, &work.RunReplyOrigin{
		Actor: actor, Conversation: conversation, Message: message,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if !checkSession() {
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"conversation_id": conversation, "run_id": run, "message_id": message, "surface": assistantRunModelReceiptSurface(projection),
	})
}

func assistantRunModelReceiptSurface(record work.RunModelReceiptProjection) []assistantA2UIMessage {
	lines := []string{"本轮模型记录"}
	// Run accounting can also include intake; manifest rows describe execution requests only.
	if usage := record.Usage; usage != nil {
		count := func(value *int64) string {
			if value == nil || *value < 0 {
				return "未知"
			}
			return strconv.FormatInt(*value, 10)
		}
		lines = append(lines, fmt.Sprintf("%d 次模型尝试 · Token 输入 %s / 输出 %s", usage.ModelRequests, count(usage.InputTokens), count(usage.OutputTokens)))
		if usage.UnreportedRequests > 0 {
			lines = append(lines, fmt.Sprintf("%d 次尝试未完整报告 Token。这里只显示已记录的用量。", usage.UnreportedRequests))
		}
	} else {
		lines = append(lines, "未记录本轮 Token 用量。")
	}
	if len(record.Receipts) == 0 {
		return assistantSurface(append(lines, "未记录本轮模型请求明细。"), false)
	}
	if record.More {
		lines = append(lines, "仅显示最近 8 条模型请求记录。")
	}
	lines = append(lines, "Token 按整轮汇总，模型明细只列执行阶段的请求。")
	for _, item := range record.Receipts {
		state := "未记录提供方回执"
		if item.Receipt != nil {
			state = assistantModelReceiptStatus(item.Receipt.State)
		}
		lines = append(lines, fmt.Sprintf("执行代次 %d · %s", item.Generation, state))
		if id := item.RequestedModel; work.ValidModelReceiptIdentifier(id) && !assistantCredentialValue.MatchString(id) {
			lines = append(lines, assistantSurfaceParts("请求模型："+id))
		}
		if item.Receipt != nil {
			if id := item.Receipt.ReportedModel; work.ValidModelReceiptIdentifier(id) && !assistantCredentialValue.MatchString(id) {
				lines = append(lines, assistantSurfaceParts("提供方报告："+id))
			}
		}
	}
	return assistantSurface(append(lines, "提供方报告不等于独立验证。"), false)
}
