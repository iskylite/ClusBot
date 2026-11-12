package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"clusbot/internal/mcpclient"
)

type TaskToolDefinition struct {
	Name, Description string
	Parameters        json.RawMessage
}

type TaskToolCall struct {
	Name, ID  string
	Arguments map[string]any
}

type TaskToolTurn struct {
	TaskAnswer
	Output json.RawMessage
	Calls  []TaskToolCall
}

type automaticTaskToolModel interface {
	TaskToolTurn(context.Context, []Message, []TaskToolDefinition, []TaskReadRound, int, func(string) error) (TaskToolTurn, error)
}

type taskTool struct {
	definition TaskToolDefinition
	source     TaskRead
}

const automaticReadProtocol = "responses/automatic-tools/v1"

var workspaceTools = []taskTool{
	{
		definition: TaskToolDefinition{"workspace_list", "Find text or Markdown material by filename inside materials/. Use an empty query to list available files.", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","maxLength":160}},"required":["query"],"additionalProperties":false}`)},
		source:     TaskRead{ConnectionName: "工作区", ToolName: "workspace_list", ToolTitle: "查找材料"},
	},
	{
		definition: TaskToolDefinition{"workspace_read", "Read a UTF-8 text or Markdown file listed in materials/, using its full materials/ path.", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","maxLength":200}},"required":["path"],"additionalProperties":false}`)},
		source:     TaskRead{ConnectionName: "工作区", ToolName: "workspace_read", ToolTitle: "读取材料"},
	},
	{
		definition: TaskToolDefinition{"workspace_search", "Search text inside materials/ for a nonempty literal keyword. Returns matching paths and short excerpts; use workspace_read for full content.", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":160}},"required":["query"],"additionalProperties":false}`)},
		source:     TaskRead{ConnectionName: "工作区", ToolName: "workspace_search", ToolTitle: "搜索材料内容"},
	},
}

func (s *Store) automaticTaskTools(ctx context.Context) ([]taskTool, error) {
	options, err := s.TaskTools(ctx)
	if err != nil {
		return nil, err
	}
	var tools []taskTool
	if s.materials != nil {
		tools = append(tools, workspaceTools...)
	}
	for _, option := range options {
		tool := option.Tool
		alias := "mcp_" + digest(option.ConnectionID + "\x00" + tool.Name)[:48]
		tools = append(tools, taskTool{
			definition: TaskToolDefinition{
				Name: alias, Parameters: tool.InputSchema,
				Description: fmt.Sprintf("%s / %s. %s", option.ConnectionName, tool.Name, tool.Description),
			},
			source: TaskRead{ConnectionName: option.ConnectionName, ToolName: tool.Name, ToolTitle: tool.Title,
				request: TaskReadRequest{ConnectionID: option.ConnectionID, Revision: option.Revision, ToolName: tool.Name, ToolVersion: tool.Version}},
		})
	}
	return tools, nil
}

func (s *Store) streamWithAutomaticTools(ctx context.Context, runID string, started time.Time, model automaticTaskToolModel,
	input []Message, tools []taskTool, reads []TaskRead, remaining int, limits RunLimits, emit func(context.Context, string) error) (TaskAnswer, error) {
	saved, err := s.restoreReadRounds(ctx, runID, automaticReadProtocol, reads)
	if err != nil {
		return TaskAnswer{}, err
	}
	var history []TaskReadRound
	completeRound := func(turn TaskReadTurn) error {
		round := TaskReadRound{TaskReadTurn: turn}
		for _, call := range turn.Calls {
			if call.Position < 1 || call.Position > len(reads) || reads[call.Position-1].Position != call.Position {
				return ErrReadCheckpoint
			}
			read := &reads[call.Position-1]
			if read.Status == "queued" {
				result, err := s.runTaskReadAt(ctx, runID, call.Position)
				if err != nil && (!errors.Is(err, errReadFailed) || result == nil || result.Status == "unknown") {
					return err
				}
				*read = *result
			}
			round.Results = append(round.Results, *read)
		}
		history = append(history, round)
		return nil
	}
	for _, turn := range saved {
		if err = completeRound(turn); err != nil {
			return TaskAnswer{}, err
		}
	}
	definitions := make([]TaskToolDefinition, 0, len(tools))
	sources := make(map[string]TaskRead, len(tools))
	for _, tool := range tools {
		definitions = append(definitions, tool.definition)
		sources[tool.definition.Name] = tool.source
	}

	for {
		limit := remaining
		available := definitions
		if len(reads) >= MaxTaskReads || remaining < 32 {
			available = nil
		} else if len(available) > 0 {
			limit = recoverableAnswerLimit(remaining, executionGeneration(ctx))
		}
		if limit < 16 {
			return TaskAnswer{}, ErrOutputBudget
		}
		if err = s.reserveReadRequest(ctx, runID, limit, time.Since(started)); err != nil {
			return TaskAnswer{}, err
		}
		modelCtx, cancel := context.WithTimeoutCause(ctx, limits.ModelTimeout, errModelTimeout)
		turn, err := model.TaskToolTurn(modelCtx, input, available, history, limit, func(text string) error {
			return emit(modelCtx, text)
		})
		if cause := context.Cause(modelCtx); cause != nil {
			err = cause
		}
		cancel()
		if err != nil {
			return turn.TaskAnswer, err
		}
		charged := limit
		if turn.Usage != nil && turn.Usage.OutputTokens != nil {
			if *turn.Usage.OutputTokens > int64(limit) {
				return turn.TaskAnswer, ErrOutputBudget
			}
			charged = int(*turn.Usage.OutputTokens)
		}
		if len(turn.Calls) == 0 {
			return turn.TaskAnswer, nil
		}
		if len(available) == 0 || len(reads)+len(turn.Calls) > MaxTaskReads || turn.Question != "" {
			return turn.TaskAnswer, ErrReadModelCall
		}
		round := TaskReadTurn{Protocol: automaticReadProtocol, Output: turn.Output}
		planned := make([]TaskRead, 0, len(turn.Calls))
		for _, call := range turn.Calls {
			read, ok := sources[call.Name]
			args, jsonErr := json.Marshal(call.Arguments)
			if !ok || call.Arguments == nil || jsonErr != nil || len(args) > 24*1024 {
				return turn.TaskAnswer, ErrReadModelCall
			}
			read.Position, read.Status = len(reads)+len(planned)+1, "queued"
			read.request.Arguments = call.Arguments
			round.Calls = append(round.Calls, TaskReadInvocation{Position: read.Position, CallID: call.ID})
			planned = append(planned, read)
		}
		if err = s.saveReadRound(ctx, runID, len(history)+1, round, turn.Usage, planned...); err != nil {
			return turn.TaskAnswer, err
		}
		reads = append(reads, planned...)
		remaining -= charged
		if err = completeRound(round); err != nil {
			return TaskAnswer{}, err
		}
	}
}

func (s *Store) runWorkspaceTool(ctx context.Context, runID string, read *TaskRead) (*TaskRead, error) {
	var definition TaskToolDefinition
	for _, tool := range workspaceTools {
		if tool.definition.Name == read.ToolName {
			definition = tool.definition
			break
		}
	}
	args := read.request.Arguments
	err := mcpclient.ValidateArguments(mcpclient.Tool{InputSchema: definition.Parameters}, args)
	var result mcpclient.Result
	if err == nil {
		result, err = s.workspaceToolResult(ctx, read.ToolName, args)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	read.Status = "completed"
	if err != nil {
		read.Status, read.Detail = "tool_error", "无法读取该工作区材料，请检查文件名或查询参数"
		result = mcpclient.Result{Text: read.Detail, IsError: true}
	}
	read.ReceivedAt, read.Result = now(), &result
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if err = s.finishTaskRead(runID, read.Position, executionGeneration(ctx), read.Status, read.Detail, read.ReceivedAt, string(raw)); err != nil {
		return nil, err
	}
	return read, nil
}

func (s *Store) workspaceToolResult(ctx context.Context, name string, args map[string]any) (mcpclient.Result, error) {
	switch name {
	case "workspace_read":
		path, _ := args["path"].(string)
		body, err := s.readMaterial(path)
		if err != nil {
			return mcpclient.Result{}, err
		}
		metadata, err := json.Marshal(map[string]string{"path": path, "sha256": digest(string(body))})
		return mcpclient.Result{Text: string(body), Structured: metadata}, err
	case "workspace_list":
		query, _ := args["query"].(string)
		files, err := s.MaterialFiles(query)
		if err != nil {
			return mcpclient.Result{}, err
		}
		raw, err := json.Marshal(files)
		return mcpclient.Result{Structured: raw}, err
	case "workspace_search":
		query, _ := args["query"].(string)
		query = strings.TrimSpace(query)
		if query == "" {
			return mcpclient.Result{}, mcpclient.ErrArguments
		}
		files, err := s.MaterialFiles("")
		if err != nil {
			return mcpclient.Result{}, err
		}
		type match struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Text string `json:"text"`
		}
		const maxMatches, excerptLength = 20, 160
		matches := make([]match, 0, maxMatches)
		for _, file := range files {
			if err = ctx.Err(); err != nil {
				return mcpclient.Result{}, err
			}
			body, err := s.readMaterial(file.Path)
			if err != nil {
				continue
			}
			for index, line := range strings.Split(string(body), "\n") {
				at := strings.Index(line, query)
				if at < 0 {
					continue
				}
				if len(matches) == maxMatches {
					raw, err := json.Marshal(matches)
					return mcpclient.Result{Structured: raw, Truncated: true}, err
				}
				snippet := []rune(line)
				start := max(0, utf8.RuneCountInString(line[:at+len(query)])-excerptLength)
				end := min(start+excerptLength, len(snippet))
				matches = append(matches, match{file.Path, index + 1, string(snippet[start:end])})
			}
		}
		raw, err := json.Marshal(matches)
		return mcpclient.Result{Structured: raw}, err
	default:
		return mcpclient.Result{}, mcpclient.ErrArguments
	}
}
