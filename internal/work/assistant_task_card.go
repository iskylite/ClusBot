package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const assistantTaskListEvent = "assistant.task_list_snapshot"
const assistantTaskProgressEvent = "assistant.task_progress_snapshot:"
const assistantTaskListLimit = 10
const assistantTaskProgressSource = "assistant-task-progress"

type AssistantTaskListRow struct {
	ID, Title, Status, Scope string
}

type AssistantTaskListCard struct {
	Rows     []AssistantTaskListRow
	More     bool
	Revision int64                            `json:"-"`
	Progress map[string]AssistantTaskProgress `json:"-"`
}

type AssistantTaskProgress struct {
	Decision  string
	Title     string
	Status    string
	LatestRun string
	CheckedAt int64
}

// A task-list query remembers its bounded snapshot in the original chat turn.
// There is no new Task, run of existing work, or change to its business state.
func (s *Store) OpenAssistantTaskList(ctx context.Context, actor, conversation, request, text string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if allowed, err := ownsPersonalBusiness(tx, actor); err != nil {
		return Run{}, err
	} else if !allowed {
		return Run{}, ErrAuthorizationDenied
	}
	if conversation != "" {
		if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
			return Run{}, err
		}
	}
	key := "assistant-task-list:" + digest(toJSON([]string{actor, request}))
	fingerprint := digest(toJSON([]string{assistantTaskListEvent, conversation, text}))
	if prior, err := previousSubmission(tx, key, fingerprint); err != nil || prior.ID != "" {
		return prior, err
	}
	list, err := queryTasks(ctx, tx, TaskFilter{Limit: assistantTaskListLimit + 1}, "1=1", taskUpdatedOrder)
	if err != nil {
		return Run{}, err
	}
	card := AssistantTaskListCard{Rows: make([]AssistantTaskListRow, 0, assistantTaskListLimit), More: len(list) > assistantTaskListLimit}
	for _, task := range list[:min(len(list), assistantTaskListLimit)] {
		card.Rows = append(card.Rows, AssistantTaskListRow{task.ID, task.Title, task.Status, task.Scope})
	}
	intent := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{Kind: "clarify", Question: "这是本人的任务清单。可以在清单内选择一项核对进度。"}}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fingerprint, text, "", conversation, false, Delivery{}, &intent, nil, true)
	if err != nil {
		return Run{}, err
	}
	if err := addEvent(tx, run.ID, assistantTaskListEvent, card); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func assistantTaskListCardTx(ctx context.Context, tx *sql.Tx, actor, conversation, run string) (AssistantTaskListCard, error) {
	var card AssistantTaskListCard
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return card, err
	}
	var snapshot string
	err := tx.QueryRowContext(ctx, `SELECT c.revision, e.data FROM conversations c JOIN runs r ON r.conversation_id=c.id
JOIN run_events e ON e.run_id=r.id AND e.kind=? WHERE c.id=? AND r.id=?`, assistantTaskListEvent, conversation, run).Scan(&card.Revision, &snapshot)
	if err != nil {
		return card, err
	}
	if err := json.Unmarshal([]byte(snapshot), &card); err != nil {
		return card, err
	}
	if len(card.Rows) > assistantTaskListLimit {
		return card, ErrState
	}
	card.Progress = make(map[string]AssistantTaskProgress)
	events, err := tx.QueryContext(ctx, `SELECT kind,data FROM run_events WHERE run_id=? AND kind GLOB ? ORDER BY seq`, run, assistantTaskProgressEvent+"*")
	if err != nil {
		return card, err
	}
	defer events.Close()
	for events.Next() {
		var kind, raw string
		if err := events.Scan(&kind, &raw); err != nil {
			return card, err
		}
		var result AssistantTaskProgress
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return card, err
		}
		card.Progress[strings.TrimPrefix(kind, assistantTaskProgressEvent)] = result
	}
	return card, events.Err()
}

func (s *Store) AssistantTaskListCard(ctx context.Context, actor, conversation, run string) (AssistantTaskListCard, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AssistantTaskListCard{}, err
	}
	defer tx.Rollback()
	return assistantTaskListCardTx(ctx, tx, actor, conversation, run)
}

func (c AssistantTaskListCard) HasTask(target string) bool {
	for _, row := range c.Rows {
		if row.ID == target {
			return true
		}
	}
	return false
}

type AssistantTaskProgressAction = AssistantReadCardAction

var taskCardReceiptPolicy = assistantReadReceiptPolicy{assistantTaskProgressSource, "query_task_progress", 3 * assistantTaskListLimit}

// Reserve a read before asking the model. A repeated key never restarts an
// ambiguous model call and history views never claim or restart work.
func (s *Store) BeginAssistantTaskProgress(ctx context.Context, a AssistantTaskProgressAction) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	card, err := assistantTaskListCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return false, err
	}
	if a.Revision != card.Revision || !card.HasTask(a.Target) {
		return false, ErrConversationRevision
	}
	_, restored := card.Progress[a.Target]
	done, err := beginAssistantReadReceipt(ctx, tx, taskCardReceiptPolicy, a, restored)
	if err != nil {
		return false, err
	}
	return done, tx.Commit()
}

func (s *Store) FailAssistantTaskProgress(ctx context.Context, a AssistantTaskProgressAction) error {
	return s.failAssistantReadReceipt(ctx, taskCardReceiptPolicy, a)
}

// Finish reads current facts and writes the conversation-only receipt in one
// transaction. Duplicate clicks can only return the existing receipt.
func (s *Store) FinishAssistantTaskProgress(ctx context.Context, a AssistantTaskProgressAction, decision string) error {
	if decision != "show_progress" && decision != "ask_for_details" && decision != "cannot_prepare" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	card, err := assistantTaskListCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return err
	}
	if a.Revision != card.Revision || !card.HasTask(a.Target) {
		return ErrConversationRevision
	}
	if err := finishAssistantReadReceipt(ctx, tx, taskCardReceiptPolicy, a); err != nil {
		return err
	}
	if _, exists := card.Progress[a.Target]; exists {
		return tx.Commit()
	}
	result := AssistantTaskProgress{Decision: decision, CheckedAt: now()}
	if decision == "show_progress" {
		err = tx.QueryRowContext(ctx, `SELECT title,status,coalesce((SELECT status FROM runs WHERE task_id=tasks.id ORDER BY rowid DESC LIMIT 1),'')
FROM tasks WHERE id=?`, a.Target).Scan(&result.Title, &result.Status, &result.LatestRun)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			result.Decision = "task_unavailable"
		}
	}
	if err := addEvent(tx, a.Run, assistantTaskProgressEvent+a.Target, result); err != nil {
		return err
	}
	return tx.Commit()
}
