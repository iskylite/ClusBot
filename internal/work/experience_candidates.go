package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

var ErrExperienceSource = errors.New("经验来源无效、已变更或已归档")
var ErrExperienceRevision = errors.New("经验候选已变更，请刷新后重试")

type ExperienceCandidate struct {
	ID                  string   `json:"id"`
	TaskID              string   `json:"task_id"`
	Kind                string   `json:"kind"`
	Scope               string   `json:"scope_label"`
	Goal                string   `json:"goal,omitempty"`
	Criteria            string   `json:"acceptance_criteria,omitempty"`
	FormatCues          []string `json:"format_cues,omitempty"`
	Correction          string   `json:"correction,omitempty"`
	CorrectionTruncated bool     `json:"correction_truncated,omitempty"`
	ArtifactID          string   `json:"artifact_id,omitempty"`
	AcceptanceRevision  int      `json:"acceptance_revision,omitempty"`
	MessageID           int64    `json:"message_id,omitempty"`
	Digest              string   `json:"sha256"`
	CounterArtifactID   string   `json:"counter_artifact_id,omitempty"`
	CounterDigest       string   `json:"counter_sha256,omitempty"`
	Status              string   `json:"status"`
	Revision            int      `json:"revision"`
	CreatedAt           int64    `json:"created_at"`
}

func acceptedFormatCues(content string) []string {
	cues := []string{}
	var fence byte
	var fenceSize int
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if len(line) >= 3 && (line[0] == 96 || line[0] == '~') {
			size := len(line) - len(strings.TrimLeft(line, string(line[0])))
			switch {
			case fence == 0 && size >= 3:
				fence, fenceSize = line[0], size
			case line[0] == fence && size >= fenceSize && strings.TrimSpace(line[size:]) == "":
				fence = 0
			}
			continue
		}
		if fence != 0 {
			continue
		}
		level := len(line) - len(strings.TrimLeft(line, "#"))
		if level < 1 || level > 6 || len(line) <= level || line[level] != ' ' {
			continue
		}
		heading := strings.TrimSpace(strings.TrimRight(line[level:], "#"))
		if heading != "" {
			runes := []rune(heading)
			cues = append(cues, string(runes[:min(len(runes), 100)]))
		}
		if len(cues) == 5 {
			break
		}
	}
	return cues
}

