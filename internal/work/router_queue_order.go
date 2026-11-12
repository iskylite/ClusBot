package work

import (
	"database/sql"
	"errors"
	"fmt"
)

type queueOrderEdge struct {
	before string
	after  string
}

type queueOrderChange struct {
	position int
	tasks    queueOrderEdge
	runs     queueOrderEdge
	detail   string
}

func applyRouterQueueOrders(tx *sql.Tx, conversation string, decisions []RouteDecision, results []RouterIntent) error {
	var changes []queueOrderChange
	for position, decision := range decisions {
		i := &results[position]
		if decision.Kind != "order_tasks" || i.Status != "" {
			continue
		}
		if err := checkRouterResolution(tx, conversation, decision.ResolveID, i); err != nil {
			return err
		}
		if i.Status != "" {
			continue
		}
		change, err := prepareQueueOrder(tx, decision, i)
		if err != nil {
			return err
		}
		if i.Status == "applied" {
			if err := recordRouterResolution(tx, decision.ResolveID, i.ID); err != nil {
				return err
			}
		} else if i.Status == "" {
			change.position = position
			changes = append(changes, change)
		}
	}
	if len(changes) == 0 {
		return nil
	}
	if err := markQueueOrderConflicts(tx, changes, results); err != nil {
		return err
	}
	for _, change := range changes {
		i := &results[change.position]
		if i.Status != "" {
			continue
		}
		if _, err := tx.Exec("DELETE FROM run_queue_order WHERE before_run_id=? AND after_run_id=?", change.runs.after, change.runs.before); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO run_queue_order(before_run_id,after_run_id) VALUES(?,?) ON CONFLICT DO NOTHING", change.runs.before, change.runs.after); err != nil {
			return err
		}
		i.Status, i.Detail = "applied", change.detail
		if err := recordRouterResolution(tx, decisions[change.position].ResolveID, i.ID); err != nil {
			return err
		}
	}
	return nil
}

func prepareQueueOrder(tx *sql.Tx, d RouteDecision, i *RouterIntent) (queueOrderChange, error) {
	change := queueOrderChange{tasks: queueOrderEdge{d.TaskID, d.AfterTaskID}}
	const lookup = `SELECT t.title,t.revision,r.id,r.status FROM tasks t
JOIN conversations c ON c.id=t.conversation_id
JOIN runs r ON r.task_id=t.id AND r.status IN ('queued','running')
WHERE t.id=? AND c.archived=0`
	var firstTitle, secondTitle, firstStatus, secondStatus string
	var firstRevision, secondRevision int
	err := tx.QueryRow(lookup, d.TaskID).Scan(&firstTitle, &firstRevision, &change.runs.before, &firstStatus)
	if err == nil {
		err = tx.QueryRow(lookup, d.AfterTaskID).Scan(&secondTitle, &secondRevision, &change.runs.after, &secondStatus)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		i.Status, i.Detail = "clarify", "有事项已离开队列或尚未入队。请确认要调整哪两个排队事项。"
	case err != nil:
		return change, err
	case secondStatus != "queued":
		i.Status, i.Detail = "clarify", fmt.Sprintf("「%s」已经开始执行。请确认接下来如何安排这两项。", secondTitle)
	case secondRevision != d.AfterRevision || (firstStatus == "queued" && firstRevision != d.Revision):
		i.Status, i.Detail = "clarify", routerConflictQuestion(ErrTaskRevision)
	case firstStatus == "running":
		i.Status, i.Detail = "applied", fmt.Sprintf("「%s」已经先开始，「%s」仍在排队。", firstTitle, secondTitle)
	default:
		change.detail = fmt.Sprintf("本轮先排「%s」，再排「%s」。", firstTitle, secondTitle)
	}
	return change, nil
}

func markQueueOrderConflicts(tx *sql.Tx, changes []queueOrderChange, results []RouterIntent) error {
	rows, err := tx.Query(`
SELECT prerequisite_id,task_id,1 FROM workflow_prerequisites
UNION ALL
SELECT b.task_id,a.task_id,0 FROM run_queue_order q
JOIN runs b ON b.id=q.before_run_id AND b.status='queued'
JOIN runs a ON a.id=q.after_run_id AND a.status='queued'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	workflow := make(map[string][]string)
	var saved []queueOrderEdge
	for rows.Next() {
		var edge queueOrderEdge
		var dependency bool
		if err := rows.Scan(&edge.before, &edge.after, &dependency); err != nil {
			return err
		}
		if dependency {
			workflow[edge.before] = append(workflow[edge.before], edge.after)
		} else {
			saved = append(saved, edge)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, change := range changes {
		if queueOrderReaches(workflow, change.tasks.after, change.tasks.before) {
			i := &results[change.position]
			i.Status, i.Detail = "clarify", "这两个事项的顺序与任务依赖相反。请确认要先处理哪个依赖事项。"
		}
	}
	// A new direction replaces an earlier order for the same pair. If an
	// intent is rejected, its prior order stays in force on the next check.
	for {
		proposed := make(map[queueOrderEdge]bool)
		graph := make(map[string][]string)
		for from, to := range workflow {
			graph[from] = append([]string(nil), to...)
		}
		for _, change := range changes {
			if results[change.position].Status == "" {
				proposed[change.tasks] = true
				graph[change.tasks.before] = append(graph[change.tasks.before], change.tasks.after)
			}
		}
		for _, edge := range saved {
			if !proposed[queueOrderEdge{edge.after, edge.before}] {
				graph[edge.before] = append(graph[edge.before], edge.after)
			}
		}
		rejected := false
		for _, change := range changes {
			i := &results[change.position]
			if i.Status == "" && queueOrderReaches(graph, change.tasks.after, change.tasks.before) {
				i.Status, i.Detail = "clarify", "这项顺序会和已排顺序或任务依赖形成循环。请确认先后顺序。"
				rejected = true
			}
		}
		if !rejected {
			return nil
		}
	}
}

func queueOrderReaches(graph map[string][]string, from, to string) bool {
	pending := []string{from}
	seen := make(map[string]bool)
	for len(pending) != 0 {
		last := len(pending) - 1
		node := pending[last]
		pending = pending[:last]
		if node == to {
			return true
		}
		if !seen[node] {
			seen[node] = true
			pending = append(pending, graph[node]...)
		}
	}
	return false
}
