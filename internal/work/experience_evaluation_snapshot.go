package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

type evaluationSnapshot struct {
	Case             ExperienceEvaluationCase
	Current          *ExperienceRef
	CurrentSources   []ExperienceEvaluationSource
	CandidateText    string
	CandidateSources []ExperienceEvaluationSource
}

func snapshotEvaluationCases(ctx context.Context, path string, suite ExperienceEvaluationSuite) ([]evaluationSnapshot, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteFileURI(path, "mode=ro"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if err := requireDatabaseVersion(version); err != nil {
		return nil, err
	}
	snapshots, err := snapshotEvaluationCasesTx(ctx, tx, suite)
	if err != nil {
		return nil, err
	}
	return snapshots, tx.Commit()
}

func snapshotEvaluationCasesTx(ctx context.Context, tx *sql.Tx, suite ExperienceEvaluationSuite) ([]evaluationSnapshot, error) {
	holdout := make([]string, 0, len(suite.HoldoutTasks)+len(suite.Cases))
	holdout = append(holdout, suite.HoldoutTasks...)
	for _, c := range suite.Cases {
		if c.TaskID != "" {
			holdout = append(holdout, c.TaskID)
		}
	}
	excluded, err := json.Marshal(holdout)
	if err != nil {
		return nil, err
	}
	snapshots := make([]evaluationSnapshot, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		c.Goal, c.Scope = strings.TrimSpace(c.Goal), strings.TrimSpace(c.Scope)
		snapshot := evaluationSnapshot{Case: c}
		source, hash, revision, err := selectExperienceForGoal(ctx, tx, c.TaskID, c.Scope, c.Goal, string(excluded))
		if err != nil {
			return nil, err
		}
		if source.Valid {
			snapshot.Current, err = loadExperience(ctx, tx, source.String, hash.String, revision.Int64)
			if err != nil {
				return nil, fmt.Errorf("用例 %q 的现行策略来源不可用: %w", c.ID, err)
			}
			if snapshot.Current != nil {
				var taskID string
				if err := tx.QueryRowContext(ctx, "SELECT task_id FROM artifacts WHERE id=?", source.String).Scan(&taskID); err != nil {
					return nil, err
				}
				snapshot.CurrentSources = []ExperienceEvaluationSource{{TaskID: taskID, ArtifactID: source.String, AcceptanceRevision: int(revision.Int64), Digest: hash.String}}
			}
		}
		snapshot.CandidateText, snapshot.CandidateSources, err = evaluationCandidates(ctx, tx, c, string(excluded))
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func evaluationCandidates(ctx context.Context, tx *sql.Tx, c ExperienceEvaluationCase, excluded string) (string, []ExperienceEvaluationSource, error) {
	if c.Scope == "" {
		return "", nil, nil
	}
	rows, err := tx.QueryContext(ctx, candidateSelect+`
 WHERE e.status='approved' AND e.scope_label=? AND e.goal=?
 AND e.task_id NOT IN (SELECT value FROM json_each(?))
 ORDER BY e.message_seq IS NULL,e.created_at DESC,e.id`, c.Scope, c.Goal, excluded)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	type hint struct {
		Kind       string   `json:"kind"`
		Criteria   string   `json:"acceptance_criteria,omitempty"`
		FormatCues []string `json:"format_cues,omitempty"`
		Correction string   `json:"correction,omitempty"`
	}
	var hints []hint
	var sources []ExperienceEvaluationSource
	for len(hints) < 3 && rows.Next() {
		candidate, err := scanExperienceCandidate(rows)
		if err != nil {
			return "", nil, err
		}
		if candidate.Status != "approved" || candidate.CorrectionTruncated ||
			(candidate.Correction == "" && candidate.Criteria == "" && len(candidate.FormatCues) == 0) {
			continue
		}
		hints = append(hints, hint{candidate.Kind, candidate.Criteria, candidate.FormatCues, candidate.Correction})
		sources = append(sources, ExperienceEvaluationSource{
			TaskID: candidate.TaskID, CandidateID: candidate.ID, CandidateRevision: candidate.Revision, ArtifactID: candidate.ArtifactID,
			AcceptanceRevision: candidate.AcceptanceRevision, MessageID: candidate.MessageID, Digest: candidate.Digest,
		})
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if len(hints) == 0 {
		return "", nil, nil
	}
	encoded, err := json.Marshal(hints)
	return string(encoded), sources, err
}
