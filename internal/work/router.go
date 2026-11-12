package work

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

type RouteDecision struct {
	Kind             string          `json:"kind"`
	TaskID           string          `json:"task_id,omitempty"`
	AfterTaskID      string          `json:"after_task_id,omitempty"`
	Goal             string          `json:"goal,omitempty"`
	Criteria         string          `json:"criteria,omitempty"`
	Question         string          `json:"question,omitempty"`
	Revision         int             `json:"revision,omitempty"`
	AfterRevision    int             `json:"after_revision,omitempty"`
	Priority         string          `json:"priority,omitempty"`
	PreviousPriority string          `json:"previous_priority,omitempty"`
	Fragment         string          `json:"fragment,omitempty"`
	ResolveID        string          `json:"resolve_id,omitempty"`
	Intents          []RouteDecision `json:"intents,omitempty"`
	Issue            string          `json:"issue,omitempty"`
}

type RouteCandidate struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Goal     string `json:"goal"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Priority string `json:"priority"`
	Revision int    `json:"-"`
}

type Router func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error)

// SubmitRoutedChat keeps routing under the existing admission budget and
// request identity. Model proposals are checked against the supplied candidates
// and the selected Task is checked again in the submission transaction.
func (s *Store) SubmitRoutedChat(ctx context.Context, key, text, scope, conversation string, delivery Delivery, route Router, materials ...string) (Run, error) {
	ctx = context.WithValue(ctx, routerBudgetKey{}, true)
	return s.SubmitClassifiedChat(ctx, key, text, scope, conversation, delivery, func(ctx context.Context, text string, history []Message, limit int) (DelegationResult, error) {
		candidates, err := s.routeCandidates(ctx, text)
		if err != nil {
			return DelegationResult{}, err
		}
		pending, err := s.pendingRouterIntents(ctx, conversation)
		if err != nil {
			return DelegationResult{}, err
		}
		ctx = context.WithValue(ctx, routerPendingKey{}, pending)
		result, err := route(ctx, text, history, candidates, limit)
		if err != nil {
			return result, err
		}
		if err = validateRoutedInput(result.Route, candidates, pending, text); err != nil {
			return result, err
		}
		result.Task = result.Route.Kind == "new_task" || result.Route.Kind == "follow_up"
		return result, nil
	}, materials...)
}

type routerBudgetKey struct{}
type routerPendingKey struct{}

func RouterPending(ctx context.Context) []RouterIntent {
	pending, _ := ctx.Value(routerPendingKey{}).([]RouterIntent)
	return pending
}

func validateRoutedInput(route *RouteDecision, candidates []RouteCandidate, pending []RouterIntent, text string) error {
	if route == nil {
		return validateRoute(route, candidates)
	}
	route.Issue = ""
	if route.Kind != "batch" && route.Kind != "clarify" && !usesRouterReceipt(route.Kind) && route.ResolveID == "" {
		if len(route.Intents) != 0 {
			return errors.New("路由决定包含未声明的子意图")
		}
		return validateRoute(route, candidates)
	}
	intents := route.Intents
	if route.Kind == "batch" {
		if len(intents) < 2 || len(intents) > 6 || route.ResolveID != "" {
			return errors.New("每次可处理 2 至 6 个独立意图")
		}
	} else {
		if len(intents) != 0 {
			return errors.New("澄清回复包含未声明的子意图")
		}
		intents = []RouteDecision{*route}
	}
	for i := range intents {
		item := &intents[i]
		item.Issue = ""
		item.Fragment = strings.TrimSpace(item.Fragment)
		if route.Kind != "batch" && item.Fragment == "" {
			item.Fragment = text
		}
		if item.Fragment == "" || !strings.Contains(text, item.Fragment) {
			item.Fragment = ""
			item.Issue = "请指出这一项要求对应的原话。"
		}
		if len(item.Intents) != 0 || validateRoute(item, candidates) != nil {
			item.Issue = "请明确这一项要做什么，以及它对应的事项。"
			if item.Kind == "order_tasks" {
				item.Issue = "请指出要先做和后做的两个不同事项。"
			}
		}
		item.Intents = nil
		goalFromUser := strings.Contains(item.Fragment, item.Goal)
		if item.ResolveID != "" {
			var known bool
			for _, p := range pending {
				if p.ID == item.ResolveID {
					known = true
					goalFromUser = goalFromUser || strings.Contains(p.Fragment, item.Goal)
					break
				}
			}
			if !known {
				item.Issue = "请明确你要回答当前对话中的哪一条待澄清问题。"
			}
		}
		if item.Kind == "set_goal" && item.Issue == "" && !goalFromUser {
			item.Issue = "请写明要替换成的目标原文。"
		}
	}
	if route.Kind != "batch" {
		*route = intents[0]
	}
	return nil
}

func (s *Store) routeCandidates(ctx context.Context, text string) ([]RouteCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,t.title,substr(t.goal,1,180),t.status,t.reason,t.priority,t.revision
FROM tasks t JOIN conversations c ON c.id=t.conversation_id
WHERE t.status<>'cancelled' AND c.archived=0
ORDER BY (instr(?,t.id)>0) DESC,(length(t.title)>1 AND instr(?,t.title)>0) DESC,t.updated_at DESC,t.id LIMIT 16`, text, text)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := []RouteCandidate{}
	for rows.Next() {
		var item RouteCandidate
		if err = rows.Scan(&item.ID, &item.Title, &item.Goal, &item.Status, &item.Reason, &item.Priority, &item.Revision); err != nil {
			return nil, err
		}
		candidates = append(candidates, item)
	}
	return candidates, rows.Err()
}

