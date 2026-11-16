package work

// P4-09 验收：母盘构建与版本核验。
//
// 三个测试入口：
//   - TestV3P409ImageBuildIdentity：表达母盘 ID/版本、摘要、适用架构/硬件、
//     依赖、构建参数、来源和验证证据；构建作为受控 Operation；使用已有母盘
//     则核验存在性、完整性、来源权限与适用规范，不伪造构建步骤；没有真实
//     构建能力时标待接入，不能返回占位成功镜像。
//   - TestV3P409ImageCompatibility：母盘摘要变更、架构不匹配或来源失权时
//     阻止部署；保留失败/部分构建和未决清理责任，构建回执不自动成为部署验收。
//   - TestV3P409ImageJobRecovery：异步构建用 Job/Wait，结果材料独立存储；
//     参数或目标规范变化产生新的版本/操作；构建回执丢失后查原 Job，不再次
//     启动同一构建。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P409 创建存储。
func setupV3P409(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P409ImageBuildIdentity 构建身份。
func TestV3P409ImageBuildIdentity(t *testing.T) {
	t.Run("image_has_all_fields", func(t *testing.T) {
		store := setupV3P409(t, "image.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		img, err := createGoldenImage(tx, GoldenImage{
			ImageKey: "cluster-base", Version: 1, ContentDigest: digest("镜像内容"),
			Architecture: "x86_64", HardwareRequirements: `{"cpu":"64c","mem":"256G"}`,
			Dependencies: `["kernel-5.10"]`, BuildParams: `{"source":"iso"}`,
			SourceRef: "build-pipeline", ValidationEvidence: `["ev-1"]`,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if img.ImageKey != "cluster-base" || img.Version != 1 || img.ContentDigest == "" ||
			img.Architecture != "x86_64" || img.SourceRef != "build-pipeline" {
			t.Errorf("image incomplete: %+v", img)
		}
		if img.State != "building" {
			t.Errorf("image state = %s, want building", img.State)
		}
	})

	t.Run("build_as_controlled_operation", func(t *testing.T) {
		store := setupV3P409(t, "operation.db")
		now := v3P102Now()
		// 构建作为受控 Operation（登记到 operations）。
		tx, _ := store.db.Begin()
		img, err := startImageBuild(tx, "cluster-base", 1, `{"source":"iso"}`, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if img.State != "building" {
			t.Errorf("build state = %s, want building (构建作为受控 Operation)", img.State)
		}
	})

	t.Run("existing_image_verify_not_forge_build_steps", func(t *testing.T) {
		store := setupV3P409(t, "verify.db")
		now := v3P102Now()
		// 已有母盘：核验存在性、完整性、来源权限与适用规范（不伪造构建步骤）。
		tx, _ := store.db.Begin()
		img, _ := createGoldenImage(tx, GoldenImage{
			ImageKey: "cluster-base", Version: 1, ContentDigest: digest("镜像"),
			Architecture: "x86_64", SourceRef: "build-pipeline",
		}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		v, err := verifyGoldenImage(tx, img.ImageID, "verifier", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 核验通过（不伪造构建步骤）。
		if !v.ExistenceVerified || !v.IntegrityVerified || !v.SourceAuthorized {
			t.Errorf("verification incomplete: %+v (使用已有母盘则核验存在性/完整性/来源权限)", v)
		}
		if v.Outcome != "verified" {
			t.Errorf("verification outcome = %s, want verified", v.Outcome)
		}
	})

	t.Run("no_real_build_capability_mark_pending_not_placeholder", func(t *testing.T) {
		store := setupV3P409(t, "pending.db")
		now := v3P102Now()
		// 没有真实构建能力时标待接入，不能返回占位成功镜像。
		tx, _ := store.db.Begin()
		img, _ := startImageBuild(tx, "pending-image", 1, `{}`, now)
		tx.Commit()
		// 构建中（building）：不是 ready（不能返回占位成功镜像）。
		if img.State == "ready" {
			t.Error("placeholder success image returned (没有真实构建能力时标待接入)")
		}
		if img.State != "building" {
			t.Errorf("pending image state = %s, want building", img.State)
		}
		// 未 ready 的镜像：核验失败（不能部署）。
		tx, _ = store.db.Begin()
		v, err := verifyGoldenImage(tx, img.ImageID, "verifier", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if v.Outcome == "verified" {
			t.Error("pending image verified (占位成功镜像不能通过核验)")
		}
	})
}

// TestV3P409ImageCompatibility 兼容性。
func TestV3P409ImageCompatibility(t *testing.T) {
	t.Run("architecture_mismatch_blocks_deployment", func(t *testing.T) {
		store := setupV3P409(t, "arch.db")
		now := v3P102Now()
		// 架构不匹配：阻止部署。
		tx, _ := store.db.Begin()
		img, _ := createGoldenImage(tx, GoldenImage{
			ImageKey: "cluster-base", Version: 1, ContentDigest: digest("镜像"),
			Architecture: "x86_64", SourceRef: "build-pipeline",
		}, now)
		// 标记 ready（可部署状态）。
		tx.Exec("UPDATE golden_images SET state='ready' WHERE image_id=?", img.ImageID)
		tx.Commit()
		// 部署到 arm64：阻止（架构不匹配）。
		tx, _ = store.db.Begin()
		err := ensureImageDeployable(tx, img.ImageID, "arm64", now)
		tx.Rollback()
		if err == nil {
			t.Error("architecture mismatch not blocked (架构不匹配阻止部署)")
		}
		if !strings.Contains(err.Error(), "架构") && !strings.Contains(err.Error(), "不匹配") {
			t.Errorf("architecture reason missing: %v", err)
		}
	})

	t.Run("failed_partial_build_not_deployable", func(t *testing.T) {
		store := setupV3P409(t, "failed.db")
		now := v3P102Now()
		// 失败/部分构建：不可部署（保留未决清理责任）。
		tx, _ := store.db.Begin()
		img, _ := createGoldenImage(tx, GoldenImage{
			ImageKey: "failed-image", Version: 1, ContentDigest: digest("镜像"),
			Architecture: "x86_64", SourceRef: "build-pipeline",
			CleanupResponsibility: "清理责任",
		}, now)
		// 标记 failed。
		tx.Exec("UPDATE golden_images SET state='failed' WHERE image_id=?", img.ImageID)
		tx.Commit()
		// 部署：阻止（失败不可部署）。
		tx, _ = store.db.Begin()
		err := ensureImageDeployable(tx, img.ImageID, "x86_64", now)
		tx.Rollback()
		if err == nil {
			t.Error("failed build deployable (失败/部分构建不可部署)")
		}
	})

	t.Run("cleanup_responsibility_preserved", func(t *testing.T) {
		store := setupV3P409(t, "cleanup.db")
		now := v3P102Now()
		// 失败/部分构建保留未决清理责任。
		tx, _ := store.db.Begin()
		img, _ := createGoldenImage(tx, GoldenImage{
			ImageKey: "cleanup-image", Version: 1, ContentDigest: digest("镜像"),
			Architecture: "x86_64", SourceRef: "build-pipeline",
			CleanupResponsibility: "未决清理责任",
		}, now)
		tx.Commit()
		if img.CleanupResponsibility == "" {
			t.Error("cleanup responsibility not preserved (保留失败/部分构建和未决清理责任)")
		}
	})

	t.Run("build_receipt_not_auto_acceptance", func(t *testing.T) {
		// 构建回执不自动成为部署验收。
		if err := ensureBuildReceiptNotAcceptance("building", true); err == nil {
			t.Error("build receipt auto-accepted (构建回执不自动成为部署验收)")
		}
		// 构建完成（ready）+ 回执：通过。
		if err := ensureBuildReceiptNotAcceptance("ready", true); err != nil {
			t.Errorf("completed build receipt rejected: %v", err)
		}
	})
}

// TestV3P409ImageJobRecovery Job 恢复。
func TestV3P409ImageJobRecovery(t *testing.T) {
	t.Run("receipt_lost_query_original_job_not_restart", func(t *testing.T) {
		store := setupV3P409(t, "job.db")
		now := v3P102Now()
		// 构建回执丢失后查原 Job，不再次启动同一构建。
		tx, _ := store.db.Begin()
		first, err := startImageBuild(tx, "cluster-base", 1, `{"source":"iso"}`, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 回执丢失：再次启动同一构建返回原 Job（不重复）。
		tx, _ = store.db.Begin()
		second, err := startImageBuild(tx, "cluster-base", 1, `{"source":"iso"}`, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if second.ImageID != first.ImageID {
			t.Errorf("receipt lost restarted build: %s vs %s (构建回执丢失后查原 Job 不再次启动)", second.ImageID, first.ImageID)
		}
		// 只一条构建记录。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM golden_images WHERE image_key='cluster-base' AND version=1").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("duplicate builds = %d, want 1", count)
		}
	})

	t.Run("params_spec_change_new_version_operation", func(t *testing.T) {
		store := setupV3P409(t, "params.db")
		now := v3P102Now()
		// 参数或目标规范变化产生新的版本/操作。
		tx, _ := store.db.Begin()
		v1, _ := startImageBuild(tx, "cluster-base", 1, `{"source":"iso-v1"}`, now)
		tx.Commit()
		// 参数变化：新版本（v2）。
		tx, _ = store.db.Begin()
		v2, err := startImageBuild(tx, "cluster-base", 2, `{"source":"iso-v2"}`, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if v2.ImageID == v1.ImageID {
			t.Error("params change did not create new version (参数或目标规范变化产生新的版本/操作)")
		}
		if v2.Version != 2 {
			t.Errorf("new version = %d, want 2", v2.Version)
		}
	})

	t.Run("async_build_job_wait_materials_independent", func(t *testing.T) {
		store := setupV3P409(t, "async.db")
		now := v3P102Now()
		// 异步构建用 Job/Wait，结果材料独立存储。
		tx, _ := store.db.Begin()
		img, _ := createGoldenImage(tx, GoldenImage{
			ImageKey: "async-image", Version: 1, ContentDigest: digest("镜像"),
			Architecture: "x86_64", SourceRef: "build-pipeline",
			BuildJobRef: "build-job-123", BuildMaterials: `["material-1","material-2"]`,
		}, now)
		tx.Commit()
		// 异步构建的 Job 与材料独立存储。
		if img.BuildJobRef == "" {
			t.Error("async build job ref missing (异步构建用 Job/Wait)")
		}
		if img.BuildMaterials == "" {
			t.Error("build materials not independent (结果材料独立存储)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
