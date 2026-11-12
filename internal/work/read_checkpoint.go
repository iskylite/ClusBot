package work

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const readCheckpointVersion = 1
const maxReadModelRequests = 20

type textCheckpointProvider interface {
	TextRunProvider(context.Context, bool) (Provider, string, error)
}

// Retain some of the original budget for an explicit continuation if the
// first answer stops without reporting usage. A continuation may use the rest.
func recoverableAnswerLimit(remaining, generation int) int {
	if generation == 0 && remaining >= 32 {
		return remaining - min(256, max(16, remaining/4))
	}
	return remaining
}

var ErrReadCheckpoint = errors.New("本轮检查点不可继续，请补充要求后重新提交")

type executionGenerationKey struct{}

func executionGeneration(ctx context.Context) int {
	generation, _ := ctx.Value(executionGenerationKey{}).(int)
	return generation
}

func (s *Store) ModelCredentialBinding(key string) string {
	mac := hmac.New(sha256.New, s.connectionKey)
	_, _ = mac.Write([]byte("model-checkpoint\x00" + key))
	return hex.EncodeToString(mac.Sum(nil))
}

func readResumeStatus(tx *sql.Tx, runID string) (string, error) {
	var version int
	var status, control, reason string
	var room, canRetry bool
	err := tx.QueryRow(`SELECT c.version,r.status,r.control,r.error_code,
c.output_budget-c.output_used>=16*(1+(SELECT count(*) FROM run_reads d WHERE d.run_id=r.id AND d.model_round IS NULL AND d.status='queued'))
AND c.active_budget_ms>r.active_ms AND r.model_requests<?
AND (c.automatic_tools=1 OR NOT EXISTS(SELECT 1 FROM run_reads WHERE run_id=r.id)
OR EXISTS(SELECT 1 FROM run_reads WHERE run_id=r.id AND status IN ('queued','completed','reused'))),
r.task_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM run_reads d WHERE d.run_id=r.id AND d.status NOT IN ('completed','reused'))
FROM run_read_checkpoints c JOIN runs r ON r.id=c.run_id WHERE c.run_id=?`, maxReadModelRequests, runID).
		Scan(&version, &status, &control, &reason, &room, &canRetry)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if version != readCheckpointVersion || status != "interrupted" || control == "stop" {
		return "unavailable", nil
	}
	switch reason {
	case "pause", "process_interrupted", "model_timeout", "model_error", "tool_read_unconfirmed":
		if room {
			return "ready", nil
		}
		if canRetry {
			return "retry", nil
		}
	case "output_budget", "run_time_budget", "read_checkpoint_unavailable":
		if canRetry {
			return "retry", nil
		}
	}
	return "unavailable", nil
}

func (s *Store) prepareReadCheckpoint(ctx context.Context, run Run, input []Message, request *Message, query, binding string, limits RunLimits, automatic bool) (int, error) {
	if binding == "" {
		return 0, ErrReadCheckpoint
	}
	binding = digest(binding)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var version, budget, used int
	var previousBinding, previousContext string
	err = tx.QueryRowContext(ctx, `SELECT version,provider_binding,context_digest,output_budget,output_used
FROM run_read_checkpoints WHERE run_id=?`, run.ID).Scan(&version, &previousBinding, &previousContext, &budget, &used)
	fresh := errors.Is(err, sql.ErrNoRows) && run.Generation == 0
	if !fresh && (errors.Is(err, sql.ErrNoRows) || (err == nil && (version != readCheckpointVersion || previousBinding != binding))) {
		return 0, ErrReadCheckpoint
	}
	if err != nil && !fresh {
		return 0, err
	}
	memories, err := listMemories(ctx, tx)
	if err != nil {
		return 0, err
	}
	if fresh {
		request.Memories = matchingMemories(memories, query)
		for _, memory := range request.Memories {
			if _, err = tx.ExecContext(ctx, "INSERT INTO run_memories(run_id,memory_id,revision,title) VALUES(?,?,?,?)", run.ID, memory.ID, memory.Revision, memory.Title); err != nil {
				return 0, err
			}
		}
	} else {
		refs, err := readMemoryRefs(tx, run.ID)
		if err != nil {
			return 0, err
		}
		byID := make(map[string]Memory, len(memories))
		for _, memory := range memories {
			byID[memory.ID] = memory
		}
		for _, ref := range refs {
			memory, ok := byID[ref.ID]
			if !ok || memory.Revision != ref.Revision {
				return 0, ErrReadCheckpoint
			}
			request.Memories = append(request.Memories, memory)
		}
	}
	contextDigest, err := readContextDigest(input, request.Memories)
	if err != nil {
		return 0, err
	}
	if fresh {
		budget = limits.OutputTokens
		if _, err = tx.ExecContext(ctx, `INSERT INTO run_read_checkpoints(run_id,version,provider_binding,context_digest,output_budget,active_budget_ms,automatic_tools)
VALUES(?,?,?,?,?,?,?)`, run.ID, readCheckpointVersion, binding, contextDigest, budget, limits.ActiveTime.Milliseconds(), automatic); err != nil {
			return 0, err
		}
	} else if previousContext != contextDigest {
		return 0, ErrReadCheckpoint
	}
	return min(budget, limits.OutputTokens) - used, tx.Commit()
}

