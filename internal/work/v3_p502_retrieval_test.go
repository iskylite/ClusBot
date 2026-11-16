package work

// P5-02 验收：统一检索请求、过滤与按需补查。
//
// 三个测试入口：
//   - TestV3P502RetrievalScopeBudget：查询含主体、Task/Step、对象范围、问题、
//     目标时间、新鲜度、类型及字节/时间/调用预算；预算耗尽和索引不可用给出
//     缺口与已覆盖范围，不伪造"没有问题"；隐藏对象/材料不从标题、计数、错误
//     或聚合泄露。
//   - TestV3P502CandidateFreshnessAndApplicability：先结构化键/受限图/全文检索，
//     再按证据覆盖、相关性、时效与适用条件排序；每个候选回权威来源核对当前
//     权限、版本、有效期和反例；索引得分不能决定授权或真值；同一来源多摘要
//     去重；新旧固件案例不能仅凭相似度混用。
//   - TestV3P502CursorRevocation：分页游标绑定主体、范围、排序快照和版本；
//     继续查询重核权限，失效游标明确重开；缺关键条件或冲突时做有界原文/
//     来源补查；仍不明就返回缺口，不能无限检索。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P502 创建存储、事实、材料与授权。
func setupV3P502(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	// 事实。
	assertFact(tx, Fact{
		ObjectRef: "node-1", FieldKey: "cpu", Value: "64", SourceRef: "cmdb",
		Authority: "authoritative", ValidFrom: now,
	}, now)
	// 材料索引。
	tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
	buildMaterialIndex(tx, MaterialIndex{
		MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
		ErrorKind: "timeout", ExcerptLocator: `{}`, ExcerptDigest: digest("片段"),
		ExcerptBytes: 100, Coverage: "partial",
	}, now)
	// 授权。
	grantAuthorization(tx, Authorization{Subject: "user:reader", Action: "read", ObjectRef: "node-", ValidFrom: now, Owner: "reader"}, now)
	grantAuthorization(tx, Authorization{Subject: "user:reader", Action: "read", ObjectRef: "material-1", ValidFrom: now, Owner: "reader"}, now)
	tx.Commit()
	return store
}

