package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"clusbot/internal/mcpclient"
)

const MaxTaskReads = 8

var (
	ErrReadNeedsSelection = errors.New("请在补充要求中重新选择读取工具和参数")
	ErrReadSelection      = errors.New("每轮最多选择 8 个读取工具，请使用一种提交格式")
	errReadFailed         = errors.New("事项读取工具未完成")
)

type TaskReadRequest struct {
	ConnectionID string         `json:"connection_id"`
	Revision     int            `json:"revision"`
	ToolName     string         `json:"tool_name"`
	ToolVersion  string         `json:"tool_version"`
	Arguments    map[string]any `json:"arguments"`
}

type TaskRead struct {
	Position          int               `json:"position"`
	ConnectionName    string            `json:"connection_name"`
	ToolName          string            `json:"tool_name"`
	ToolTitle         string            `json:"tool_title,omitempty"`
	Status            string            `json:"status"`
	Detail            string            `json:"detail,omitempty"`
	ReceivedAt        int64             `json:"received_at,omitempty"`
	Result            *mcpclient.Result `json:"result,omitempty"`
	SourceUnavailable bool              `json:"source_unavailable,omitempty"`
	Acknowledged      bool              `json:"acknowledged,omitempty"`
	SourceRunID       string            `json:"source_run_id,omitempty"`
	request           TaskReadRequest
	modelRound        int
	modelOrder        int
	modelCallID       string
}

type TaskToolOption struct {
	ConnectionID   string         `json:"connection_id"`
	ConnectionName string         `json:"connection_name"`
	Revision       int            `json:"revision"`
	Tool           mcpclient.Tool `json:"tool"`
}

func (s *Store) TaskTools(ctx context.Context) ([]TaskToolOption, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.name,c.revision,t.definition FROM connections c
JOIN connection_tools t ON t.connection_id=c.id WHERE c.enabled=1 AND c.status='ready' AND t.allowed=1 AND t.read_only=1 ORDER BY c.name,t.name LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	options := []TaskToolOption{}
	for rows.Next() {
		var option TaskToolOption
		var definition string
		if err = rows.Scan(&option.ConnectionID, &option.ConnectionName, &option.Revision, &definition); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(definition), &option.Tool); err != nil {
			return nil, err
		}
		if s.cluskitBenchmarkToolReserved(option.Tool.Name) || isToolDisallowedForReadOnly(option.Tool) || mcpclient.ValidateToolSchemas(option.Tool) != nil {
			continue
		}
		option.Tool.Allowed = true
		option.Tool.ReadOnly = true
		options = append(options, option)
	}
	return options, rows.Err()
}

