package work

import "context"

type BenchmarkEvidenceItem struct {
	ID               string `json:"id"`
	CaseKey          string `json:"case_key"`
	StepID           string `json:"step_id"`
	OperationID      string `json:"operation_id"`
	ProviderJobID    string `json:"provider_job_id"`
	Connection       string `json:"connection"`
	BindingRevision  int    `json:"binding_revision"`
	Target           string `json:"target"`
	SHA256           string `json:"sha256"`
	SizeBytes        int64  `json:"size_bytes"`
	AssessmentLinked bool   `json:"assessment_linked"`
}

type BenchmarkEvidencePage struct {
	TaskID    string                  `json:"task_id"`
	Items     []BenchmarkEvidenceItem `json:"items"`
	NextAfter string                  `json:"next_after,omitempty"`
}

// ListBenchmarkEvidence reads only currently present local artifacts and
// their persisted associations. Capturing bytes with a digest is not the
// same as the verifier accepting their measurements. No bytes, provider
// resource path, or query key is exposed in this inventory.
func (s *Store) ListBenchmarkEvidence(ctx context.Context, actor, task, after string, limit int) (BenchmarkEvidencePage, error) {
	if limit < 1 || limit > 100 || after != "" && !validBenchmarkSHA(after) {
		return BenchmarkEvidencePage{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkEvidencePage{}, err
	}
	defer tx.Rollback()
	page := BenchmarkEvidencePage{Items: []BenchmarkEvidenceItem{}}
	if err := tx.QueryRowContext(ctx, "SELECT task_id FROM benchmark_runs WHERE task_id=? AND actor=?", task, actor).Scan(&page.TaskID); err != nil {
		return BenchmarkEvidencePage{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.artifact_id,c.case_key,c.step_id,o.operation_id,j.provider_job_id,
o.connection_id,o.capability_version,e.target_ref,e.sha256,length(e.body),
EXISTS(SELECT 1 FROM json_each(CASE WHEN json_valid(c.report) THEN c.report ELSE '{}' END,'$.raw_artifact_ids') a
 WHERE a.key=e.target_ref AND a.value=e.artifact_id)
FROM benchmark_evidence e JOIN benchmark_cases c ON c.step_id=e.step_id
JOIN operations o ON o.operation_id=c.operation_id AND o.task_id=c.task_id AND o.step_id=c.step_id
JOIN operation_jobs j ON j.operation_id=o.operation_id AND j.provider_job_id=c.job_id
WHERE c.task_id=? AND e.artifact_id>? ORDER BY e.artifact_id LIMIT ?`, task, after, limit+1)
	if err != nil {
		return BenchmarkEvidencePage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(page.Items) == limit {
			page.NextAfter = page.Items[len(page.Items)-1].ID
			break
		}
		var item BenchmarkEvidenceItem
		if err := rows.Scan(&item.ID, &item.CaseKey, &item.StepID, &item.OperationID, &item.ProviderJobID,
			&item.Connection, &item.BindingRevision, &item.Target, &item.SHA256, &item.SizeBytes, &item.AssessmentLinked); err != nil {
			return BenchmarkEvidencePage{}, err
		}
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}
