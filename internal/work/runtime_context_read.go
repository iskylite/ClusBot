package work

import (
	"context"
	"database/sql"
)

// LatestContextManifest 读取任务最近一次上下文装配，供排障视图查看。
// 返回 sql.ErrNoRows 表示该任务还没有装配记录。
func (s *Store) LatestContextManifest(ctx context.Context, taskID string) (ContextManifest, error) {
	var m ContextManifest
	err := s.db.QueryRowContext(ctx, `SELECT manifest_id,task_id,step_id,contract_revision,
facts_refs,rules_refs,capability_snapshot,evidence_refs,experience_refs,unknowns,used_refs,
filtered_refs,omitted_refs,omission_reasons,token_budget,byte_budget,tokens_estimated,
request_digest,provider_ref,state,source_revisions,excerpt_ranges,manifest_version,created_at,sent_at
FROM v3_context_manifests WHERE task_id=? ORDER BY created_at DESC, manifest_id DESC LIMIT 1`, taskID).
		Scan(&m.ManifestID, &m.TaskID, &m.StepID, &m.ContractRevision, &m.FactsRefs, &m.RulesRefs,
			&m.CapabilitySnapshot, &m.EvidenceRefs, &m.ExperienceRefs, &m.Unknowns, &m.UsedRefs,
			&m.FilteredRefs, &m.OmittedRefs, &m.OmissionReasons, &m.TokenBudget, &m.ByteBudget,
			&m.TokensEstimated, &m.RequestDigest, &m.ProviderRef, &m.State, &m.SourceRevisions,
			&m.ExcerptRanges, &m.ManifestVersion, &m.CreatedAt, &m.SentAt)
	if err == sql.ErrNoRows {
		return ContextManifest{}, sql.ErrNoRows
	}
	return m, err
}