func readContextDigest(input []Message, memories []Memory) (string, error) {
	type memoryContent struct{ ID, Kind, Title, Content string }
	var selected []memoryContent
	for _, memory := range memories {
		selected = append(selected, memoryContent{memory.ID, memory.Kind, memory.Title, memory.Content})
	}
	var loadedMaterials []string
	var reads []TaskRead
	var experiences []*ExperienceRef
	var skillHashes []string
	for _, message := range input {
		if message.Experience != nil {
			experiences = append(experiences, message.Experience)
		}
		if message.Skill != nil {
			skillHashes = append(skillHashes, message.Skill.Digest)
		}
		for _, material := range message.Materials {
			if material.Content != "" {
				loadedMaterials = append(loadedMaterials, material.ID)
			}
		}
		if message.Read != nil {
			reads = append(reads, *message.Read)
		}
		reads = append(reads, message.Reads...)
	}
	payload, err := json.Marshal(struct {
		Messages        []Message
		LoadedMaterials []string
		Memories        []memoryContent
		Reads           []TaskRead       `json:",omitempty"`
		Experiences     []*ExperienceRef `json:",omitempty"`
		SkillHashes     []string         `json:",omitempty"`
	}{input, loadedMaterials, selected, reads, experiences, skillHashes})
	return digest(string(payload)), err
}

func (s *Store) reserveReadRequest(ctx context.Context, runID string, limit int, elapsed time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE runs SET model_requests=model_requests+1,active_ms=max(active_ms,?)
WHERE id=? AND status='running' AND control='' AND execution_generation=? AND model_requests<?
AND NOT EXISTS(SELECT 1 FROM run_reads d WHERE d.run_id=runs.id AND d.model_round IS NOT NULL
AND (d.status NOT IN ('completed','failed','tool_error','unknown') OR (d.status='unknown' AND d.acknowledged_at=0)))`,
		elapsed.Milliseconds(), runID, executionGeneration(ctx), maxReadModelRequests)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrReadCheckpoint
	}
	updated, err = tx.ExecContext(ctx, `UPDATE run_read_checkpoints SET output_used=output_used+?,request_limit=?
WHERE run_id=? AND version=? AND request_limit=0 AND output_budget-output_used>=?`, limit, limit, runID, readCheckpointVersion, limit)
	if err != nil {
		return err
	}
	count, err = updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrReadCheckpoint
	}
	return tx.Commit()
}

func settleReadRequest(tx *sql.Tx, runID string, usage *TokenUsage) error {
	var used *int64
	if usage != nil {
		used = usage.OutputTokens
	}
	_, err := tx.Exec(`UPDATE run_read_checkpoints SET output_used=output_used-request_limit+coalesce(?,request_limit),request_limit=0
WHERE run_id=? AND request_limit>0`, used, runID)
	return err
}
