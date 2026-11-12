package model

import (
	"context"
	"encoding/json"

	"clusbot/internal/work"
)

const (
	taskReadFunction = "read_selected_source"
	taskReadProtocol = "responses/v1"
)

func (p *Responses) PlanTaskRead(ctx context.Context, messages []work.Message, limit int) (work.TaskReadCall, *work.TokenUsage, error) {
	request, err := conversationRequest(messages, true, limit)
	if err != nil {
		return work.TaskReadCall{}, nil, err
	}
	request.Instructions += " 本轮用户已选定一个只读数据来源。先调用 read_selected_source 获取这次读取的结果，然后再写成果。工具的来源和参数由用户确定，调用不接受参数。"
	request.Tools = []responseFunction{taskReadDefinition()}
	request.ToolChoice = "required"
	request.Include = []string{"reasoning.encrypted_content"}
	result, err := p.requestStream(ctx, request, nil)
	usage := normalizedUsage(result.Usage)
	if err != nil {
		return work.TaskReadCall{}, usage, err
	}
	callID, ok := singleFunctionCall(result.Output, taskReadFunction)
	if !ok {
		return work.TaskReadCall{}, usage, work.ErrReadModelCall
	}
	output, err := json.Marshal(result.Output)
	if err != nil {
		return work.TaskReadCall{}, usage, err
	}
	return work.TaskReadCall{Protocol: taskReadProtocol, CallID: callID, Output: output}, usage, nil
}

func (p *Responses) CompleteTaskRead(ctx context.Context, messages []work.Message, call work.TaskReadCall, read *work.TaskRead, limit int, emit func(string) error) (work.TaskAnswer, error) {
	var output []json.RawMessage
	if call.Protocol != taskReadProtocol || json.Unmarshal(call.Output, &output) != nil || read == nil || read.Result == nil {
		return work.TaskAnswer{}, work.ErrReadModelCall
	}
	id, ok := singleFunctionCall(output, taskReadFunction)
	if !ok || id != call.CallID {
		return work.TaskAnswer{}, work.ErrReadModelCall
	}
	request, err := conversationRequest(messages, true, limit)
	if err != nil {
		return work.TaskAnswer{}, err
	}
	evidence, err := json.Marshal(read)
	if err != nil {
		return work.TaskAnswer{}, err
	}
	// Replay the provider's completed items in order, including any opaque
	// reasoning items, without depending on a remotely stored response.
	for _, item := range output {
		request.Input = append(request.Input, item)
	}
	request.Input = append(request.Input, map[string]string{"type": "function_call_output", "call_id": call.CallID, "output": string(evidence)})
	request.Instructions += " 已提供本轮用户指定的读取结果。以该结果为依据交付成果。"
	request.Include = []string{"reasoning.encrypted_content"}
	return p.requestTaskAnswer(ctx, request, emit)
}

func taskReadDefinition() responseFunction {
	return responseFunction{
		Type:        "function",
		Name:        taskReadFunction,
		Description: "Read the data source the user selected for this task. The source and arguments are fixed by the user.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
		Strict:      true,
	}
}
