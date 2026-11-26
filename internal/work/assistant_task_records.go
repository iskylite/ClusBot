package work

import (
	"context"
	"database/sql"
	"errors"
	"sort"
)

const assistantTaskStepLimit = 12
const assistantTaskEvidenceLimit = 8

type AssistantTaskRecords struct {
	Steps          []AssistantTaskStep
	Evidence       []AssistantTaskEvidence
	MoreSteps      bool
	MoreEvidence   bool
	LimitedSources bool
}

type AssistantTaskStep struct {
	Position             int
	Label, State, Source string
	RecordedAt           int64
	EvidenceUnverified   bool
}

type AssistantTaskEvidence struct {
	Kind, Label, Fingerprint string
	RecordedAt, SizeBytes    int64
}

// ReadAssistantTaskRecords is anchored to a successful task selection on an
// original chat card. Its data is a current, scoped projection of independent
// records. It cannot reapply that receipt, dispatch, or fetch artifact bytes.
func (s *Store) ReadAssistantTaskRecords(ctx context.Context, actor, conversation, run, task string) (AssistantTaskRecords, error) {
	var out AssistantTaskRecords
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	card, err := assistantTaskListCardTx(ctx, tx, actor, conversation, run)
	if err != nil {
		return out, err
	}
	if !card.HasTask(task) || card.Progress[task].Decision != "show_progress" {
		return out, sql.ErrNoRows
	}
	var benchmark bool
	var queryObject, queryOwner string
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM benchmark_runs b WHERE b.task_id=t.id),coalesce(q.object_ref,''),coalesce(q.principal,'')
FROM tasks t LEFT JOIN domain_query_tasks q ON q.task_id=t.id
WHERE t.id=? AND NOT EXISTS(SELECT 1 FROM benchmark_runs b WHERE b.task_id=t.id AND b.actor<>?)`, task, actor).Scan(&benchmark, &queryObject, &queryOwner); err != nil {
		return out, err
	}
	if queryObject != "" {
		if queryOwner != actor {
			return out, sql.ErrNoRows
		}
		stamp := now()
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ops_assets a WHERE a.object_ref=? AND `+assistantAssetReadGrant+`)`, queryObject, actor, stamp, stamp).Scan(&allowed); err != nil {
			return out, err
		}
		if !allowed {
			return out, sql.ErrNoRows
		}
	}
	rows, err := readAssistantTaskStepRows(ctx, tx, task, benchmark)
	if err != nil {
		return out, err
	}
	out.MoreSteps = len(rows) > assistantTaskStepLimit
	for _, row := range rows[:min(len(rows), assistantTaskStepLimit)] {
		if benchmark {
			// A missing case or stale target grant cannot be used to disclose
			// even the case label or state. The existing verifier also checks
			// the exact job query key, credential binding, and assessment.
			if row.Label == "" {
				out.LimitedSources = true
				continue
			}
			proof, err := readAssistantBenchmarkSource(ctx, tx, actor, task, row.Label)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrState) {
				out.LimitedSources = true
				continue
			}
			if err != nil {
				return out, err
			}
			if row.State == string(RuntimeStepCompleted) {
				row.EvidenceUnverified = proof.Status != "verified"
				if row.EvidenceUnverified {
					out.LimitedSources = true
				} else if err := appendAssistantBenchmarkEvidence(ctx, tx, proof, row.Label, &out); err != nil {
					return out, err
				}
			}
		}
		out.Steps = append(out.Steps, row)
	}
	if !benchmark {
		if err := readAssistantTaskAcceptances(ctx, tx, task, &out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func readAssistantTaskStepRows(ctx context.Context, tx *sql.Tx, task string, benchmark bool) ([]AssistantTaskStep, error) {
	// `updated_at` is also set by plan edits and is not evidence of completion.
	// Benchmark producers record the terminal phase in the same transaction;
	// local results use the applied receipt for an exact, current work unit.
	rows, err := tx.QueryContext(ctx, `SELECT s.position,coalesce(c.case_key,CASE WHEN s.definition_key=? THEN '资产验收状态查询' ELSE '' END),s.state,
coalesce(CASE WHEN ? THEN (
 SELECT a.occurred_at FROM runtime_audit a WHERE a.task_id=s.task_id AND a.step_id=s.step_id AND a.occurred_at>0 AND
 ((s.state='completed' AND c.phase='verified' AND a.event='benchmark_case_verified') OR
  (s.state='failed' AND c.phase='incomplete' AND a.event='benchmark_case_incomplete') OR
  (s.state='cancelled' AND c.phase='skipped' AND a.event IN ('benchmark_case_skipped','benchmark_case_skipped_this_run')))
 ORDER BY a.audit_id DESC LIMIT 1
) ELSE (
 SELECT r.applied_at FROM result_receipts r JOIN work_units u ON u.unit_id=r.unit_id
 WHERE u.task_id=s.task_id AND u.step_id=s.step_id AND r.task_id=s.task_id AND r.state='applied' AND r.applied_at>0
 AND r.lease_generation=u.lease_generation
 AND ((r.run_id IS NULL AND r.execution_generation=u.lease_generation) OR EXISTS(SELECT 1 FROM runs rr
  WHERE rr.id=r.run_id AND rr.task_id=s.task_id AND rr.execution_generation=r.execution_generation))
 AND ((s.state='completed' AND u.state='completed' AND s.result_ref=u.result_ref AND r.evidence_ref=u.result_ref)
      OR (s.state='failed' AND u.state='failed'))
 ORDER BY r.applied_at DESC,r.receipt_id LIMIT 1
) END,0)
FROM steps s LEFT JOIN benchmark_cases c ON c.task_id=s.task_id AND c.step_id=s.step_id
WHERE s.task_id=? ORDER BY s.position,s.step_id LIMIT ?`, assetAcceptanceHandlerKey, benchmark, task, assistantTaskStepLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantTaskStep
	for rows.Next() {
		var row AssistantTaskStep
		if err := rows.Scan(&row.Position, &row.Label, &row.State, &row.RecordedAt); err != nil {
			return nil, err
		}
		if row.RecordedAt > 0 {
			row.Source = "result_applied"
			if benchmark {
				row.Source = "terminal_audit"
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func appendAssistantBenchmarkEvidence(ctx context.Context, tx *sql.Tx, proof assistantBenchmarkSource, label string, out *AssistantTaskRecords) error {
	// Only evidence cited by this verified report and for its frozen targets
	// can be listed. Keep physical addresses, job IDs, paths, and bytes private.
	targets := make([]string, 0, len(proof.Report.RawArtifactIDs))
	for target := range proof.Report.RawArtifactIDs {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, target := range targets {
		id := proof.Report.RawArtifactIDs[target]
		if _, ok := proof.Report.VerifiedMetrics[target]; !ok || !validBenchmarkSHA(id) {
			continue
		}
		if len(out.Evidence) == assistantTaskEvidenceLimit {
			out.MoreEvidence = true
			break
		}
		item := AssistantTaskEvidence{Kind: "benchmark_raw", Label: label}
		if err := tx.QueryRowContext(ctx, `SELECT sha256,length(body) FROM benchmark_evidence WHERE artifact_id=? AND step_id=? AND target_ref=?`,
			id, proof.Step, target).Scan(&item.Fingerprint, &item.SizeBytes); errors.Is(err, sql.ErrNoRows) {
			out.LimitedSources = true
			continue
		} else if err != nil {
			return err
		}
		if item.SizeBytes < 1 || !validBenchmarkSHA(item.Fingerprint) {
			out.LimitedSources = true
			continue
		}
		out.Evidence = append(out.Evidence, item)
	}
	return nil
}

func readAssistantTaskAcceptances(ctx context.Context, tx *sql.Tx, task string, out *AssistantTaskRecords) error {
	rows, err := tx.QueryContext(ctx, `SELECT substr(a.name,1,120),a.sha256,max(x.accepted_at)
FROM task_acceptances x JOIN artifacts a ON a.id=x.artifact_id AND a.task_id=x.task_id AND a.sha256=x.sha256
JOIN runs r ON r.id=a.run_id AND r.task_id=x.task_id
WHERE x.task_id=? AND x.accepted_at>0 AND NOT `+revokedMCPReply("a.run_id")+`
GROUP BY a.id ORDER BY max(x.accepted_at) DESC,a.id LIMIT ?`, task, assistantTaskEvidenceLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if len(out.Evidence) == assistantTaskEvidenceLimit {
			out.MoreEvidence = true
			break
		}
		item := AssistantTaskEvidence{Kind: "accepted_artifact"}
		if err := rows.Scan(&item.Label, &item.Fingerprint, &item.RecordedAt); err != nil {
			return err
		}
		if !validBenchmarkSHA(item.Fingerprint) {
			out.LimitedSources = true
			continue
		}
		out.Evidence = append(out.Evidence, item)
	}
	return rows.Err()
}
