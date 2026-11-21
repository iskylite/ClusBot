package work

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

type TaskReadInvocation struct {
	Position int
	CallID   string
}

type TaskReadTurn struct {
	Question string
	Protocol string
	Output   json.RawMessage
	Calls    []TaskReadInvocation
}

type TaskReadRound struct {
	TaskReadTurn
	Results []TaskRead
}

type MultiTaskReadModel interface {
	ReadCheckpointIdentity() (binding, protocol string)
	TaskReadTurn(context.Context, []Message, []TaskRead, []TaskReadRound, int, func(string) error) (TaskReadTurn, *TokenUsage, error)
}

type multiTaskReadProvider interface {
	MultiTaskReadModel(context.Context) (MultiTaskReadModel, error)
}

func (s *Store) saveReadRound(ctx context.Context, runID string, number int, turn TaskReadTurn, usage *TokenUsage, planned ...TaskRead) error {
	if turn.Protocol == "" || len(turn.Output) > 2<<20 || !json.Valid(turn.Output) || len(turn.Calls) == 0 || len(turn.Calls) > MaxTaskReads {
		return ErrReadModelCall
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs r JOIN run_read_checkpoints c ON c.run_id=r.id
WHERE r.id=? AND r.status='running' AND r.control='' AND r.execution_generation=? AND c.version=? AND c.request_limit>0
AND ?=(SELECT coalesce(max(request_number),0)+1 FROM run_read_rounds WHERE run_id=r.id))`,
		runID, executionGeneration(ctx), readCheckpointVersion, number).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrState
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO run_read_rounds(run_id,request_number,protocol,output,output_digest) VALUES(?,?,?,?,?)`,
		runID, number, turn.Protocol, string(turn.Output), readTurnDigest(turn)); err != nil {
		return err
	}
	for _, read := range planned {
		args, err := json.Marshal(read.request.Arguments)
		if err != nil {
			return err
		}
		if err := rememberConnectionReference(ctx, tx, read.request.ConnectionID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO run_reads(run_id,position,connection_id,connection_revision,connection_name,tool_name,tool_title,tool_version,arguments)
VALUES(?,?,nullif(?,''),?,?,?,?,?,?)`, runID, read.Position, read.request.ConnectionID, read.request.Revision, read.ConnectionName, read.ToolName, read.ToolTitle, read.request.ToolVersion, string(args)); err != nil {
			return err
		}
	}
	if len(planned) > 0 {
		// Any streamed prose preceding a tool call was interim, not a deliverable.
		if _, err = tx.ExecContext(ctx, "UPDATE runs SET response='' WHERE id=?", runID); err != nil {
			return err
		}
		if err = addEvent(tx, runID, "message.reset", map[string]int{"execution_generation": executionGeneration(ctx)}); err != nil {
			return err
		}
		if err = addEvent(tx, runID, "tool.read_changed", map[string]string{"status": "queued"}); err != nil {
			return err
		}
	}
	for order, call := range turn.Calls {
		if call.CallID == "" || len(call.CallID) > 256 {
			return ErrReadModelCall
		}
		updated, err := tx.ExecContext(ctx, `UPDATE run_reads SET model_round=?,model_call_id=?,model_order=?
WHERE run_id=? AND position=? AND status='queued' AND model_round IS NULL`, number, call.CallID, order+1, runID, call.Position)
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrReadModelCall
		}
	}
	if err = recordModelUsage(tx, runID, usage); err != nil {
		return err
	}
	if err = settleReadRequest(tx, runID, usage); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) restoreReadRounds(ctx context.Context, runID, protocol string, reads []TaskRead) ([]TaskReadTurn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT request_number,protocol,output,output_digest
FROM run_read_rounds WHERE run_id=? ORDER BY request_number`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rounds []TaskReadTurn
	var checksums []string
	for rows.Next() {
		var number int
		var turn TaskReadTurn
		var output, checksum string
		if err = rows.Scan(&number, &turn.Protocol, &output, &checksum); err != nil {
			return nil, err
		}
		if number != len(rounds)+1 || turn.Protocol != protocol || !json.Valid([]byte(output)) {
			return nil, ErrReadCheckpoint
		}
		turn.Output = json.RawMessage(output)
		rounds = append(rounds, turn)
		checksums = append(checksums, checksum)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	ordered := slices.Clone(reads)
	slices.SortFunc(ordered, func(a, b TaskRead) int { return a.modelOrder - b.modelOrder })
	for _, read := range ordered {
		if read.modelRound == 0 {
			if read.Status != "queued" || read.modelOrder != 0 || read.modelCallID != "" {
				return nil, ErrReadCheckpoint
			}
			continue
		}
		if read.modelRound > len(rounds) || read.modelRound < 1 || read.modelCallID == "" ||
			(read.Status == "queued" && read.modelRound != len(rounds)) {
			return nil, ErrReadCheckpoint
		}
		turn := &rounds[read.modelRound-1]
		if read.modelOrder != len(turn.Calls)+1 {
			return nil, ErrReadCheckpoint
		}
		switch read.Status {
		case "completed", "failed", "tool_error", "queued":
		case "unknown":
			if !read.Acknowledged {
				return nil, ErrReadUnconfirmed
			}
		default:
			return nil, ErrReadCheckpoint
		}
		if read.Status == "completed" && read.Result == nil {
			return nil, ErrReadCheckpoint
		}
		turn.Calls = append(turn.Calls, TaskReadInvocation{Position: read.Position, CallID: read.modelCallID})
	}
	for i, turn := range rounds {
		if len(turn.Calls) == 0 || readTurnDigest(turn) != checksums[i] {
			return nil, ErrReadCheckpoint
		}
	}
	return rounds, nil
}

func readTurnDigest(turn TaskReadTurn) string {
	calls, _ := json.Marshal(turn.Calls)
	return digest(string(turn.Output) + "\x00" + string(calls))
}

func (s *Store) streamWithTaskReads(ctx context.Context, runID string, started time.Time, model MultiTaskReadModel,
	input []Message, available []TaskRead, remaining int, limits RunLimits, emit func(context.Context, string) error) (TaskAnswer, error) {
	single := len(available) == 1
	_, protocol := model.ReadCheckpointIdentity()
	saved, err := s.restoreReadRounds(ctx, runID, protocol, available)
	if err != nil {
		return TaskAnswer{}, err
	}
	unplanned := 0
	for _, read := range available {
		if read.modelRound == 0 {
			unplanned++
		}
	}
	if remaining < 16*(unplanned+1) {
		return TaskAnswer{}, ErrOutputBudget
	}
	var history []TaskReadRound
	successful := false
	completeRound := func(turn TaskReadTurn) error {
		round := TaskReadRound{TaskReadTurn: turn, Results: make([]TaskRead, 0, len(turn.Calls))}
		for _, call := range turn.Calls {
			index := slices.IndexFunc(available, func(read TaskRead) bool { return read.Position == call.Position })
			if index < 0 {
				return ErrReadCheckpoint
			}
			read := &available[index]
			if read.Status == "queued" {
				var err error
				read, err = s.runTaskReadAt(ctx, runID, call.Position)
				if err != nil && (!errors.Is(err, errReadFailed) || read == nil || read.Status == "unknown") {
					return err
				}
			}
			successful = successful || read.Status == "completed"
			round.Results = append(round.Results, *read)
			available = slices.Delete(available, index, index+1)
		}
		history = append(history, round)
		return nil
	}
	for _, turn := range saved {
		if err = completeRound(turn); err != nil {
			return TaskAnswer{}, err
		}
	}
	for number := len(saved) + 1; ; number++ {
		answer := len(available) == 0
		if answer && !successful {
			return TaskAnswer{}, errReadFailed
		}
		limit := remaining
		if answer && single {
			limit = recoverableAnswerLimit(remaining, executionGeneration(ctx))
		} else if !answer {
			// Keep enough output budget for each remaining selection and the final answer.
			limit = min(256, remaining/(len(available)+1))
		}
		if limit < 16 {
			return TaskAnswer{}, ErrOutputBudget
		}
		if err = s.reserveReadRequest(ctx, runID, limit, time.Since(started)); err != nil {
			return TaskAnswer{}, err
		}
		if err = s.checkTaskReadModelAuthority(ctx, runID); err != nil {
			return TaskAnswer{}, err
		}
		modelCtx, cancel := context.WithTimeoutCause(ctx, limits.ModelTimeout, errModelTimeout)
		var emitAnswer func(string) error
		if answer {
			emitAnswer = func(text string) error { return emit(modelCtx, text) }
		}
		turn, usage, err := model.TaskReadTurn(modelCtx, input, available, history, limit, emitAnswer)
		if cause := context.Cause(modelCtx); cause != nil {
			err = cause
		}
		cancel()
		if err != nil {
			return TaskAnswer{Usage: usage}, err
		}
		charged := limit
		if usage != nil && usage.OutputTokens != nil {
			if *usage.OutputTokens > int64(limit) {
				return TaskAnswer{Usage: usage}, ErrOutputBudget
			}
			charged = int(*usage.OutputTokens)
		}
		if answer {
			if len(turn.Calls) != 0 {
				return TaskAnswer{Usage: usage}, ErrReadModelCall
			}
			return TaskAnswer{Question: turn.Question, Usage: usage}, nil
		}
		if turn.Question != "" {
			return TaskAnswer{Usage: usage}, ErrReadModelCall
		}
		if turn.Protocol != protocol {
			return TaskAnswer{Usage: usage}, ErrReadCheckpoint
		}
		if err = s.saveReadRound(ctx, runID, number, turn, usage); err != nil {
			return TaskAnswer{Usage: usage}, err
		}
		remaining -= charged
		if err = completeRound(turn); err != nil {
			return TaskAnswer{}, err
		}
	}
}
