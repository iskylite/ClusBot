package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"clusbot/internal/work"
)

func (p *Provider) readProtocol() string { return p.protocol + "/v1" }

func (p *Provider) ReadCheckpointIdentity() (string, string) {
	identity := fmt.Sprintf("%s\n%s\n%d\n%s\n%s", p.endpoint, p.model, p.configRevision, p.credentialBinding, p.readProtocol())
	if limit := p.requestByteLimit(); limit != DefaultMaxRequestBytes {
		identity += fmt.Sprintf("\nmax_request_bytes=%d", limit)
	}
	if p.checkpointScope != "" {
		identity += "\n" + p.checkpointScope
	}
	return identity, p.readProtocol()
}

func (p *Provider) TaskReadTurn(ctx context.Context, input []work.Message, available []work.TaskRead,
	history []work.TaskReadRound, limit int, emit func(string) error) (work.TaskReadTurn, *work.TokenUsage, error) {
	if len(history) == 0 && len(available) == 1 {
		call, usage, err := p.PlanTaskRead(ctx, input, limit)
		if err != nil {
			return work.TaskReadTurn{}, usage, err
		}
		return work.TaskReadTurn{Protocol: call.Protocol, Output: call.Output,
			Calls: []work.TaskReadInvocation{{Position: available[0].Position, CallID: call.CallID}}}, usage, nil
	}
	if len(history) == 1 && len(history[0].Calls) == 1 && len(available) == 0 {
		round := history[0]
		if len(round.Results) != 1 || round.Results[0].Position != round.Calls[0].Position {
			return work.TaskReadTurn{}, nil, work.ErrReadModelCall
		}
		call := work.TaskReadCall{Protocol: round.Protocol, CallID: round.Calls[0].CallID, Output: round.Output}
		answer, err := p.CompleteTaskRead(ctx, input, call, &round.Results[0], limit, emit)
		return work.TaskReadTurn{Question: answer.Question}, answer.Usage, err
	}
	request, err := conversationRequest(input, true, limit)
	if err != nil {
		return work.TaskReadTurn{}, nil, err
	}
	request.Include = []string{"reasoning.encrypted_content"}
	request.Instructions += " 本轮用户选定多个只读来源，应用已固定实际参数。逐个或同批请求剩余来源，每个来源最多一次，函数参数一律为 {}。所有读取返回后再交付。未成功的读取应明确说明，不能当作已验证的事实。"
	usedIDs := make(map[string]bool)
	for _, round := range history {
		var output []json.RawMessage
		if round.Protocol != p.readProtocol() || json.Unmarshal(round.Output, &output) != nil || len(round.Calls) != len(round.Results) {
			return work.TaskReadTurn{}, nil, work.ErrReadModelCall
		}
		for _, item := range output {
			request.Input = append(request.Input, item)
		}
		for i, call := range round.Calls {
			result := round.Results[i]
			if call.Position != result.Position || usedIDs[call.CallID] {
				return work.TaskReadTurn{}, nil, work.ErrReadModelCall
			}
			usedIDs[call.CallID] = true
			evidence, err := json.Marshal(result)
			if err != nil {
				return work.TaskReadTurn{}, nil, err
			}
			request.Input = append(request.Input, map[string]string{"type": "function_call_output", "call_id": call.CallID, "output": string(evidence)})
		}
	}
	if len(available) == 0 {
		answer, err := p.requestTaskAnswer(ctx, request, emit)
		return work.TaskReadTurn{Question: answer.Question}, answer.Usage, err
	}
	positions := make(map[string]int, len(available))
	for _, read := range available {
		definition := selectedReadDefinition(read)
		positions[definition.Name] = read.Position
		request.Tools = append(request.Tools, definition)
	}
	request.ToolChoice = "required"
	result, err := p.requestStream(ctx, request, emit)
	usage := normalizedUsage(result.Usage)
	if err != nil {
		return work.TaskReadTurn{}, usage, err
	}
	calls, err := selectedReadCalls(result.Output, positions, usedIDs)
	if err != nil || len(calls) == 0 {
		return work.TaskReadTurn{}, usage, work.ErrReadModelCall
	}
	output, err := json.Marshal(result.Output)
	if err != nil {
		return work.TaskReadTurn{}, usage, err
	}
	return work.TaskReadTurn{Protocol: p.readProtocol(), Output: output, Calls: calls}, usage, nil
}

func selectedReadDefinition(read work.TaskRead) responseFunction {
	definition := taskReadDefinition()
	definition.Name = fmt.Sprintf("%s_%d", taskReadFunction, read.Position)
	title := read.ToolTitle
	if title == "" {
		title = read.ToolName
	}
	definition.Description = fmt.Sprintf("Read user-selected source %d (%q / %q) with its fixed arguments.", read.Position, read.ConnectionName, title)
	return definition
}

func selectedReadCalls(output []json.RawMessage, positions map[string]int, usedIDs map[string]bool) ([]work.TaskReadInvocation, error) {
	var calls []work.TaskReadInvocation
	selected := make(map[int]bool)
	for _, raw := range output {
		var item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return nil, work.ErrReadModelCall
		}
		switch item.Type {
		case "reasoning", "message":
			continue
		case "function_call":
			position := positions[item.Name]
			var args map[string]json.RawMessage
			if position == 0 || selected[position] || usedIDs[item.CallID] || strings.TrimSpace(item.CallID) == "" || len(item.CallID) > 256 ||
				(item.Status != "" && item.Status != "completed") ||
				json.Unmarshal([]byte(item.Arguments), &args) != nil || args == nil || len(args) != 0 {
				return nil, work.ErrReadModelCall
			}
			selected[position], usedIDs[item.CallID] = true, true
			calls = append(calls, work.TaskReadInvocation{Position: position, CallID: item.CallID})
		default:
			return nil, work.ErrReadModelCall
		}
	}
	return calls, nil
}