func recordTaskRead(ctx context.Context, tx *sql.Tx, run Run, position int, request TaskReadRequest) error {
	var c Connection
	var raw string
	var tool mcpclient.Tool
	var allowed, readOnly bool
	err := tx.QueryRowContext(ctx, `SELECT c.name,c.revision,t.definition,t.allowed,t.read_only FROM connections c JOIN connection_tools t ON t.connection_id=c.id
WHERE c.id=? AND c.enabled=1 AND c.status='ready' AND t.name=?`, request.ConnectionID, request.ToolName).Scan(&c.Name, &c.Revision, &raw, &allowed, &readOnly)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!allowed || !readOnly || isKnownUnsafeWriteTool(request.ToolName))) {
		return ErrToolPermission
	}
	if err != nil {
		return err
	}
	if json.Unmarshal([]byte(raw), &tool) == nil && isToolDisallowedForReadOnly(tool) {
		return ErrToolPermission
	}
	if c.Revision != request.Revision {
		return ErrConnectionRevision
	}
	if err = json.Unmarshal([]byte(raw), &tool); err != nil {
		return err
	}
	if tool.Version != request.ToolVersion {
		return mcpclient.ErrSchemaChanged
	}
	args, err := json.Marshal(request.Arguments)
	if err != nil || len(args) > 24*1024 {
		return mcpclient.ErrArguments
	}
	if err = mcpclient.ValidateArguments(tool, request.Arguments); err != nil {
		return err
	}
	if err := rememberConnectionReference(ctx, tx, request.ConnectionID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO run_reads(run_id,position,connection_id,connection_revision,connection_name,tool_name,tool_title,tool_version,arguments)
VALUES(?,?,?,?,?,?,?,?,?)`, run.ID, position, request.ConnectionID, request.Revision, c.Name, tool.Name, tool.Title, tool.Version, string(args))
	return err
}

const taskReadColumns = `position,coalesce(connection_id,''),connection_revision,connection_name,tool_name,tool_title,tool_version,arguments,status,detail,received_at,result,
coalesce(model_round,0),model_order,coalesce(model_call_id,''),acknowledged_at<>0`

func authorizedTaskReadColumns() string {
	return taskReadColumns + `,connection_id IS NOT NULL AND NOT ` + exactMCPAuthority("run_reads")
}

func scanTaskRead(row scanner) (TaskRead, error) {
	var read TaskRead
	var args, result string
	err := row.Scan(&read.Position, &read.request.ConnectionID, &read.request.Revision, &read.ConnectionName, &read.ToolName, &read.ToolTitle,
		&read.request.ToolVersion, &args, &read.Status, &read.Detail, &read.ReceivedAt, &result,
		&read.modelRound, &read.modelOrder, &read.modelCallID, &read.Acknowledged, &read.SourceUnavailable)
	if err != nil {
		return read, err
	}
	read.request.ToolName = read.ToolName
	if read.SourceUnavailable {
		read.Detail = "原读取来源或授权已变更，结果不可查看"
		return read, nil
	}
	if err = json.Unmarshal([]byte(args), &read.request.Arguments); err != nil {
		return read, err
	}
	if result != "" {
		err = json.Unmarshal([]byte(result), &read.Result)
	}
	return read, err
}

func reuseTaskRead(tx *sql.Tx, taskID, runID string, requirePrior bool) error {
	var previous, previousStatus, failureRunID string
	var incomplete bool
	err := tx.QueryRow(`SELECT r.id,r.status,coalesce(r.read_failure_run_id,''),
EXISTS(SELECT 1 FROM run_reads WHERE run_id=r.id AND status NOT IN ('completed','reused'))
FROM runs r WHERE r.task_id=? AND r.id<>? ORDER BY r.rowid DESC LIMIT 1`, taskID, runID).
		Scan(&previous, &previousStatus, &failureRunID, &incomplete)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if incomplete && requirePrior {
		return ErrReadNeedsSelection
	}
	if previousStatus == "input_required" && incomplete {
		failureRunID = previous
	}
	if failureRunID != "" {
		if _, err = tx.Exec("UPDATE runs SET read_failure_run_id=? WHERE id=?", failureRunID, runID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO run_reads(run_id,position,connection_id,connection_revision,connection_name,tool_name,tool_title,tool_version,arguments,status,call_id,received_at,result)
SELECT ?,CASE WHEN ? THEN position ELSE row_number() OVER (ORDER BY position) END,
connection_id,connection_revision,connection_name,tool_name,tool_title,tool_version,
CASE WHEN connection_id IS NULL OR `+exactMCPAuthority("run_reads")+` THEN arguments ELSE '{}' END,
'reused',call_id,received_at,
CASE WHEN connection_id IS NULL OR `+exactMCPAuthority("run_reads")+` THEN result ELSE '' END
FROM run_reads WHERE run_id=? AND status IN ('completed','reused')
AND NOT EXISTS(SELECT 1 FROM run_read_checkpoints WHERE run_id=run_reads.run_id AND automatic_tools=1)`, runID, failureRunID != "", previous)
	return err
}

type taskReadQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func listTaskReads(ctx context.Context, q taskReadQuerier, runID string) ([]TaskRead, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+authorizedTaskReadColumns()+" FROM run_reads WHERE run_id=? ORDER BY position", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reads []TaskRead
	for rows.Next() {
		read, err := scanTaskRead(rows)
		if err != nil {
			return nil, err
		}
		reads = append(reads, read)
	}
	return reads, rows.Err()
}

// revokedMCPReply conservatively classifies direct Run outputs. A returned
// source may have informed the answer; this does not infer sentence-level or
// downstream dependencies. runID is an internal SQL expression.
func revokedMCPReply(runID string) string {
	return `EXISTS(SELECT 1 FROM runs reply JOIN run_reads rr ON rr.run_id IN (reply.id, reply.read_failure_run_id)
WHERE reply.id=` + runID + ` AND rr.connection_id IS NOT NULL
AND (rr.run_id=reply.id AND rr.status IN ('completed','reused','tool_error')
OR rr.run_id=reply.read_failure_run_id AND rr.status='tool_error'
AND EXISTS(SELECT 1 FROM runs prev WHERE prev.id=rr.run_id AND prev.task_id=reply.task_id))
AND (rr.result<>'' OR rr.call_id IS NOT NULL) AND NOT ` + exactMCPAuthority("rr") + `)`
}

func checkMCPReplyAvailable(tx *sql.Tx, runID string) error {
	var unavailable bool
	err := tx.QueryRow("SELECT "+revokedMCPReply("?"), runID).Scan(&unavailable)
	if err == nil && unavailable {
		return ErrReadNeedsSelection
	}
	return err
}

// Recheck persisted source identity immediately before building an outbound
// model request. A completed/reused result is not new authorization to send it.
func (s *Store) checkTaskReadModelAuthority(ctx context.Context, runID string) error {
	var revoked bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run_reads rr
JOIN runs r ON r.id=?
WHERE rr.connection_id IS NOT NULL
AND (rr.run_id=r.id OR (rr.run_id=r.read_failure_run_id AND rr.status NOT IN ('completed','reused')
AND EXISTS(SELECT 1 FROM runs prev WHERE prev.id=rr.run_id AND prev.task_id=r.task_id)))
AND NOT `+exactMCPAuthority("rr")+`)`, runID).Scan(&revoked)
	if err == nil && revoked {
		return ErrReadNeedsSelection
	}
	return err
}

func listTaskReadEvidence(ctx context.Context, q taskReadQuerier, runID string) ([]TaskRead, error) {
	reads, err := listTaskReads(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	return includeEarlierReadFailures(ctx, q, runID, reads)
}

func includeEarlierReadFailures(ctx context.Context, q taskReadQuerier, runID string, reads []TaskRead) ([]TaskRead, error) {
	var source sql.NullString
	err := q.QueryRowContext(ctx, `SELECT original.id FROM runs r
