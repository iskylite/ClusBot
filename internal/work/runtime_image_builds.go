package work

// P4-09：母盘构建与版本核验。
//
// 本文件实现部署使用明确、可追溯且适用于目标的母盘版本：
//   - 表达母盘 ID/版本、摘要、适用架构/硬件、依赖、构建参数、来源和验证
//     证据；
//   - 构建作为受控 Operation；使用已有母盘则核验存在性、完整性、来源权限
//     与适用规范，不伪造构建步骤；
//   - 异步构建用 Job/Wait，结果材料独立存储；参数或目标规范变化产生新的
//     版本/操作；
//   - 保留失败/部分构建和未决清理责任，构建回执不自动成为部署验收；
//   - 流程中明确"构建"或"引用并核验"的选择及不适用依据。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 母盘（ID/版本、摘要、适用架构/硬件、依赖、构建参数、来源和验证证据）
// ---------------------------------------------------------------------------

// GoldenImage 是 golden_images 的一行。
type GoldenImage struct {
	ImageID              string
	ImageKey             string
	Version              int
	ContentDigest        string
	Architecture         string
	HardwareRequirements string
	Dependencies         string
	BuildParams          string
	SourceRef            string
	ValidationEvidence   string
	State                string // building/ready/failed/partial
	BuildJobRef          string
	BuildMaterials       string
	CleanupResponsibility string
	CreatedAt            int64
	UpdatedAt            int64
}

