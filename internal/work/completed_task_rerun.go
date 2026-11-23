package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// A rerun is marked in its durable submission fingerprint for idempotency and
// in its event history so continuations can inherit its no-tool policy.
const completedTaskRerunPrefix = "completed-rerun:"
const rerunNoAutomaticToolsEvent = "run.rerun_no_automatic_tools"

var ErrCompletedTaskRerunUnsupported = errors.New("该事项包含专用流程、外部操作或自动工具记录，不能直接重跑")

func completedTaskRerunFingerprint(taskID string, revision int) string {
	return completedTaskRerunPrefix + digest(fmt.Sprintf("%s:%d", taskID, revision))
}

type rerunInputBoundary struct {
	SourceRunID string `json:"source_run_id"`
	RootRunID   string `json:"root_run_id"`
}

// CheckCompletedTaskRerun exposes the same eligibility check used by the write
// path. A caller must still handle a changed state when submitting the action.
func (s *Store) CheckCompletedTaskRerun(ctx context.Context, taskID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, _, err = completedTaskRerunInputs(ctx, tx, taskID, false)
	return err
}

// ReadCompletedTaskRerun recovers an existing submission without requiring a
// model to be currently available. An empty Run means no submission exists.
func (s *Store) ReadCompletedTaskRerun(ctx context.Context, taskID, requestKey string, revision int) (Run, error) {
	if taskID == "" || strings.TrimSpace(requestKey) == "" || len(requestKey) > 256 || revision < 1 {
		return Run{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	return previousSubmission(tx, requestKey, completedTaskRerunFingerprint(taskID, revision))
}

// RerunCompletedTask creates a new Run under the original Task, preserving its
// previous runs and acceptances. The original request key and revision identify
// one action; a replay with the same pair returns the same Run.
func (s *Store) RerunCompletedTask(ctx context.Context, taskID, requestKey string, revision int) (Run, error) {
	if taskID == "" || strings.TrimSpace(requestKey) == "" || len(requestKey) > 256 || revision < 1 {
		return Run{}, ErrState
	}
	fingerprint := completedTaskRerunFingerprint(taskID, revision)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()

	previous, err := previousSubmission(tx, requestKey, fingerprint)
	if err != nil {
		return Run{}, err
	}
	if previous.ID != "" {
		return previous, tx.Commit()
	}
	task, materials, err := completedTaskRerunInputs(ctx, tx, taskID, true)
	if err != nil {
		return Run{}, err
	}
	if task.Revision != revision {
		return Run{}, ErrTaskRevision
	}
	var sourceRun string
	if err = tx.QueryRowContext(ctx, "SELECT id FROM runs WHERE task_id=? ORDER BY rowid DESC LIMIT 1", taskID).Scan(&sourceRun); err != nil {
		return Run{}, err
	}

	run := Run{ID: id(), TaskID: taskID, ConversationID: task.ConversationID, Status: "queued"}
	if _, err = tx.ExecContext(ctx, "INSERT INTO runs(id,conversation_id,task_id,status,created_at) VALUES(?,?,?,?,?)",
		run.ID, run.ConversationID, run.TaskID, run.Status, now()); err != nil {
		return Run{}, err
	}
	// Existing read results are evidence, not permission to call the tools
	// again. The worker rechecks their authority just before model dispatch.
	if err = reuseTaskRead(tx, taskID, run.ID, true); err != nil {
		return Run{}, err
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO messages(conversation_id,run_id,role,text,replace_materials) VALUES(?,?,'user',?,1)",
		task.ConversationID, run.ID, task.Goal)
	if err != nil {
		return Run{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Run{}, err
	}
	if err = recordInputEvent(tx, seq, requestKey); err != nil {
		return Run{}, err
	}
	for i, material := range materials {
		if _, err = tx.ExecContext(ctx, "INSERT INTO message_materials(message_seq,material_id,ordinal) VALUES(?,?,?)", seq, material.ID, i); err != nil {
			return Run{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO submissions VALUES(?,?,?)", requestKey, fingerprint, run.ID); err != nil {
		return Run{}, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE tasks SET status='active',status_reason='queued',reason='等待重新执行',
completed_at=NULL,revision=revision+1,updated_at=? WHERE id=? AND status='completed' AND revision=?`, now(), taskID, revision)
	if err != nil {
		return Run{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Run{}, err
	} else if n != 1 {
		return Run{}, ErrTaskRevision
	}
	if err = addEvent(tx, run.ID, rerunNoAutomaticToolsEvent, rerunInputBoundary{SourceRunID: sourceRun, RootRunID: run.ID}); err != nil {
		return Run{}, err
	}
	if err = addEvent(tx, run.ID, "run.state_changed", map[string]string{"status": "queued"}); err != nil {
		return Run{}, err
	}
	if err = tx.Commit(); err != nil {
		return Run{}, err
	}
	s.notifyQueue()
	return run, nil
}

// completedTaskRerunInputs validates the current Task and its last accepted
// result in one transaction, and resolves the current persisted material set.
func completedTaskRerunInputs(ctx context.Context, tx *sql.Tx, taskID string, verifyContent bool) (Task, []Material, error) {
	if err := requireOrdinaryTaskFollowup(tx, taskID); err != nil {
		return Task{}, nil, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=?", taskID))
	if err != nil {
		return Task{}, nil, err
	}
	if task.Status != "completed" || task.RequirementsChanged || strings.TrimSpace(task.Goal) == "" {
		return Task{}, nil, ErrState
	}
	if err = checkConversationSubmission(tx, task.ConversationID); err != nil {
		return Task{}, nil, err
	}
	if err = conversationIdle(tx, task.ConversationID); err != nil {
		return Task{}, nil, err
	}
	if err = ensureGoalTaskContinuable(tx, taskID); err != nil {
		return Task{}, nil, err
	}

	// The generic personal worker cannot safely recreate workflow graphs,
	// scheduled occurrences, side effects, or automatic tool conversations.
	var unsupported bool
	err = tx.QueryRowContext(ctx, `SELECT
EXISTS(SELECT 1 FROM workflow_children WHERE task_id=? OR parent_id=?) OR
EXISTS(SELECT 1 FROM workflow_bindings WHERE task_id=?) OR
EXISTS(SELECT 1 FROM goal_tasks WHERE task_id=?) OR
EXISTS(SELECT 1 FROM schedule_occurrences WHERE task_id=?) OR
EXISTS(SELECT 1 FROM operations WHERE task_id=?) OR
EXISTS(SELECT 1 FROM outbox_commands WHERE task_id=?) OR
EXISTS(SELECT 1 FROM runs r JOIN run_read_checkpoints c ON c.run_id=r.id
  JOIN run_reads d ON d.run_id=r.id WHERE r.task_id=? AND c.automatic_tools=1)`,
		taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID).Scan(&unsupported)
	if err != nil {
		return Task{}, nil, err
	}
	if unsupported {
		return Task{}, nil, ErrCompletedTaskRerunUnsupported
	}

	var latestRun, status, content, hash string
	var accepted, incomplete bool
	err = tx.QueryRowContext(ctx, `SELECT r.id,r.status,CASE WHEN ? THEN coalesce(a.content,'') ELSE '' END,coalesce(a.sha256,''),
EXISTS(SELECT 1 FROM task_acceptances x WHERE x.task_id=r.task_id AND x.artifact_id=a.id AND x.sha256=a.sha256),
EXISTS(SELECT 1 FROM run_reads d WHERE d.run_id=r.id AND d.status NOT IN ('completed','reused'))
FROM runs r LEFT JOIN artifacts a ON a.run_id=r.id WHERE r.task_id=? ORDER BY r.rowid DESC LIMIT 1`, verifyContent, taskID).
		Scan(&latestRun, &status, &content, &hash, &accepted, &incomplete)
	if err != nil {
		return Task{}, nil, err
	}
	if status != "completed" || !accepted || hash == "" || (verifyContent && digest(content) != hash) {
		return Task{}, nil, ErrState
	}
	if incomplete {
		return Task{}, nil, ErrReadNeedsSelection
	}
	if err = checkMCPReplyAvailable(tx, latestRun); err != nil {
		return Task{}, nil, err
	}
	var skill, skillScope string
	var skillVersion int
	err = tx.QueryRowContext(ctx, `SELECT r.skill_id,r.version,coalesce(s.scope_label,'') FROM run_skills r
LEFT JOIN personal_skills s ON s.id=r.skill_id WHERE r.run_id=?`, latestRun).Scan(&skill, &skillVersion, &skillScope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Task{}, nil, err
	}
	if err == nil {
		if skillScope != task.Scope {
			return Task{}, nil, ErrReadCheckpoint
		}
		if err = checkRunSkillVersion(ctx, tx, latestRun, skill, skillVersion); err != nil {
			return Task{}, nil, err
		}
	}
	materials, err := completedRunMaterials(ctx, tx, task.ConversationID, latestRun, verifyContent)
	if err != nil {
		return Task{}, nil, err
	}
	materials, err = sourceManifestMaterials(tx, latestRun, materials)
	if err != nil {
		return Task{}, nil, err
	}
	return task, materials, nil
}

// Newer model adapters record the actual material digests sent in the model
// manifest. Older adapters without that field use the persisted selection.
func sourceManifestMaterials(tx *sql.Tx, runID string, selected []Material) ([]Material, error) {
	var raw string
	err := tx.QueryRow(`SELECT manifest FROM context_manifests WHERE run_id=?
AND generation=(SELECT execution_generation FROM runs WHERE id=?) ORDER BY id DESC LIMIT 1`, runID, runID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return selected, nil
	}
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Materials map[string]string `json:"materials"`
	}
	if err = json.Unmarshal([]byte(raw), &manifest); err != nil {
		return nil, ErrReadCheckpoint
	}
	if manifest.Materials == nil {
		return selected, nil
	}
	retained := make([]Material, 0, len(manifest.Materials))
	for _, material := range selected {
		if hash, ok := manifest.Materials[material.ID]; ok {
			if material.Digest != hash {
				return nil, ErrReadCheckpoint
			}
			retained = append(retained, material)
		}
	}
	if len(retained) != len(manifest.Materials) {
		return nil, ErrReadCheckpoint
	}
	return retained, nil
}

// Resolve the selected snapshots as they stood when the accepted Run was
// created. Later standalone conversation inputs do not belong to that Run.
func completedRunMaterials(ctx context.Context, tx *sql.Tx, conversationID, runID string, includeContent bool) ([]Material, error) {
	rows, err := tx.QueryContext(ctx, `WITH eligible AS (
SELECT msg.seq,msg.replace_materials FROM messages msg JOIN runs r ON r.id=msg.run_id
WHERE msg.conversation_id=? AND r.rowid <= (SELECT rowid FROM runs WHERE id=?) AND r.memory_command=0
AND NOT EXISTS(SELECT 1 FROM isolated_reply_pins p WHERE p.run_id=r.id)
), active AS (
SELECT refs.material_id,max(msg.seq) AS latest_seq FROM eligible msg
JOIN message_materials refs ON refs.message_seq=msg.seq
WHERE msg.seq>=coalesce((SELECT max(seq) FROM eligible WHERE replace_materials=1),0)
GROUP BY refs.material_id)
SELECT m.id,m.path,m.sha256,CASE WHEN ? THEN m.content ELSE '' END,m.captured_at,
CASE WHEN u.id IS NULL THEN 'null' ELSE json_object('id',u.id,'name',u.name,'sha256',u.sha256,
 'bytes',u.size,'part',m.upload_part,'parts',u.parts) END
FROM active JOIN materials m ON m.id=active.material_id
JOIN message_materials refs ON refs.message_seq=active.latest_seq AND refs.material_id=m.id
LEFT JOIN uploads u ON u.id=m.upload_id
ORDER BY active.latest_seq DESC,refs.ordinal LIMIT ?`, conversationID, runID, includeContent, MaxMaterials)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var materials []Material
	for rows.Next() {
		var material Material
		var upload string
		if err = rows.Scan(&material.ID, &material.Path, &material.Digest, &material.Content, &material.CapturedAt, &upload); err != nil {
			return nil, err
		}
		if includeContent && digest(material.Content) != material.Digest {
			return nil, errors.New("material snapshot checksum mismatch")
		}
		if err = json.Unmarshal([]byte(upload), &material.Upload); err != nil {
			return nil, err
		}
		material.Size = len(material.Content)
		materials = append(materials, material)
	}
	return materials, rows.Err()
}

func (s *Store) isCompletedTaskRerun(ctx context.Context, runID string) (bool, error) {
	var rerun bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run_events WHERE run_id=? AND kind=?)`, runID, rerunNoAutomaticToolsEvent).Scan(&rerun)
	return rerun, err
}

func inheritRerunNoAutomaticTools(tx *sql.Tx, taskID, runID string) error {
	var payload string
	err := tx.QueryRow(`SELECT data FROM run_events WHERE kind=? AND run_id=(
SELECT id FROM runs WHERE task_id=? AND id<>? ORDER BY rowid DESC LIMIT 1) ORDER BY seq DESC LIMIT 1`,
		rerunNoAutomaticToolsEvent, taskID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var boundary rerunInputBoundary
	if err = json.Unmarshal([]byte(payload), &boundary); err != nil || boundary.SourceRunID == "" || boundary.RootRunID == "" {
		return ErrReadCheckpoint
	}
	return addEvent(tx, runID, rerunNoAutomaticToolsEvent, boundary)
}

func readRerunInputBoundary(tx *sql.Tx, runID string) (*rerunInputBoundary, error) {
	var payload string
	err := tx.QueryRow("SELECT data FROM run_events WHERE run_id=? AND kind=? ORDER BY seq DESC LIMIT 1", runID, rerunNoAutomaticToolsEvent).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var boundary rerunInputBoundary
	if err = json.Unmarshal([]byte(payload), &boundary); err != nil || boundary.SourceRunID == "" || boundary.RootRunID == "" {
		return nil, ErrReadCheckpoint
	}
	var valid bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM runs current JOIN runs root ON root.id=?
JOIN runs source ON source.id=? WHERE current.id=? AND current.task_id IS NOT NULL
AND current.task_id=root.task_id AND current.task_id=source.task_id
AND current.conversation_id=root.conversation_id AND current.conversation_id=source.conversation_id
AND source.rowid<root.rowid AND root.rowid<=current.rowid)`, boundary.RootRunID, boundary.SourceRunID, runID).Scan(&valid)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrReadCheckpoint
	}
	return &boundary, nil
}
