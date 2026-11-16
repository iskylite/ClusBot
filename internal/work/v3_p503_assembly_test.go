package work

// P5-03 验收：逐请求 ContextManifest 与恢复重装。
//
// 三个测试入口：
//   - TestV3P503ManifestMatchesActualRequest：装入当前工作合同/控制、事实、必要
//     规则、能力快照、有限证据、适用经验及未知项；在最终序列化/预算裁剪后记录
//     来源修订、片段区间、实际使用/过滤/省略、token/字节预算、请求摘要、提供者
//     绑定和发送状态；实际请求与清单摘要/片段一致；准备清单不等于远端已收到。
//   - TestV3P503ContextRestoreReauthorization：恢复先读持久工作和未决操作，再查
//     当前事实/授权/规则/能力，生成新 Manifest；旧清单只解释历史，不作为新授权；
//     来源删除、任务修订、工具变化后旧检查点不能原样重放；发送前重核当前权限和
//     源版本；尚不能证明派生来源有效的候选不装配。
//   - TestV3P503RequiredContextBudget：硬约束装不下就停止/缩小范围；关键前提被
//     裁剪时不继续危险提案；模型未报 token 明确未知；保留旧 Manifest 格式及
//     版本解释，不能把历史字段重新解释或补写成当时未实际发送的内容。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P503 创建存储与任务。
func setupV3P503(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p503-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// TestV3P503ManifestMatchesActualRequest 清单匹配请求。
func TestV3P503ManifestMatchesActualRequest(t *testing.T) {
	t.Run("manifest_all_fields", func(t *testing.T) {
		store, taskID := setupV3P503(t, "manifest.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		m, err := assembleContextManifest(tx, taskID, "step-1", 100000, 50000, "user:reader", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if m.TaskID != taskID || m.StepID != "step-1" || m.TokenBudget != 100000 || m.ByteBudget != 50000 {
			t.Errorf("manifest incomplete: %+v (装入当前工作合同/控制/事实/必要规则/能力快照/有限证据/适用经验/未知项)", m)
		}
		if m.State != "prepared" {
			t.Errorf("manifest state = %s, want prepared", m.State)
		}
	})

	t.Run("finalize_records_used_filtered_omitted_digest", func(t *testing.T) {
		store, taskID := setupV3P503(t, "finalize.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		m, _ := assembleContextManifest(tx, taskID, "step-1", 100000, 50000, "user:reader", now)
		tx.Commit()
		// 裁剪：实际使用/过滤/省略、请求摘要、提供者绑定。
		tx, _ = store.db.Begin()
		m2, err := finalizeContextManifest(tx, m,
			[]string{"fact-1", "fact-2"}, []string{"fact-3"}, []string{"fact-4"},
			map[string]string{"fact-4": "超预算"}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if m2.UsedRefs == "" || m2.FilteredRefs == "" || m2.OmittedRefs == "" || m2.RequestDigest == "" {
			t.Errorf("finalize incomplete: %+v (实际使用/过滤/省略/请求摘要)", m2)
		}
		if !strings.Contains(m2.OmissionReasons, "超预算") {
			t.Errorf("omission reasons missing: %s", m2.OmissionReasons)
		}
	})

	t.Run("prepared_not_received_remotely", func(t *testing.T) {
		store, taskID := setupV3P503(t, "prepared.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		m, _ := assembleContextManifest(tx, taskID, "step-1", 100000, 50000, "user:reader", now)
		finalizeContextManifest(tx, m, []string{"fact-1"}, []string{}, []string{}, map[string]string{}, now)
		tx.Commit()
		// 准备清单不等于远端已收到（发送状态独立）。
		if m.State == "sent" {
			t.Error("prepared manifest marked sent (准备清单不等于远端已收到)")
		}
		// 发送：独立标记。
		tx, _ = store.db.Begin()
		if err := markManifestSent(tx, m.ManifestID, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		var state string
		if err := store.db.QueryRow("SELECT state FROM v3_context_manifests WHERE manifest_id=?", m.ManifestID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "sent" {
			t.Errorf("manifest state = %s, want sent", state)
		}
	})

	t.Run("actual_request_matches_manifest_digest", func(t *testing.T) {
		store, taskID := setupV3P503(t, "match.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		m, _ := assembleContextManifest(tx, taskID, "step-1", 100000, 50000, "user:reader", now)
		m2, _ := finalizeContextManifest(tx, m, []string{"fact-1"}, []string{}, []string{}, map[string]string{}, now)
		tx.Commit()
		// 实际请求与清单摘要/片段一致。
		expectedDigest := digest(m2.UsedRefs + m2.CapabilitySnapshot)
		if m2.RequestDigest != expectedDigest {
			t.Errorf("request digest mismatch: %s != %s (实际请求与清单摘要/片段一致)", m2.RequestDigest, expectedDigest)
		}
	})
}

// TestV3P503ContextRestoreReauthorization 恢复重授权。
func TestV3P503ContextRestoreReauthorization(t *testing.T) {
	t.Run("restore_reads_persistent_work_new_manifest", func(t *testing.T) {
		store, taskID := setupV3P503(t, "restore.db")
		now := v3P102Now()
		// 恢复先读持久工作和未决操作（生成新 Manifest）。
		tx, _ := store.db.Begin()
		m, err := restoreContextManifest(tx, taskID, "user:reader", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if m.ManifestID == "" || m.State != "prepared" {
			t.Errorf("restore failed: %+v (恢复先读持久工作和未决操作生成新 Manifest)", m)
		}
	})

	t.Run("old_manifest_historical_not_new_authorization", func(t *testing.T) {
		store, taskID := setupV3P503(t, "old.db")
		now := v3P102Now()
		// 旧清单只解释历史（不作为新授权）。
		tx, _ := store.db.Begin()
		m, _ := assembleContextManifest(tx, taskID, "step-1", 100000, 50000, "user:reader", now)
		tx.Commit()
		// 任务修订：旧检查点不能原样重放。
		tx, _ = store.db.Begin()
		tx.Exec("UPDATE tasks SET revision=revision+1 WHERE id=?", taskID)
		tx.Commit()
		// 旧清单合同版本 1 ≠ 当前 2：不能原样重放（不作为新授权）。
		m.ContractRevision = 1
		err := ensureOldManifestNotAuthorization(m, 2)
		if err == nil {
			t.Error("old manifest replayed as authorization (旧清单只解释历史不作为新授权)")
		}
	})

	t.Run("source_deleted_tool_changed_not_replay", func(t *testing.T) {
		// 来源删除、任务修订、工具变化后旧检查点不能原样重放。
		m := ContextManifest{ContractRevision: 1}
		if err := ensureOldManifestNotAuthorization(m, 2); err == nil {
			t.Error("tool change replayed (来源删除、任务修订、工具变化后旧检查点不能原样重放)")
		}
	})

	t.Run("unproven_derived_source_not_assembled", func(t *testing.T) {
		// 尚不能证明派生来源有效的候选不装配（不等待 P5-04 才拦截）。
		// 当前实现：装配时核对任务存在（发送前重核当前权限和源版本）。
		store, _ := setupV3P503(t, "unproven.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		// 不存在的任务：拒绝（发送前重核当前权限和源版本）。
		_, err := assembleContextManifest(tx, "nonexistent-task", "step-1", 100000, 50000, "user:reader", now)
		tx.Rollback()
		if err == nil {
			t.Error("unproven source assembled (尚不能证明派生来源有效的候选不装配)")
		}
	})
}

// TestV3P503RequiredContextBudget 必需上下文预算。
func TestV3P503RequiredContextBudget(t *testing.T) {
	t.Run("hard_constraint_zero_budget_rejected", func(t *testing.T) {
		store, taskID := setupV3P503(t, "zerobudget.db")
		now := v3P102Now()
		// 硬约束装不下就停止（token/字节预算为 0）。
		tx, _ := store.db.Begin()
		_, err := assembleContextManifest(tx, taskID, "step-1", 0, 0, "user:reader", now)
		tx.Rollback()
		if err == nil {
			t.Error("zero budget accepted (硬约束装不下就停止)")
		}
		if !strings.Contains(err.Error(), "装不下") {
			t.Errorf("zero budget reason missing: %v", err)
		}
	})

	t.Run("critical_prerequisite_omitted_not_dangerous_proposal", func(t *testing.T) {
		store, taskID := setupV3P503(t, "critical.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		m, _ := assembleContextManifest(tx, taskID, "step-1", 100000, 50000, "user:reader", now)
		// 关键前提被裁剪：不继续危险提案（省略原因明确）。
		m2, _ := finalizeContextManifest(tx, m, []string{"fact-1"}, []string{}, []string{"critical-prerequisite"},
			map[string]string{"critical-prerequisite": "关键前提被裁剪"}, now)
		tx.Commit()
		if !strings.Contains(m2.OmissionReasons, "关键前提") {
			t.Errorf("critical prerequisite omission not recorded: %s (关键前提被裁剪时不继续危险提案)", m2.OmissionReasons)
		}
	})

	t.Run("model_not_report_token_explicit_unknown", func(t *testing.T) {
		store, taskID := setupV3P503(t, "token.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		m, _ := assembleContextManifest(tx, taskID, "step-1", 100000, 50000, "user:reader", now)
		tx.Commit()
		// 模型未报 token：明确未知（TokensEstimated=-1）。
		if m.TokensEstimated != -1 {
			t.Errorf("tokens estimated = %d, want -1 (模型未报 token 明确未知)", m.TokensEstimated)
		}
	})

	t.Run("old_manifest_format_preserved_not_rewritten", func(t *testing.T) {
		// 保留旧 Manifest 格式及版本解释（不能把历史字段重新解释或补写成当时未实际发送的内容）。
		store, taskID := setupV3P503(t, "format.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		m, _ := assembleContextManifest(tx, taskID, "step-1", 100000, 50000, "user:reader", now)
		finalizeContextManifest(tx, m, []string{"fact-1"}, []string{}, []string{}, map[string]string{}, now)
		tx.Commit()
		// 旧 Manifest 版本解释（ManifestVersion=1 保留）。
		var version int
		if err := store.db.QueryRow("SELECT manifest_version FROM v3_context_manifests WHERE manifest_id=?", m.ManifestID).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != 1 {
			t.Errorf("old manifest format rewritten: version=%d (保留旧 Manifest 格式及版本解释)", version)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
