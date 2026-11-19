package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type RouterIntent struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Fragment       string `json:"fragment"`
	Status         string `json:"status"`
	TaskID         string `json:"task_id,omitempty"`
	AfterTaskID    string `json:"after_task_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Detail         string `json:"detail"`
	DuplicateOf    string `json:"duplicate_of,omitempty"`
	ResolvedBy     string `json:"resolved_by,omitempty"`
}

const routerIntentColumns = `i.id,i.kind,i.fragment,i.status,coalesce(i.task_id,''),coalesce(i.after_task_id,''),coalesce(target.id,''),
coalesce(target.conversation_id,''),i.detail,coalesce(i.duplicate_of,''),coalesce(i.resolved_by,'')`

const routerTargetJoin = ` LEFT JOIN task_followups f ON f.request_key=i.dispatch_key
LEFT JOIN runs target ON target.id=coalesce(f.dispatched_run_id,i.target_run_id)`

func scanRouterIntents(rows *sql.Rows) ([]RouterIntent, error) {
	defer rows.Close()
	var intents []RouterIntent
	for rows.Next() {
		var i RouterIntent
		if err := rows.Scan(&i.ID, &i.Kind, &i.Fragment, &i.Status, &i.TaskID, &i.AfterTaskID, &i.RunID, &i.ConversationID,
			&i.Detail, &i.DuplicateOf, &i.ResolvedBy); err != nil {
			return nil, err
		}
		intents = append(intents, i)
	}
	return intents, rows.Err()
}

func routerIntentReceipt(tx *sql.Tx, run string) (*[]RouterIntent, error) {
	rows, err := tx.Query("SELECT "+routerIntentColumns+" FROM router_intents i"+routerTargetJoin+
		" WHERE i.source_run_id=? ORDER BY i.position", run)
	if err != nil {
		return nil, err
	}
	intents, err := scanRouterIntents(rows)
	if len(intents) == 0 || err != nil {
		return nil, err
	}
	return &intents, nil
}

func (s *Store) pendingRouterIntents(ctx context.Context, conversation string) ([]RouterIntent, error) {
	if conversation == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+routerIntentColumns+" FROM router_intents i"+
		" JOIN runs source ON source.id=i.source_run_id"+routerTargetJoin+`
WHERE source.conversation_id=? AND i.status='clarify' AND i.resolved_by IS NULL
ORDER BY i.rowid DESC LIMIT 8`, conversation)
	if err != nil {
		return nil, err
	}
	return scanRouterIntents(rows)
}

func submitRouterBatch(ctx context.Context, tx *sql.Tx, key, fingerprint, text, scope, conversation string, classification *submissionClassification, materials []string) (Run, error) {
	if conversation != "" {
		if err := checkConversationSubmission(tx, conversation); err != nil {
			return Run{}, err
		}
	}
	decisions := classification.Route.Intents
	if classification.Route.Kind != "batch" {
		decisions = []RouteDecision{*classification.Route}
	}
	results := make([]RouterIntent, len(decisions))
	seen := make(map[string]string)
	for index, d := range decisions {
		i := &results[index]
		*i = RouterIntent{ID: digest(fmt.Sprintf("router-intent:%s:%d", key, index+1))[:32], Kind: d.Kind, Fragment: d.Fragment}
		if d.Issue != "" {
			i.Status, i.Detail = "clarify", d.Issue
			continue
		}
		i.TaskID, i.AfterTaskID = d.TaskID, d.AfterTaskID
		signature := routerIntentSignature(d)
		if previous := seen[signature]; previous != "" {
			i.Status, i.Detail, i.DuplicateOf = "duplicate", "这一项已在本条消息中登记。", previous
		} else {
			seen[signature] = i.ID
		}
	}
	if err := markRouterConflicts(tx, decisions, results); err != nil {
		return Run{}, err
	}
	consumers := 0
	for index, d := range decisions {
		if results[index].Status == "" && consumesRouterMaterials(d.Kind) {
			consumers++
		}
	}
	initialRevision := make(map[string]int)
	for index, d := range decisions {
		i := &results[index]
		if i.Status != "" || d.Kind == "order_tasks" {
			continue
		}
		if err := checkRouterResolution(tx, conversation, d.ResolveID, i); err != nil {
			return Run{}, err
		}
		if i.Status != "" {
			continue
		}
		if d.Kind == "clarify" {
			i.Status, i.Detail = "clarify", d.Question
			continue
		}
		var inputMaterials []string
		if consumesRouterMaterials(d.Kind) && len(materials) != 0 {
			if consumers > 1 {
				i.Status, i.Detail = "clarify", "请说明附件用于哪一项，并在补充该项时带上相关附件。"
				continue
			}
			inputMaterials = materials
		}
		// Expected task-state failures affect only this intent. Unexpected
		// storage failures abort the transaction and retry the saved decision.
		if _, err := tx.Exec("SAVEPOINT router_intent"); err != nil {
			return Run{}, err
		}
		err := applyRouterIntent(ctx, tx, d, i, scope, initialRevision, inputMaterials)
		if err != nil {
			if !isRouterIntentConflict(err) {
				return Run{}, err
			}
			if _, rollbackErr := tx.Exec("ROLLBACK TO router_intent"); rollbackErr != nil {
				return Run{}, rollbackErr
			}
			i.Status, i.Detail = "clarify", routerConflictQuestion(err)
		}
		if _, err := tx.Exec("RELEASE router_intent"); err != nil {
			return Run{}, err
		}
		if err == nil {
			if err := recordRouterResolution(tx, d.ResolveID, i.ID); err != nil {
				return Run{}, err
			}
		}
	}
	if err := applyRouterQueueOrders(tx, conversation, decisions, results); err != nil {
		return Run{}, err
	}
	receipt := *classification
	receipt.Route = &RouteDecision{Kind: "clarify", Question: renderRouterIntents(results)}
	source, err := submitWorkTx(ctx, tx, key, fingerprint, text, scope, conversation, false, Delivery{}, &receipt, materials)
	if err != nil {
		return Run{}, err
	}
	for index, i := range results {
		var dispatchKey string
		if i.Status == "queued" {
			dispatchKey = routerDispatchKey(i.ID)
		}
		if _, err = tx.Exec(`INSERT INTO router_intents(id,source_run_id,position,kind,fragment,status,task_id,after_task_id,target_run_id,dispatch_key,detail,duplicate_of)