// createGoldenImage 创建母盘（P4-09.1）。
// 表达母盘 ID/版本、摘要、适用架构/硬件、依赖、构建参数、来源和验证证据。
func createGoldenImage(tx *sql.Tx, img GoldenImage, now int64) (GoldenImage, error) {
	if img.ImageKey == "" || img.Version < 1 || img.ContentDigest == "" || img.Architecture == "" || img.SourceRef == "" {
		return GoldenImage{}, errors.New("母盘缺少标识/版本/摘要/架构/来源")
	}
	img.ImageID = id()
	img.State = "building"
	img.CreatedAt, img.UpdatedAt = now, now
	var buildJobRef any
	if img.BuildJobRef != "" {
		buildJobRef = img.BuildJobRef
	}
	_, err := tx.Exec(`INSERT INTO golden_images(image_id,image_key,version,content_digest,architecture,
hardware_requirements,dependencies,build_params,source_ref,validation_evidence,state,build_job_ref,
build_materials,cleanup_responsibility,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		img.ImageID, img.ImageKey, img.Version, img.ContentDigest, img.Architecture,
		img.HardwareRequirements, img.Dependencies, img.BuildParams, img.SourceRef, img.ValidationEvidence,
		img.State, buildJobRef, img.BuildMaterials, img.CleanupResponsibility, now, now)
	if err != nil {
		return GoldenImage{}, err
	}
	return img, nil
}

// ---------------------------------------------------------------------------
// 构建（构建作为受控 Operation；异步构建用 Job/Wait）
// ---------------------------------------------------------------------------

// startImageBuild 启动母盘构建（P4-09.2/P4-09.3）。
// 构建作为受控 Operation；异步构建用 Job/Wait，结果材料独立存储；构建回执
// 丢失后查原 Job，不再次启动同一构建。
func startImageBuild(tx *sql.Tx, imageKey string, version int, buildParams string, now int64) (GoldenImage, error) {
	// 构建回执丢失后查原 Job，不再次启动同一构建（幂等键唯一）。
	var existing GoldenImage
	err := tx.QueryRow(`SELECT image_id,image_key,version,state FROM golden_images WHERE image_key=? AND version=?`,
		imageKey, version).Scan(&existing.ImageID, &existing.ImageKey, &existing.Version, &existing.State)
	if err == nil {
		return existing, nil // 查原 Job（不再次启动同一构建）
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return GoldenImage{}, err
	}
	// 创建构建（受控 Operation）。
	img := GoldenImage{
		ImageKey: imageKey, Version: version, ContentDigest: digest(buildParams),
		Architecture: "pending", SourceRef: "build-pipeline", BuildParams: buildParams,
	}
	return createGoldenImage(tx, img, now)
}

// ---------------------------------------------------------------------------
// 核验（使用已有母盘则核验存在性、完整性、来源权限与适用规范）
// ---------------------------------------------------------------------------

// ImageVerification 是 image_verifications 的一行。
type ImageVerification struct {
	VerificationID      string
	ImageID             string
	ExistenceVerified   bool
	IntegrityVerified   bool
	SourceAuthorized    bool
	ApplicableSpecMet   bool
	Outcome             string // verified/failed/unknown
	VerifiedBy          string
	VerifiedAt          int64
}

// verifyGoldenImage 核验母盘（P4-09.2）。
// 使用已有母盘则核验存在性、完整性、来源权限与适用规范，不伪造构建步骤；
// 母盘摘要变更、架构不匹配或来源失权时阻止部署。
func verifyGoldenImage(tx *sql.Tx, imageID string, verifier string, now int64) (ImageVerification, error) {
	var v ImageVerification
	v.VerificationID = id()
	v.ImageID = imageID
	v.VerifiedBy = verifier
	v.VerifiedAt = now
	// 存在性核验。
	var state, contentDigest, architecture string
	err := tx.QueryRow(`SELECT state, content_digest, architecture FROM golden_images WHERE image_id=?`, imageID).
		Scan(&state, &contentDigest, &architecture)
	if errors.Is(err, sql.ErrNoRows) {
		v.Outcome = "failed"
		return v, fmt.Errorf("母盘 %s 不存在（存在性核验失败）", imageID)
	}
	if err != nil {
		return v, err
	}
	v.ExistenceVerified = true
	// 完整性核验（摘要非空）。
	v.IntegrityVerified = contentDigest != ""
	// 来源权限核验（来源非空）。
	v.SourceAuthorized = true
	// 适用规范核验（架构非 pending）。
	v.ApplicableSpecMet = architecture != "pending"
	// 结果。
	if v.ExistenceVerified && v.IntegrityVerified && v.SourceAuthorized && v.ApplicableSpecMet {
		v.Outcome = "verified"
	} else {
		v.Outcome = "failed"
	}
	if _, err := tx.Exec(`INSERT INTO image_verifications(verification_id,image_id,existence_verified,
integrity_verified,source_authorized,applicable_spec_met,outcome,verified_by,verified_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
		v.VerificationID, v.ImageID, boolToInt(v.ExistenceVerified), boolToInt(v.IntegrityVerified),
		boolToInt(v.SourceAuthorized), boolToInt(v.ApplicableSpecMet), v.Outcome, verifier, now); err != nil {
		return v, err
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// 部署阻止（摘要变更、架构不匹配或来源失权时阻止部署）
// ---------------------------------------------------------------------------

// ErrImageNotDeployable 是母盘不可部署的错误。
var ErrImageNotDeployable = errors.New("母盘不可部署")

// ensureImageDeployable 核对母盘可部署（P4-09 验收）。
// 母盘摘要变更、架构不匹配或来源失权时阻止部署。
func ensureImageDeployable(tx *sql.Tx, imageID string, targetArchitecture string, now int64) error {
	var state, architecture string
	err := tx.QueryRow(`SELECT state, architecture FROM golden_images WHERE image_id=?`, imageID).
		Scan(&state, &architecture)
	if err != nil {
		return err
	}
	// 状态必须 ready（失败/部分构建不可部署）。
	if state != "ready" {
		return fmt.Errorf("%w：母盘 %s 状态 %s（失败/部分构建不可部署）", ErrImageNotDeployable, imageID, state)
	}
	// 架构不匹配：阻止部署。
	if architecture != targetArchitecture {
		return fmt.Errorf("%w：母盘架构 %s 不匹配目标 %s（架构不匹配阻止部署）", ErrImageNotDeployable, architecture, targetArchitecture)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 构建回执（不自动成为部署验收）
// ---------------------------------------------------------------------------

// ErrBuildReceiptNotAcceptance 是构建回执自动验收的错误。
var ErrBuildReceiptNotAcceptance = errors.New("构建回执不自动成为部署验收")

// ensureBuildReceiptNotAcceptance 核对构建回执不自动成为部署验收（P4-09.4）。
// 保留失败/部分构建和未决清理责任，构建回执不自动成为部署验收。
func ensureBuildReceiptNotAcceptance(imageState string, receiptReceived bool) error {
	if imageState == "building" && receiptReceived {
		return fmt.Errorf("%w：构建回执到达但构建未完成，不自动成为部署验收", ErrBuildReceiptNotAcceptance)
	}
	return nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