func recordAcceptedExperience(tx *sql.Tx, taskID, artifactID string, revision int, content, hash string) error {
	cues, err := json.Marshal(acceptedFormatCues(content))
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO experience_candidates
(id,task_id,artifact_id,acceptance_revision,source_digest,scope_label,goal,criteria,format_cues,created_at)
SELECT ?,t.id,x.artifact_id,x.revision,x.sha256,coalesce(v.scope_label,t.scope_label),coalesce(v.goal,t.goal),x.criteria,?,x.accepted_at
FROM task_acceptances x JOIN tasks t ON t.id=x.task_id JOIN conversations c ON c.id=t.conversation_id
JOIN artifacts a ON a.id=x.artifact_id JOIN runs r ON r.id=a.run_id
LEFT JOIN run_bindings b ON b.run_id=r.id AND b.generation=r.execution_generation
LEFT JOIN task_revisions v ON v.task_id=t.id AND v.revision=b.task_revision
WHERE t.id=? AND x.artifact_id=? AND x.revision=? AND x.sha256=? AND c.archived=0 AND NOT `+revokedMCPReply("a.run_id")+`
AND (v.task_id IS NOT NULL OR t.revision=x.revision)
ON CONFLICT DO NOTHING`, id(), string(cues), taskID, artifactID, revision, hash)
	return err
}

func (s *Store) CaptureAcceptedExperience(ctx context.Context, taskID, artifactID string) (ExperienceCandidate, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExperienceCandidate{}, err
	}
	defer tx.Rollback()
	var revision int
	var content, hash string
	err = tx.QueryRow(`SELECT x.revision,a.content,x.sha256 FROM workflow_accepted_outputs x
JOIN artifacts a ON a.id=x.artifact_id JOIN tasks t ON t.id=x.task_id JOIN conversations c ON c.id=t.conversation_id
WHERE x.task_id=? AND x.artifact_id=? AND c.archived=0 AND NOT `+revokedMCPReply("a.run_id"), taskID, artifactID).Scan(&revision, &content, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ExperienceCandidate{}, ErrExperienceSource
	}
	if err != nil {
		return ExperienceCandidate{}, err
	}
	if digest(content) != hash {
		return ExperienceCandidate{}, ErrExperienceSource
	}
	if err = recordAcceptedExperience(tx, taskID, artifactID, revision, content, hash); err != nil {
		return ExperienceCandidate{}, err
	}
	result, err := scanExperienceCandidate(tx.QueryRow(candidateSelect+" WHERE e.task_id=? AND e.acceptance_revision=?", taskID, revision))
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrExperienceSource
	}
	if err != nil {
		return result, err
	}
	if result.Status == "unavailable" {
		return result, ErrExperienceSource
	}
	return result, tx.Commit()
}

// Only a user-authored Task message can serve as an explicit correction.
func (s *Store) CaptureExperienceCorrection(ctx context.Context, messageID int64, counterArtifact string) (ExperienceCandidate, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExperienceCandidate{}, err
	}
	defer tx.Rollback()
	var taskID, text, scope, goal string
	var runOrder int64
	err = tx.QueryRow(`SELECT t.id,m.text,coalesce(v.scope_label,t.scope_label),coalesce(v.goal,t.goal),r.rowid FROM messages m
JOIN runs r ON r.id=m.run_id JOIN tasks t ON t.id=r.task_id JOIN conversations c ON c.id=t.conversation_id
LEFT JOIN run_bindings b ON b.run_id=r.id AND b.generation=(SELECT min(generation) FROM run_bindings WHERE run_id=r.id)
LEFT JOIN task_revisions v ON v.task_id=t.id AND v.revision=b.task_revision
WHERE m.seq=? AND m.role='user' AND t.conversation_id=m.conversation_id AND c.archived=0
AND (v.task_id IS NOT NULL OR r.rowid=(SELECT max(rowid) FROM runs WHERE task_id=t.id))`,
		messageID).Scan(&taskID, &text, &scope, &goal, &runOrder)
	if errors.Is(err, sql.ErrNoRows) {
		return ExperienceCandidate{}, ErrExperienceSource
	}
	if err != nil {
		return ExperienceCandidate{}, err
	}
	if strings.TrimSpace(text) == "" {
		return ExperienceCandidate{}, ErrExperienceSource
	}
	var counterDigest string
	if counterArtifact != "" {
		var content string
		err = tx.QueryRow(`SELECT a.sha256,a.content FROM artifacts a JOIN runs r ON r.id=a.run_id
WHERE a.id=? AND a.task_id=? AND r.rowid<? AND NOT `+revokedMCPReply("a.run_id"), counterArtifact, taskID, runOrder).Scan(&counterDigest, &content)
		if errors.Is(err, sql.ErrNoRows) {
			return ExperienceCandidate{}, ErrExperienceSource
		}
		if err != nil {
			return ExperienceCandidate{}, err
		}
		if digest(content) != counterDigest {
			return ExperienceCandidate{}, ErrExperienceSource
		}
	}
	if _, err = tx.Exec(`INSERT INTO experience_candidates
(id,task_id,message_seq,source_digest,counter_artifact_id,counter_digest,scope_label,goal,created_at)
VALUES(?,?,?,?,nullif(?,''),?,?,?,?) ON CONFLICT DO NOTHING`,
		id(), taskID, messageID, digest(text), counterArtifact, counterDigest, scope, goal, now()); err != nil {
		return ExperienceCandidate{}, err
	}
	result, err := scanExperienceCandidate(tx.QueryRow(candidateSelect+" WHERE e.message_seq=?", messageID))
	if err != nil {
		return result, err
	}
	if result.Status == "unavailable" {
		return result, ErrExperienceSource
	}
	if result.CounterArtifactID != counterArtifact {
		return ExperienceCandidate{}, ErrConflict
	}
	return result, tx.Commit()
}

var candidateSelect = `SELECT e.id,e.task_id,e.scope_label,e.goal,e.criteria,e.format_cues,
coalesce(e.artifact_id,''),coalesce(e.acceptance_revision,0),coalesce(e.message_seq,0),e.source_digest,
coalesce(e.counter_artifact_id,''),e.counter_digest,e.status,e.revision,e.created_at,coalesce(m.text,''),coalesce(source.content,''),coalesce(counter.content,''),
c.archived=0 AND t.scope_label=e.scope_label AND t.goal=e.goal
AND NOT ` + revokedMCPReply("source.run_id") + ` AND NOT ` + revokedMCPReply("counter.run_id") + `
AND (e.message_seq IS NULL OR (m.role='user' AND m.conversation_id=t.conversation_id))
AND (e.artifact_id IS NULL OR EXISTS(SELECT 1 FROM workflow_accepted_outputs a
WHERE a.task_id=e.task_id AND a.artifact_id=e.artifact_id AND a.revision=e.acceptance_revision AND a.sha256=e.source_digest))
AND (e.counter_artifact_id IS NULL OR EXISTS(SELECT 1 FROM artifacts a
WHERE a.id=e.counter_artifact_id AND a.task_id=e.task_id AND a.sha256=e.counter_digest))
FROM experience_candidates e JOIN tasks t ON t.id=e.task_id JOIN conversations c ON c.id=t.conversation_id
LEFT JOIN messages m ON m.seq=e.message_seq
LEFT JOIN artifacts source ON source.id=e.artifact_id LEFT JOIN artifacts counter ON counter.id=e.counter_artifact_id`

func scanExperienceCandidate(row scanner) (ExperienceCandidate, error) {
	var item ExperienceCandidate
	var cues, text, artifact, counter string
	var valid bool
	err := row.Scan(&item.ID, &item.TaskID, &item.Scope, &item.Goal, &item.Criteria, &cues,
		&item.ArtifactID, &item.AcceptanceRevision, &item.MessageID, &item.Digest, &item.CounterArtifactID,
		&item.CounterDigest, &item.Status, &item.Revision, &item.CreatedAt, &text, &artifact, &counter, &valid)
	if err != nil {
		return item, err
	}
	item.Kind = "accepted_result"
	if item.MessageID != 0 {
		item.Kind = "user_correction"
		valid = valid && digest(text) == item.Digest
	} else {
		valid = valid && digest(artifact) == item.Digest
	}
	if item.CounterArtifactID != "" {
		valid = valid && digest(counter) == item.CounterDigest
	}
	if !valid {
		item.Status = "unavailable"
		item.Scope, item.Goal, item.Criteria = "", "", ""
		return item, nil
	}
	if item.Kind == "user_correction" {
		runes := []rune(text)
		item.Correction = string(runes[:min(len(runes), 1200)])
		item.CorrectionTruncated = len(runes) > 1200
	}
	err = json.Unmarshal([]byte(cues), &item.FormatCues)
	return item, err
}

func (s *Store) ExperienceCandidates(ctx context.Context) ([]ExperienceCandidate, error) {
	rows, err := s.db.QueryContext(ctx, candidateSelect+" ORDER BY e.status<>'pending',e.created_at DESC,e.rowid DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ExperienceCandidate{}
	for rows.Next() {
		item, err := scanExperienceCandidate(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ReviewExperience(ctx context.Context, id string, revision int, decision string) (ExperienceCandidate, error) {
	if decision != "approved" && decision != "rejected" {
		return ExperienceCandidate{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExperienceCandidate{}, err
	}
	defer tx.Rollback()
	current, err := scanExperienceCandidate(tx.QueryRow(candidateSelect+" WHERE e.id=?", id))
	if err != nil {
		return current, err
	}
	if current.Status == "unavailable" {
		return current, ErrExperienceSource
	}
	if current.Status == decision && current.Revision == revision+1 {
		return current, nil
	}
	if current.Status != "pending" || current.Revision != revision {
		return current, ErrExperienceRevision
	}
	if _, err = tx.Exec("UPDATE experience_candidates SET status=?,revision=revision+1 WHERE id=?", decision, id); err != nil {
		return current, err
	}
	current.Status, current.Revision = decision, revision+1
	return current, tx.Commit()
}