LEFT JOIN runs original ON original.id=r.read_failure_run_id AND original.task_id=r.task_id
WHERE r.id=?`, runID).Scan(&source)
	if err != nil || !source.Valid {
		return reads, err
	}
	original, err := listTaskReads(ctx, q, source.String)
	if err != nil {
		return nil, err
	}
	for _, read := range original {
		if read.Status == "completed" || read.Status == "reused" {
			continue
		}
		read.SourceRunID = source.String
		reads = append(reads, read)
	}
	slices.SortFunc(reads, func(a, b TaskRead) int { return a.Position - b.Position })
	return reads, nil
}

func (s *Store) taskRead(ctx context.Context, runID string) (*TaskRead, error) {
	return s.taskReadAt(ctx, runID, 1)
}

func (s *Store) taskReadAt(ctx context.Context, runID string, position int) (*TaskRead, error) {
	read, err := scanTaskRead(s.db.QueryRowContext(ctx, "SELECT "+authorizedTaskReadColumns()+" FROM run_reads WHERE run_id=? AND position=?", runID, position))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &read, nil
}

func (s *Store) runTaskRead(ctx context.Context, runID string) (*TaskRead, error) {
	return s.runTaskReadAt(ctx, runID, 1)
}

func (s *Store) readTaskSources(ctx context.Context, runID string, selected []TaskRead) ([]TaskRead, error) {
	reads := make([]TaskRead, 0, len(selected))
	successful := false
	for _, source := range selected {
		read, err := s.runTaskReadAt(ctx, runID, source.Position)
		if err != nil && (!errors.Is(err, errReadFailed) || read == nil || read.Status == "unknown") {
			return nil, err
		}
		successful = successful || read.Status == "completed"
		reads = append(reads, *read)
	}
	if !successful {
		return nil, errReadFailed
	}
	return reads, nil
}

func (s *Store) runTaskReadAt(ctx context.Context, runID string, position int) (*TaskRead, error) {
	read, err := s.taskReadAt(ctx, runID, position)
	if err != nil {
		return nil, err
	}
	if read == nil {
		return nil, ErrState
	}
	if read.SourceUnavailable && read.Status != "queued" {
		return read, ErrToolPermission
	}
	if read.Status == "reused" {
		return read, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE run_reads SET status='checking' WHERE run_id=? AND position=? AND status='queued'
AND EXISTS(SELECT 1 FROM runs WHERE id=? AND status='running' AND control='' AND execution_generation=?)`, runID, position, runID, executionGeneration(ctx))
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrState
	}
	if read.request.ConnectionID == "" {
		return s.runWorkspaceTool(ctx, runID, read)
	}
	call, callErr := s.callConnectionTool(ctx, read.request.ConnectionID, read.ToolName, read.request.Revision, read.request.Arguments, runID, position)
	status, detail := "completed", ""
	var raw string
	switch {
	case callErr != nil:
		status, detail = "failed", taskReadError(callErr)
		if call.ID != "" {
			status = "unknown"
		}
	case call.IsError:
		status, detail = "tool_error", "工具返回错误，请查看读取记录"
		if call.ValidationError != "" {
			detail = call.ValidationError
		}
	}
	if callErr == nil {
		read.Result = &call.Result
		encoded, err := json.Marshal(call.Result)
		if err != nil {
			return nil, err
		}
		raw = string(encoded)
	}
	if err = s.finishTaskRead(runID, position, executionGeneration(ctx), status, detail, call.ReceivedAt, raw); err != nil {
		return nil, err
	}
	read.Status, read.Detail, read.ReceivedAt = status, detail, call.ReceivedAt
	if status != "completed" {
		return read, errReadFailed
	}
	return read, nil
}

func (s *Store) finishTaskRead(runID string, position, generation int, status, detail string, receivedAt int64, result string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updated, err := tx.Exec(`UPDATE run_reads SET status=?,detail=?,received_at=?,result=?
WHERE run_id=? AND position=? AND status IN ('checking','dispatched')
AND EXISTS(SELECT 1 FROM runs r WHERE r.id=run_reads.run_id AND r.status='running' AND r.execution_generation=?)`,
		status, detail, receivedAt, result, runID, position, generation)
	if err != nil {
		return err
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	if err = addEvent(tx, runID, "tool.read_changed", map[string]any{"status": status, "position": position}); err != nil {
		return err
	}
	return tx.Commit()
}

func taskReadError(err error) string {
	switch {
	case errors.Is(err, ErrToolPermission), errors.Is(err, ErrConnectionRevision), errors.Is(err, ErrConnectionUnavailable),
		errors.Is(err, mcpclient.ErrSchemaChanged), errors.Is(err, mcpclient.ErrCredentials), errors.Is(err, mcpclient.ErrArguments),
		errors.Is(err, mcpclient.ErrConnect), errors.Is(err, mcpclient.ErrDiscovery), errors.Is(err, mcpclient.ErrCall), errors.Is(err, mcpclient.ErrToolInput):
		return err.Error()
	default:
		return "读取未完成，请检查连接后重新提交"
	}
}
