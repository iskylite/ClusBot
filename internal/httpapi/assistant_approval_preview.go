package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"clusbot/internal/work"
)

func approvalPreviewValue(raw string) string {
	if assistantCredentialValue.MatchString(raw) {
		return "（敏感内容已省略）"
	}
	return assistantSurfaceValue(raw)
}

func approvalPreviewLines(a work.AssistantPendingApproval) []string {
	switch a.Outcome() {
	case "show_approval_preview":
		expires := "原记录未设期限"
		if a.ExpiresAt > 0 {
			expires = time.UnixMilli(a.ExpiresAt).UTC().Format("2006-01-02 15:04:05 UTC")
		}
		version := func(v int) string {
			if v <= 0 {
				return "未记录"
			}
			return strconv.Itoa(v)
		}
		decision := assistantEnum(a.ConfirmAccess, "当前无确认权限", map[string]string{
			"can_confirm":    "本次核对时有确认权限，但不代表原操作可获批准",
			"other_approver": "此请求指定由其他人决定",
		})
		reviewed := time.UnixMilli(a.CheckedAt()).UTC().Format("2006-01-02 15:04:05 UTC")
		return []string{
			"审批只读预览 · 核对于 " + reviewed + "。未批准、拒绝或派发。",
			fmt.Sprintf("待确认动作：%s。对象：%s。", approvalPreviewValue(a.Action), approvalPreviewValue(a.Object)),
			"申请主体：当前账号。" + decision + "。",
			fmt.Sprintf("到期：%s。合同修订 %s · 策略 %s · 资产修订 %s。", expires, version(a.ContractRevision), version(a.PolicyVersion), version(a.AssetRevision)),
			"记录绑定原请求的参数。此卡未读取具体参数、数据去向或现场条件。",
		}
	case "ask_for_details":
		return []string{"请在原对话补充想核对的内容。未提交决定。"}
	case "cannot_prepare":
		return []string{"这次未能准备审批预览。未提交决定。"}
	default:
		return nil
	}
}

func (s Server) assistantApprovalPreview(w http.ResponseWriter, r *http.Request) {
	rt, card, ok := s.assistantAttentionCardScope(w, r)
	if !ok {
		return
	}
	s.assistantReadSlotAction(w, r, rt, assistantReadSlotHooks{
		action: assistantApprovalPreviewAction, surfaceAction: assistantAttentionReasonAction,
		field: "attention_slot", component: "approvalview", decision: "show_approval_preview", revision: card.Revision,
		visible: func(slot string) bool { _, ok := card.Reviews[slot]; return ok },
		proof:   func(slot string) string { return card.Reviews[slot].Fingerprint },
		saved:   func(slot string) bool { return card.Reviews[slot].Outcome() != "" },
		refresh: func(ctx context.Context) (int64, error) {
			fresh, err := s.Store.AssistantAttentionCard(ctx, s.principal(r), r.PathValue("id"), r.PathValue("run"))
			card = fresh
			return card.Revision, err
		},
		claim: s.Store.BeginAssistantApprovalPreview, finish: s.Store.FinishAssistantApprovalPreview,
		fail: s.Store.FailAssistantApprovalPreview, interpret: s.Settings.AssistantApprovalPreviewDecision,
		render: func() { s.writeAssistantAttentionCard(w, r, rt, card) },
	})
}