func validateRoute(route *RouteDecision, candidates []RouteCandidate) error {
	invalid := errors.New("路由决定无效，请明确本次工作要求")
	if route == nil {
		return invalid
	}
	route.Revision = 0
	route.AfterRevision = 0
	route.PreviousPriority = ""
	route.Goal = strings.TrimSpace(route.Goal)
	route.Criteria = strings.TrimSpace(route.Criteria)
	route.Question = strings.TrimSpace(route.Question)
	if route.Kind != "set_priority" && route.Priority != "" {
		return invalid
	}
	if route.Kind != "order_tasks" && route.AfterTaskID != "" {
		return invalid
	}
	switch route.Kind {
	case "chat":
		if route.TaskID == "" && route.Goal == "" && route.Criteria == "" && route.Question == "" {
			return nil
		}
	case "new_task":
		if route.TaskID == "" && route.Question == "" && route.Goal != "" &&
			utf8.RuneCountInString(route.Goal) <= 16000 && utf8.RuneCountInString(route.Criteria) <= 1000 {
			return nil
		}
	case "follow_up", "progress", "pause", "stop", "resume", "set_goal", "set_priority":
		if route.Criteria != "" || route.Question != "" {
			return invalid
		}
		if route.Kind == "set_goal" {
			if route.Goal == "" || utf8.RuneCountInString(route.Goal) > 16000 {
				return invalid
			}
		} else if route.Goal != "" {
			return invalid
		}
		if route.Kind == "set_priority" && priorityLabel(route.Priority) == "" {
			return invalid
		}
		for _, item := range candidates {
			if route.TaskID == item.ID {
				route.Revision = item.Revision
				if route.Kind == "set_priority" {
					route.PreviousPriority = item.Priority
				}
				return nil
			}
		}
	case "clarify":
		if route.TaskID == "" && route.Goal == "" && route.Criteria == "" && route.Question != "" && utf8.RuneCountInString(route.Question) <= 600 {
			return nil
		}
	case "order_tasks":
		if route.TaskID == "" || route.AfterTaskID == "" || route.TaskID == route.AfterTaskID ||
			route.Goal != "" || route.Criteria != "" || route.Question != "" {
			return invalid
		}
		for _, item := range candidates {
			if item.ID == route.TaskID {
				route.Revision = item.Revision
			}
			if item.ID == route.AfterTaskID {
				route.AfterRevision = item.Revision
			}
		}
		if route.Revision > 0 && route.AfterRevision > 0 {
			return nil
		}
	}
	return invalid
}

func usesRouterReceipt(kind string) bool {
	return kind == "set_goal" || kind == "set_priority" || kind == "order_tasks"
}

func priorityLabel(priority string) string {
	switch priority {
	case "high":
		return "高"
	case "normal":
		return "普通"
	case "low":
		return "低"
	default:
		return ""
	}
}
