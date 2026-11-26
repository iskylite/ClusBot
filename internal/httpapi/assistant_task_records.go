package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"clusbot/internal/work"
)

func (s Server) assistantTaskRecords(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor, session := s.principal(r), s.assistantSession(r)
	conversation, run, task := r.PathValue("id"), r.PathValue("run"), r.PathValue("task")
	if !validConversationID(conversation) || !validID(run) || !validID(task) {
		fail(w, 404, "task_records_missing", "当前对话没有这项任务的核对记录。")
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
	records, err := s.Store.ReadAssistantTaskRecords(r.Context(), actor, conversation, run, task)
	if err != nil {
		writeError(w, err)
		return
	}
	if !checkSession() {
		return
	}
	jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "run_id": run, "task_id": task,
		"surface": assistantTaskRecordsSurface(records)})
}

func assistantTaskEvidenceLabel(label, fallback string) string {
	if label == "" || strings.ContainsAny(label, "/\\") || assistantCredentialValue.MatchString(label) {
		return fallback
	}
	clean := assistantSurfaceValue(label)
	if clean == "" || assistantSurfaceURL.MatchString(label) {
		return fallback
	}
	return clean
}

func assistantTaskRecordsSurface(records work.AssistantTaskRecords) []assistantA2UIMessage {
	utc := func(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05 UTC") }
	state := func(raw string) string {
		return assistantEnum(raw, "状态未记录", map[string]string{
			"blocked": "未就绪", "ready": "待执行", "running": "本地记录为运行中", "waiting": "等待处理",
			"completed": "已完成", "failed": "失败", "cancelled": "已取消",
		})
	}
	lines := []string{"步骤与证据", "步骤记录（按计划顺序）"}
	if len(records.Steps) == 0 {
		lines = append(lines, "当前没有可查看的已登记步骤。")
	}
	for _, row := range records.Steps {
		label := "步骤"
		if row.Position > 0 {
			label = fmt.Sprintf("步骤 %d", row.Position)
		}
		line := assistantSurfaceParts(label, assistantTaskEvidenceLabel(row.Label, ""), state(row.State))
		if row.RecordedAt > 0 {
			kind := "结果入账"
			if row.Source == "terminal_audit" {
				kind = "终态登记"
			}
			line += " · " + kind + " " + utc(row.RecordedAt)
		} else if row.State == "completed" || row.State == "failed" || row.State == "cancelled" {
			line += " · 终态时间未记录"
		}
		if row.EvidenceUnverified {
			line += " · 当前无法核对原作业证据"
		}
		lines = append(lines, line)
	}
	if records.MoreSteps {
		lines = append(lines, "这里只核对计划中的前 12 个步骤。")
	}
	lines = append(lines, "已核对的证据记录")
	for n, item := range records.Evidence {
		prefix := fmt.Sprintf("证据 %d", n+1)
		if item.Kind == "benchmark_raw" {
			lines = append(lines, fmt.Sprintf("%s · 基准报告引用的原始证据 · %s · %d 字节 · SHA-256 %s…",
				prefix, assistantTaskEvidenceLabel(item.Label, "测项"), item.SizeBytes, item.Fingerprint[:min(12, len(item.Fingerprint))]))
		} else {
			lines = append(lines, assistantSurfaceParts(prefix, "已验收成果", assistantTaskEvidenceLabel(item.Label, "已验收文件"), "验收登记 "+utc(item.RecordedAt)))
		}
	}
	if len(records.Evidence) == 0 {
		lines = append(lines, "当前没有可列出的已核对证据。")
	}
	if records.MoreEvidence {
		lines = append(lines, "这里只列出前 8 条可核对证据。")
	}
	if records.LimitedSources {
		lines = append(lines, "有些来源已不可查看或无法核实，未展示对应记录。")
	}
	return assistantSurface(append(lines, "这里只列出已保存的记录，不打开文件，也不重新执行。"), false)
}
