package work

import (
	"context"
	"database/sql"
	"fmt"
)

// ContextManifest 是 v3_context_manifests 的一行。
type ContextManifest struct {
	ManifestID         string `json:"manifest_id"`
	TaskID             string `json:"task_id"`
	StepID             string `json:"step_id"`
	ContractRevision   int    `json:"contract_revision"`
	FactsRefs          string `json:"facts_refs"`
	RulesRefs          string `json:"rules_refs"`
	CapabilitySnapshot string `json:"capability_snapshot"`
	EvidenceRefs       string `json:"evidence_refs"`
	ExperienceRefs     string `json:"experience_refs"`
	Unknowns           string `json:"unknowns"`
	UsedRefs           string `json:"used_refs"`
	FilteredRefs       string `json:"filtered_refs"`
	OmittedRefs        string `json:"omitted_refs"`
	OmissionReasons    string `json:"omission_reasons"`
	TokenBudget        int    `json:"token_budget"`
	ByteBudget         int    `json:"byte_budget"`
	TokensEstimated    int    `json:"tokens_estimated"`
	RequestDigest      string `json:"request_digest"`
	ProviderRef        string `json:"provider_ref"`
	State              string `json:"state"`
	SourceRevisions    string `json:"source_revisions"`
	ExcerptRanges      string `json:"excerpt_ranges"`
	ManifestVersion    int    `json:"manifest_version"`
	CreatedAt          int64  `json:"created_at"`
	SentAt             int64  `json:"sent_at"`
}

// LatestContextManifest 读取任务最近一次上下文装配，供排障视图查看。
// 返回 sql.ErrNoRows 表示该任务还没有装配记录。
func (s *Store) LatestContextManifest(ctx context.Context, taskID string) (ContextManifest, error) {
	var m ContextManifest
	var idNum, assembledAt int64
	var rawManifest string
	err := s.db.QueryRowContext(ctx, `SELECT cm.id,r.task_id,cm.run_id,cm.previous_revision,cm.control_revision,cm.assembled_at,cm.manifest
FROM context_manifests cm JOIN runs r ON r.id=cm.run_id
WHERE r.task_id=? ORDER BY cm.id DESC LIMIT 1`, taskID).
		Scan(&idNum, &m.TaskID, &m.StepID, &m.ContractRevision, &m.ManifestVersion, &assembledAt, &rawManifest)
	if err == sql.ErrNoRows {
		return ContextManifest{}, sql.ErrNoRows
	}
	if err != nil {
		return ContextManifest{}, err
	}
	m.ManifestID = fmt.Sprintf("cm-%d", idNum)
	m.RequestDigest = digest(rawManifest)
	m.UsedRefs = rawManifest
	m.State = "sent"
	m.CreatedAt = assembledAt
	m.SentAt = assembledAt
	return m, nil
}