VALUES(?,?,?,?,?,?,nullif(?,''),nullif(?,''),nullif(?,''),nullif(?,''),?,nullif(?,''))`,
			i.ID, source.ID, index+1, i.Kind, i.Fragment, i.Status, i.TaskID, i.AfterTaskID, i.RunID, dispatchKey, i.Detail, i.DuplicateOf); err != nil {
			return Run{}, err
		}
	}
	source.RouterIntents = &results
	return source, nil
}

func checkRouterResolution(tx *sql.Tx, conversation, resolveID string, i *RouterIntent) error {
	if resolveID == "" {
		return nil
	}
	var resolved string
	err := tx.QueryRow(`SELECT coalesce(i.resolved_by,'') FROM router_intents i
JOIN runs r ON r.id=i.source_run_id WHERE i.id=? AND r.conversation_id=? AND i.status='clarify'`, resolveID, conversation).Scan(&resolved)
	if errors.Is(err, sql.ErrNoRows) {
		i.Status, i.Detail = "clarify", "请指出你在回答当前对话中的哪一项。"
		return nil
	}
	if err == nil && resolved != "" {
		i.Status, i.Detail, i.DuplicateOf = "duplicate", "这项澄清已经处理。", resolved
	}
	return err
}

func recordRouterResolution(tx *sql.Tx, resolveID, intentID string) error {
	if resolveID == "" {
		return nil
	}
	_, err := tx.Exec("UPDATE router_intents SET resolved_by=? WHERE id=? AND resolved_by IS NULL", intentID, resolveID)
	return err
}

func routerDispatchKey(intentID string) string { return "router-intent:" + intentID }

func routerIntentSignature(d RouteDecision) string {
	normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	switch d.Kind {
	case "new_task":
		return d.Kind + "\n" + normalize(d.Goal) + "\n" + normalize(d.Criteria)
	case "set_goal":
		return d.Kind + "\n" + d.TaskID + "\n" + d.Goal
	case "set_priority":
		return d.Kind + "\n" + d.TaskID + "\n" + d.Priority
	case "order_tasks":
		return d.Kind + "\n" + d.TaskID + "\n" + d.AfterTaskID
	case "follow_up", "chat":
		return d.Kind + "\n" + d.TaskID + "\n" + normalize(d.Fragment)
	case "clarify":
		return d.Kind + "\n" + normalize(d.Fragment) + "\n" + normalize(d.Question)
	default:
		return d.Kind + "\n" + d.TaskID
	}
}

func consumesRouterMaterials(kind string) bool {
	return kind == "new_task" || kind == "follow_up" || kind == "chat"
}

func changesRouterTask(kind string) bool {
	return kind == "pause" || kind == "stop" || kind == "resume" || kind == "follow_up" || kind == "set_goal"
}

func markRouterConflicts(tx *sql.Tx, decisions []RouteDecision, results []RouterIntent) error {
	conflicts := make(map[int]string)
	for a, first := range decisions {
		if results[a].Status != "" {
			continue
		}
		for b := a + 1; b < len(decisions); b++ {
			second := decisions[b]
			if results[b].Status != "" {
				continue
			}
			var reason string
			sameTask := first.TaskID != "" && first.TaskID == second.TaskID
			switch {
			case first.ResolveID != "" && first.ResolveID == second.ResolveID:
				reason = "同一待澄清项有多个答案。请确认要采用哪一个。"
			case sameTask && first.Kind == "set_goal" && second.Kind == "set_goal":
				reason = "同一事项有多个新目标。请确认要采用哪一个。"
			case sameTask && first.Kind == "set_priority" && second.Kind == "set_priority":
				reason = "同一事项有多个优先级。请确认要采用哪一个。"
			case sameTask && ((first.Kind == "stop" && second.Kind == "set_priority") ||
				(first.Kind == "set_priority" && second.Kind == "stop")):
				reason = "同一事项同时收到停止和调整排队优先级的要求。请确认要执行哪一个。"
			case sameTask && changesRouterTask(first.Kind) && changesRouterTask(second.Kind) && first.Kind != second.Kind:
				reason = "同一事项收到了相互影响的操作。请确认执行哪一个，或说明先后顺序。"
			default:
				related, err := conflictingWorkflowRoutes(tx, first, second)
				if err != nil {
					return err
				}
				if related {
					reason = "暂停、停止或修改目标会影响另一项要求的任务依赖。请确认先处理哪一项。"
				}
			}
			if reason != "" {
				conflicts[a], conflicts[b] = reason, reason
			}
		}
	}
	for index, reason := range conflicts {
		results[index].Status, results[index].Detail = "clarify", reason
	}
	return nil
}

func conflictingWorkflowRoutes(tx *sql.Tx, a, b RouteDecision) (bool, error) {
	stops := func(kind string) bool { return kind == "stop" || kind == "pause" || kind == "set_goal" }
	advances := func(kind string) bool { return kind == "resume" || kind == "follow_up" }
	if stops(b.Kind) && advances(a.Kind) {
		a, b = b, a
	}
	if !stops(a.Kind) || !advances(b.Kind) || a.TaskID == b.TaskID || a.TaskID == "" || b.TaskID == "" {
		return false, nil
	}
	var related bool
	err := tx.QueryRow(`WITH RECURSIVE upstream(id) AS (
SELECT prerequisite_id FROM workflow_prerequisites WHERE task_id=?
UNION
SELECT p.prerequisite_id FROM workflow_prerequisites p JOIN upstream u ON u.id=p.task_id
)
SELECT EXISTS(SELECT 1 FROM upstream WHERE id=?)
OR EXISTS(SELECT 1 FROM workflow_children WHERE parent_id=? AND task_id=?)`,
		b.TaskID, a.TaskID, a.TaskID, b.TaskID).Scan(&related)
	return related, err
}

func applyRouterIntent(ctx context.Context, tx *sql.Tx, d RouteDecision, i *RouterIntent, scope string, revisions map[string]int, materials []string) error {
	var target Task
	if d.TaskID != "" {
		var err error
		target, err = scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=? AND conversation_id IN (SELECT id FROM conversations WHERE archived=0)", d.TaskID))
		if err != nil {
			return err
		}
		if changesRouterTask(d.Kind) {
			original, seen := revisions[target.ID]
			if !seen {
				original = target.Revision
				revisions[target.ID] = original
			}
			if original != d.Revision {
				return ErrTaskRevision
			}
			d.Revision = target.Revision
		}
	}
	switch d.Kind {
	case "set_goal":
		if target.Goal == d.Goal {
			i.Status, i.Detail = "applied", "事项已使用该目标。"
			return nil
		}
		if err := reviseRoutedGoal(ctx, tx, target, d.Goal); err != nil {
			return err
		}
		i.Status, i.Detail = "applied", "目标已更新，等待按新要求执行。"
	case "set_priority":
		if target.Status == "cancelled" || priorityLabel(d.Priority) == "" {
			return ErrState
		}
		if target.Priority != d.Priority {
			if target.Priority != d.PreviousPriority {
				return ErrTaskRevision
			}
			// Scheduling is independent of the contract bound by an in-flight run.
			if _, err := tx.ExecContext(ctx, "UPDATE tasks SET priority=? WHERE id=?", d.Priority, target.ID); err != nil {
				return err
			}
		}
		i.Status, i.Detail = "applied", "优先级为"+priorityLabel(d.Priority)+"，按此档位调度。"
	case "new_task", "follow_up", "chat":
		isTask := d.Kind != "chat"
		decision := submissionClassification{DelegationResult: DelegationResult{Task: isTask, Route: &d}}
		run, err := submitWorkTx(ctx, tx, routerDispatchKey(i.ID), digest(d.Fragment), d.Fragment, scope, "", isTask, Delivery{}, &decision, materials)
		if err != nil {
			return err
		}
		i.TaskID, i.RunID, i.ConversationID = run.TaskID, run.ID, run.ConversationID
		i.Status, i.Detail = "applied", "已受理。"
		if run.FollowupQueued {
			i.Status, i.Detail = "queued", "已排入该事项的下一轮。"
			i.RunID, i.ConversationID = "", ""
		}
	case "resume":
		run, err := resumeTaskTx(tx, target.ID, routerDispatchKey(i.ID), d.Revision, "resume")
		if err != nil {
			return err
		}
		i.RunID, i.ConversationID = run.ID, run.ConversationID
		i.Status, i.Detail = "applied", "已安排继续「"+target.Title+"」。"
	case "pause", "stop":
		if err := controlTask(tx, target.ID, d.Kind, d.Revision); err != nil {
			return err
		}
		var reason string
		if err := tx.QueryRow("SELECT reason FROM tasks WHERE id=?", target.ID).Scan(&reason); err != nil {
			return err
		}
		i.Status, i.Detail = "applied", target.Title+"："+reason
	case "progress":
		i.Status, i.Detail = "applied", target.Title+"："+target.Reason
	default:
		return ErrState
	}
	return nil
}

func reviseRoutedGoal(ctx context.Context, tx *sql.Tx, task Task, goal string) error {
	var hasPlan bool
	var active string
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_children WHERE parent_id=?),
coalesce((SELECT status FROM runs WHERE conversation_id=? AND status IN ('queued','running') LIMIT 1),'')`,
		task.ID, task.ConversationID).Scan(&hasPlan, &active)
	if err != nil {
		return err
	}
	if hasPlan {
		return TaskEditError("该事项已有子任务计划。请明确要调整哪个子任务，或是否将新目标作为独立事项。")
	}
	if active == "running" {
		return TaskEditError("该事项仍在执行。请先暂停，等暂停完成后再确认新目标。")
	}
	if active == "queued" {
		if err := controlTask(tx, task.ID, "pause", task.Revision); err != nil {
			return err
		}
		task.Revision++
	}
	_, err = editTaskTx(ctx, tx, task.ID, TaskEdit{Revision: task.Revision, Goal: &goal})
	return err
}

