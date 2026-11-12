package model

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const probeTool = "clusbot_test_value"

var (
	ErrTestFailed  = errors.New("模型能力测试未通过")
	ErrTestTimeout = errors.New("模型连接测试超时")
)

func (p *Responses) testCapabilities(ctx context.Context) error {
	text, err := p.readText(ctx, responseRequest{
		Input:           []any{responseMessage{Type: "message", Role: "user", Content: "ClusBot 连接测试，请只回复 OK。"}},
		MaxOutputTokens: 128,
	})
	if err != nil || strings.TrimSpace(text) == "" {
		return probeFailure(ctx, "未收到完整的文本流", err)
	}

	message := responseMessage{
		Type:    "message",
		Role:    "user",
		Content: "请调用 clusbot_test_value 获取测试码。收到工具结果后，逐字回复测试码。",
	}
	request := responseRequest{
		Input:           []any{message},
		MaxOutputTokens: 256,
		Tools: []responseFunction{{
			Type:        "function",
			Name:        probeTool,
			Description: "Returns a one-time test value. Takes no arguments.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
			Strict:      true,
		}},
		ToolChoice: "required",
	}
	result, err := p.requestStream(ctx, request, nil)
	if err != nil {
		return probeFailure(ctx, "未收到完整的工具调用", err)
	}
	callID, ok := singleFunctionCall(result.Output, probeTool)
	if !ok {
		return probeFailure(ctx, "未收到一个有效的测试工具调用", nil)
	}

	// Generate the value only after the model's call is complete. It cannot be
	// echoed from the initial prompt or a previous successful test.
	value := rand.Text()
	input := make([]any, 0, len(result.Output)+2)
	input = append(input, message)
	for _, item := range result.Output {
		input = append(input, item)
	}
	input = append(input, map[string]string{"type": "function_call_output", "call_id": callID, "output": value})
	request.Input = input
	request.ToolChoice = "none"
	text, err = p.readText(ctx, request)
	if err != nil || !strings.Contains(text, value) {
		return probeFailure(ctx, "模型未能根据工具结果回复测试码", err)
	}
	return nil
}

func (p *Responses) readText(ctx context.Context, request responseRequest) (string, error) {
	var text strings.Builder
	_, err := p.requestStream(ctx, request, func(delta string) error {
		if text.Len()+len(delta) > 4096 {
			return errors.New("model text exceeds size limit")
		}
		text.WriteString(delta)
		return nil
	})
	return text.String(), err
}

func singleFunctionCall(output []json.RawMessage, name string) (string, bool) {
	var id string
	for _, raw := range output {
		var item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return "", false
		}
		switch item.Type {
		case "reasoning", "message":
			continue
		case "function_call":
			var arguments map[string]json.RawMessage
			if id != "" || item.Name != name || strings.TrimSpace(item.CallID) == "" || len(item.CallID) > 256 ||
				(item.Status != "" && item.Status != "completed") ||
				json.Unmarshal([]byte(item.Arguments), &arguments) != nil || arguments == nil || len(arguments) != 0 {
				return "", false
			}
			id = item.CallID
		default:
			return "", false
		}
	}
	return id, id != ""
}

func probeFailure(ctx context.Context, phase string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w，%s", ErrTestTimeout, phase)
	}
	var status responseHTTPError
	if errors.As(err, &status) {
		return fmt.Errorf("%w，%s（HTTP %d）", ErrTestFailed, phase, int(status))
	}
	return fmt.Errorf("%w，%s", ErrTestFailed, phase)
}
