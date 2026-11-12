package model

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"clusbot/internal/work"
)

const inputRequestFunction = "ask_user_for_input"
const taskInputInstructions = " 先用当前要求、对话和材料完成工作。不要请求用户确认是否开始或复述已经给出的要求。仅当缺少无法从现有信息取得的必要事实或决定时，调用 ask_user_for_input 提出一个具体问题并结束本轮。不要用普通回复代替必要追问，也不要同时生成成果和发起追问。"

func (p *Responses) StreamTaskAnswer(ctx context.Context, messages []work.Message, limit int, emit func(string) error) (work.TaskAnswer, error) {
	request, err := conversationRequest(messages, true, limit)
	if err != nil {
		return work.TaskAnswer{}, err
	}
	return p.requestTaskAnswer(ctx, request, emit)
}

func (p *Responses) requestTaskAnswer(ctx context.Context, request responseRequest, emit func(string) error) (work.TaskAnswer, error) {
	request.Instructions += taskInputInstructions
	request.Tools = []responseFunction{inputRequestDefinition()}
	allowPlan := work.WorkerCanPlan(ctx)
	if allowPlan {
		addWorkerPlanTool(&request)
	}
	request.ToolChoice = "auto"
	result, err := p.requestStream(ctx, request, emit)
	answer := work.TaskAnswer{Usage: normalizedUsage(result.Usage)}
	if err != nil {
		return answer, err
	}
	answer.Question, answer.Plan, err = parseTaskDecision(result.Output, allowPlan)
	return answer, err
}

func inputRequestDefinition() responseFunction {
	return responseFunction{
		Type:        "function",
		Name:        inputRequestFunction,
		Description: "End this task run with one question about essential information that the user must supply before delivery.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"],"additionalProperties":false}`),
		Strict:      true,
	}
}

func inputRequest(output []json.RawMessage) (string, error) {
	var question string
	for _, raw := range output {
		var item struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			CallID    string `json:"call_id"`
			Status    string `json:"status"`
			Arguments string `json:"arguments"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return "", work.ErrTaskAnswer
		}
		switch item.Type {
		case "message", "reasoning":
			continue
		case "function_call":
			if question != "" || item.Name != inputRequestFunction || strings.TrimSpace(item.CallID) == "" || len(item.CallID) > 256 ||
				(item.Status != "" && item.Status != "completed") {
				return "", work.ErrTaskAnswer
			}
			var arguments map[string]json.RawMessage
			if json.Unmarshal([]byte(item.Arguments), &arguments) != nil || len(arguments) != 1 || json.Unmarshal(arguments["question"], &question) != nil {
				return "", work.ErrTaskAnswer
			}
			question = strings.TrimSpace(question)
			if question == "" || utf8.RuneCountInString(question) > 600 || strings.ContainsRune(question, 0) {
				return "", work.ErrTaskAnswer
			}
		default:
			return "", work.ErrTaskAnswer
		}
	}
	return question, nil
}

const workerPlanFunction = "propose_task_plan"

func addWorkerPlanTool(request *responseRequest) {
	request.Instructions += " 仅当用户目标确实需要可独立交付和验收的工作时，才使用 propose_task_plan 提议 2 至 4 个子任务。每项只做用户目标内的工作，写明验收条件，以编号声明必要依赖。不要把紧密相连的普通步骤拆成子任务，不要扩大工具、数据或外部操作权限。提议计划时只调用此函数，不同时生成正文、追问或调用读取工具。"
	request.Tools = append(request.Tools, responseFunction{
		Type: "function", Name: workerPlanFunction,
		Description: "Propose independently verifiable subtasks for the current authorized goal.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"steps":{"type":"array","items":{"type":"object","properties":{"key":{"type":"string"},"goal":{"type":"string"},"acceptance_criteria":{"type":"string"},"depends_on":{"type":"array","items":{"type":"string"}},"optional":{"type":"boolean"}},"required":["key","goal","acceptance_criteria","depends_on","optional"],"additionalProperties":false}}},"required":["steps"],"additionalProperties":false}`),
		Strict:      true,
	})
}

func parseTaskDecision(output []json.RawMessage, allowPlan bool) (string, []work.WorkflowStep, error) {
	var plan []work.WorkflowStep
	var calls int
	for _, raw := range output {
		var item struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			CallID    string `json:"call_id"`
			Status    string `json:"status"`
			Arguments string `json:"arguments"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return "", nil, work.ErrTaskAnswer
		}
		switch item.Type {
		case "message", "reasoning":
			continue
		case "function_call":
			calls++
		default:
			return "", nil, work.ErrTaskAnswer
		}
		if item.Name != workerPlanFunction {
			continue
		}
		if !allowPlan || plan != nil || strings.TrimSpace(item.CallID) == "" || len(item.CallID) > 256 ||
			(item.Status != "" && item.Status != "completed") || len(item.Arguments) > 16*1024 || !json.Valid([]byte(item.Arguments)) {
			return "", nil, work.ErrTaskAnswer
		}
		var args struct {
			Steps []work.WorkflowStep `json:"steps"`
		}
		decoder := json.NewDecoder(strings.NewReader(item.Arguments))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&args); err != nil || len(args.Steps) < 2 || len(args.Steps) > work.MaxWorkerPlanSteps {
			return "", nil, work.ErrTaskAnswer
		}
		plan = args.Steps
	}
	if plan != nil {
		if calls != 1 {
			return "", nil, work.ErrTaskAnswer
		}
		return "", plan, nil
	}
	question, err := inputRequest(output)
	return question, nil, err
}