func isRouterIntentConflict(err error) bool {
	var edit TaskEditError
	if errors.As(err, &edit) {
		return true
	}
	for _, known := range []error{sql.ErrNoRows, ErrState, ErrTaskRevision, ErrActive, ErrConversationArchived,
		ErrReadUnconfirmed, ErrReadCheckpoint, ErrReadNeedsSelection, ErrPublicationOpen, ErrDeterministicTaskRecoveryUnsupported, ErrBenchmarkRecoveryUnsupported} {
		if errors.Is(err, known) {
			return true
		}
	}
	return false
}

func routerConflictQuestion(err error) string {
	var edit TaskEditError
	switch {
	case errors.As(err, &edit):
		return edit.Error()
	case errors.Is(err, ErrDeterministicTaskRecoveryUnsupported), errors.Is(err, ErrBenchmarkRecoveryUnsupported):
		return err.Error()
	case errors.Is(err, ErrTaskRevision):
		return "事项状态已变化。请查看最新状态后确认是否仍要执行这一项。"
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, ErrConversationArchived):
		return "找不到可处理的目标事项。请确认要处理哪一项。"
	default:
		return "当前状态无法执行这一项。请查看事项进展后确认下一步。"
	}
}

func renderRouterIntents(results []RouterIntent) string {
	var reply strings.Builder
	escape := strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "*", "\\*", "_", "\\_", "`", "\\`", "<", "\\<")
	for index, i := range results {
		if index > 0 {
			reply.WriteString("\n")
		}
		fragment := escape.Replace(strings.Join(strings.Fields(i.Fragment), " "))
		detail := escape.Replace(strings.Join(strings.Fields(i.Detail), " "))
		if fragment != "" {
			fmt.Fprintf(&reply, "- %s：%s", fragment, detail)
		} else {
			fmt.Fprintf(&reply, "- %s", detail)
		}
		if i.AfterTaskID != "" {
			fmt.Fprintf(&reply, " [先做事项](/tasks/%s) [后做事项](/tasks/%s)", i.TaskID, i.AfterTaskID)
		} else if i.TaskID != "" {
			fmt.Fprintf(&reply, " [打开事项](/tasks/%s)", i.TaskID)
		} else if i.ConversationID != "" {
			fmt.Fprintf(&reply, " [打开对话](/chat/%s)", i.ConversationID)
		}
	}
	return reply.String()
}