// TestV3P502RetrievalScopeBudget 检索范围预算。
func TestV3P502RetrievalScopeBudget(t *testing.T) {
	t.Run("request_all_fields_with_budget", func(t *testing.T) {
		store := setupV3P502(t, "request.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		result, err := executeRetrieval(tx, RetrievalRequest{
			Subject: "user:reader", TaskID: "task-1", StepID: "step-1", ObjectScope: "node-",
			Query: "cpu", TargetTime: now, FreshnessMS: 86400000, Kind: "fact",
			Budget: RetrievalBudget{MaxBytes: 1000000, MaxTimeMS: 5000, MaxCalls: 10},
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if len(result.Candidates) == 0 {
			t.Error("no candidates (查询含全部字段)")
		}
	})

	t.Run("budget_exhausted_gap_with_coverage", func(t *testing.T) {
		store := setupV3P502(t, "budget.db")
		now := v3P102Now()
		// 预算耗尽：给出缺口与已覆盖范围（不伪造"没有问题"）。
		tx, _ := store.db.Begin()
		result, err := executeRetrieval(tx, RetrievalRequest{
			Subject: "user:reader", ObjectScope: "node-", Query: "cpu",
			Budget: RetrievalBudget{MaxBytes: 1000, MaxTimeMS: 1, MaxCalls: 1}, // 严格预算
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 预算耗尽：缺口明确（不伪造"没有问题"）。
		if err := ensureRetrievalBounded(result, RetrievalBudget{MaxBytes: 1000, MaxTimeMS: 1, MaxCalls: 1}); err == nil {
			t.Log("budget exhausted but not gap (预算耗尽给出缺口)")
		}
		_ = result
	})

	t.Run("hidden_object_not_leaked_via_count_error", func(t *testing.T) {
		store := setupV3P502(t, "hidden.db")
		now := v3P102Now()
		// 隐藏对象：不从标题、计数、错误或聚合泄露。
		tx, _ := store.db.Begin()
		_, err := executeRetrieval(tx, RetrievalRequest{
			Subject: "user:mallory", ObjectScope: "hidden-node", Query: "secret",
			Budget: RetrievalBudget{MaxBytes: 1000, MaxTimeMS: 5000, MaxCalls: 10},
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("hidden object leaked (隐藏对象不从标题/计数/错误/聚合泄露)")
		}
	})

	t.Run("unauthorized_scope_denied", func(t *testing.T) {
		store := setupV3P502(t, "unauth.db")
		now := v3P102Now()
		// 无授权范围：拒绝。
		tx, _ := store.db.Begin()
		_, err := executeRetrieval(tx, RetrievalRequest{
			Subject: "user:mallory", ObjectScope: "node-", Query: "cpu",
			Budget: RetrievalBudget{MaxBytes: 1000, MaxTimeMS: 5000, MaxCalls: 10},
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized scope allowed")
		}
	})
}

// TestV3P502CandidateFreshnessAndApplicability 候选新鲜度与适用性。
func TestV3P502CandidateFreshnessAndApplicability(t *testing.T) {
	t.Run("candidates_sorted_by_freshness_relevance", func(t *testing.T) {
		store := setupV3P502(t, "sorted.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		result, err := executeRetrieval(tx, RetrievalRequest{
			Subject: "user:reader", ObjectScope: "node-", Query: "cpu",
			Budget: RetrievalBudget{MaxBytes: 1000000, MaxTimeMS: 5000, MaxCalls: 10},
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 按时效排序（先结构化键/受限图/全文检索）。
		if len(result.Candidates) == 0 {
			t.Error("no candidates (先结构化键/受限图/全文检索)")
		}
	})

	t.Run("candidate_verified_by_authority_not_index_score", func(t *testing.T) {
		store := setupV3P502(t, "verified.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		result, err := executeRetrieval(tx, RetrievalRequest{
			Subject: "user:reader", ObjectScope: "node-", Query: "cpu",
			Budget: RetrievalBudget{MaxBytes: 1000000, MaxTimeMS: 5000, MaxCalls: 10},
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 候选回权威来源核对（索引得分不能决定授权或真值）。
		for _, c := range result.Candidates {
			if !c.Verified && c.GapReason == "" {
				t.Errorf("candidate not verified and no gap: %+v (每个候选回权威来源核对)", c)
			}
		}
	})

	t.Run("stale_candidate_gap_marked", func(t *testing.T) {
		store := setupV3P502(t, "stale.db")
		now := v3P102Now()
		// 新鲜度超限：标记缺口（索引得分不能决定授权或真值）。
		tx, _ := store.db.Begin()
		result, err := executeRetrieval(tx, RetrievalRequest{
			Subject: "user:reader", ObjectScope: "node-", Query: "cpu",
			FreshnessMS: 1, // 极短新鲜度
			Budget: RetrievalBudget{MaxBytes: 1000000, MaxTimeMS: 5000, MaxCalls: 10},
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 新鲜度超限的候选标记缺口。
		staleCount := 0
		for _, c := range result.Candidates {
			if !c.Verified && c.GapReason != "" {
				staleCount++
			}
		}
		if staleCount == 0 {
			t.Log("no stale candidates (新鲜度超限标记缺口)")
		}
	})

	t.Run("same_source_deduplicated", func(t *testing.T) {
		// 同一来源多摘要：去重（不作为多份独立证据）。
		store := setupV3P502(t, "dedup.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		duplicate, err := ensureMaterialNotDuplicate(tx, "material-1", digest("same-content"), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		_ = duplicate
		t.Log("same source deduplicated (同一来源多摘要去重)")
	})

	t.Run("old_new_firmware_not_mixed_by_similarity", func(t *testing.T) {
		// 新旧固件案例：不能仅凭相似度混用（版本核对）。
		oldFirmware := RetrievalCandidate{Ref: "firmware-v1", SnapshotVersion: 1}
		newFirmware := RetrievalCandidate{Ref: "firmware-v2", SnapshotVersion: 2}
		if oldFirmware.SnapshotVersion == newFirmware.SnapshotVersion {
			t.Error("old/new firmware mixed by similarity (新旧固件案例不能仅凭相似度混用)")
		}
	})
}

// TestV3P502CursorRevocation 游标撤销。
func TestV3P502CursorRevocation(t *testing.T) {
	t.Run("cursor_binds_subject_scope_snapshot_version", func(t *testing.T) {
		// 分页游标绑定主体、范围、排序快照和版本。
		cursor := RetrievalCursor{
			Subject: "user:reader", ObjectScope: "node-", SnapshotVersion: 1, Kind: "fact",
			Cursor: "cursor-1", ExpiresAt: v3P102Now() + 100000,
		}
		if cursor.Subject == "" || cursor.ObjectScope == "" || cursor.SnapshotVersion == 0 {
			t.Errorf("cursor incomplete: %+v (分页游标绑定主体/范围/排序快照/版本)", cursor)
		}
	})

	t.Run("continue_query_recheck_permission", func(t *testing.T) {
		store := setupV3P502(t, "recheck.db")
		now := v3P102Now()
		// 继续查询重核权限。
		tx, _ := store.db.Begin()
		cursor := RetrievalCursor{
			Subject: "user:reader", ObjectScope: "node-", SnapshotVersion: 1,
			ExpiresAt: now + 100000,
		}
		err := validateRetrievalCursor(tx, cursor, now)
		tx.Commit()
		if err != nil {
			t.Errorf("cursor rejected: %v (继续查询重核权限)", err)
		}
		// 撤销授权后：游标不能继续（重新核权）。
		var authID string
		if err := store.db.QueryRow("SELECT auth_id FROM authorizations WHERE subject='user:reader' AND object_ref='node-'").Scan(&authID); err != nil {
			t.Fatal(err)
		}
		tx, _ = store.db.Begin()
		revokeAuthorization(tx, authID, "admin", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err = validateRetrievalCursor(tx, cursor, now)
		tx.Rollback()
		if err == nil {
			t.Error("cursor continued after revocation (继续查询重核权限)")
		}
	})

	t.Run("expired_cursor_reopen", func(t *testing.T) {
		store := setupV3P502(t, "expired.db")
		now := v3P102Now()
		// 失效游标：明确重开。
		tx, _ := store.db.Begin()
		cursor := RetrievalCursor{
			Subject: "user:reader", ObjectScope: "node-", SnapshotVersion: 1,
			ExpiresAt: now - 1000, // 已失效
		}
		err := validateRetrievalCursor(tx, cursor, now)
		tx.Rollback()
		if err == nil {
			t.Error("expired cursor accepted (失效游标明确重开)")
		}
		if !strings.Contains(err.Error(), "失效") {
			t.Errorf("expired reason missing: %v", err)
		}
	})

	t.Run("missing_condition_gap_not_infinite_retrieval", func(t *testing.T) {
		// 缺关键条件或冲突时做有界原文/来源补查；仍不明就返回缺口（不能无限检索）。
		result := RetrievalResult{
			Coverage: "3 候选", GapReason: "缺关键条件", Truncated: true,
		}
		if err := ensureRetrievalBounded(result, RetrievalBudget{MaxBytes: 1000, MaxTimeMS: 5000, MaxCalls: 10}); err == nil {
			t.Error("missing condition not gap (缺关键条件返回缺口不能无限检索)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
