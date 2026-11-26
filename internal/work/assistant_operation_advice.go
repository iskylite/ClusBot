package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const assistantOperationAdviceEvent = "assistant.operation_advice"
const assistantOperationChoiceEvent = "assistant.operation_advice_choice"

var assistantOperationChoicePolicy = assistantReadReceiptPolicy{
	source: "a2ui-assistant-advice", kind: assistantOperationChoiceEvent, limit: 1,
}

type AssistantAdviceOption struct {
	Label       string `json:"label"`
	Detail      string `json:"detail,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// Only presentation and the exact objects the suggestion was based on:
// never save the raw inventory, credentials, tool inputs, or an operation.
type AssistantAdviceDraft struct {
	Title, Summary, Scope, Focus string
	Options                      []AssistantAdviceOption
	References                   []string
	ExpiresAt                    int64
}

type AssistantAdviceChoice struct {
	Slot       string `json:"slot"`
	Decision   string `json:"decision"`
	RecordedAt int64  `json:"recorded_at"`
}

type AssistantAdviceCard struct {
	Draft      AssistantAdviceDraft
	SourceText string
	Revision   int64
	Archived   bool
	Choice     *AssistantAdviceChoice
	Attempt    string
}

func assistantAdviceKey(actor, request, conversation, text string) (string, string) {
	return "assistant-advice:" + digest(toJSON([]string{actor, request})),
		digest(toJSON([]string{assistantOperationAdviceEvent, conversation, text}))
}

// Read-only replay before an initial model request. No task is created here
// or by selecting an option.
func (s *Store) PreviousAssistantAdvice(ctx context.Context, actor, request, conversation, text string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	allowed, err := ownsPersonalBusiness(tx, actor)
	if err != nil {
		return Run{}, err
	}
	if !allowed {
		return Run{}, ErrAuthorizationDenied
	}
	key, fp := assistantAdviceKey(actor, request, conversation, text)
	return previousSubmission(tx, key, fp)
}

func checkAdviceRefs(ctx context.Context, tx *sql.Tx, actor string, refs []string) error {
	if len(refs) < 1 || len(refs) > 2048 {
		return ErrAuthorizationDenied
	}
	stamp := now()
	for _, ref := range refs {
		var read bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM authorizations WHERE subject=? AND action='read' AND object_ref=?
AND revoked_at=0 AND valid_from<=? AND (expires_at=0 OR expires_at>?))`, actor, ref, stamp, stamp).Scan(&read); err != nil {
			return err
		}
		if !read {
			return ErrAuthorizationDenied
		}
	}
	return nil
}

func (s *Store) OpenAssistantAdvice(ctx context.Context, actor, request, conversation, text string, draft AssistantAdviceDraft) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	allowed, err := ownsPersonalBusiness(tx, actor)
	if err != nil {
		return Run{}, err
	}
	if !allowed {
		return Run{}, ErrAuthorizationDenied
	}
	if conversation != "" {
		if err = discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
			return Run{}, err
		}
	}
	key, fp := assistantAdviceKey(actor, request, conversation, text)
	if run, err := previousSubmission(tx, key, fp); err != nil || run.ID != "" {
		return run, err
	}
	if draft.Title == "" || utf8.RuneCountInString(draft.Title) > 80 || utf8.RuneCountInString(draft.Summary) > 240 ||
		len(draft.Options) < 1 || len(draft.Options) > 4 || len(request) < 8 || len(text) > 4096 || strings.TrimSpace(text) == "" {
		return Run{}, ErrState
	}
	for _, option := range draft.Options {
		if option.Label == "" || utf8.RuneCountInString(option.Label) > 80 || utf8.RuneCountInString(option.Detail) > 240 {
			return Run{}, ErrState
		}
	}
	if err = checkAdviceRefs(ctx, tx, actor, draft.References); err != nil {
		return Run{}, err
	}
	draft.ExpiresAt = now() + int64(5*time.Minute/time.Millisecond)
	intent := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{Kind: "clarify",
		Question: "已整理可选建议。记录选择不会执行运维操作，也不会授予执行权限。"}}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fp, text, "", conversation, false, Delivery{}, &intent, nil, true)
	if err != nil {
		return Run{}, err
	}
	if err = addEvent(tx, run.ID, assistantOperationAdviceEvent, draft); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func assistantAdviceCardTx(ctx context.Context, tx *sql.Tx, actor, conversation, run string) (AssistantAdviceCard, error) {
	var card AssistantAdviceCard
	allowed, err := ownsPersonalBusiness(tx, actor)
	if err != nil {
		return card, err
	}
	if !allowed {
		return card, sql.ErrNoRows
	}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT c.revision,c.archived,e.data,
  (SELECT m.text FROM messages m WHERE m.conversation_id=c.id AND m.run_id=r.id AND m.role='user' ORDER BY m.seq LIMIT 1)
FROM conversations c JOIN runs r ON r.conversation_id=c.id
JOIN run_events e ON e.run_id=r.id AND e.kind=?
WHERE c.id=? AND r.id=? AND r.status='completed'`, assistantOperationAdviceEvent, conversation, run).
		Scan(&card.Revision, &card.Archived, &raw, &card.SourceText)
	if err != nil {
		return card, err
	}
	if err = json.Unmarshal([]byte(raw), &card.Draft); err != nil || len(card.Draft.Options) < 1 || len(card.Draft.Options) > 4 {
		return card, ErrState
	}
	if err = checkAdviceRefs(ctx, tx, actor, card.Draft.References); err != nil {
		return card, err
	}
	err = tx.QueryRowContext(ctx, `SELECT data FROM run_events WHERE run_id=? AND kind=?`, run, assistantOperationChoiceEvent).Scan(&raw)
	if err == nil {
		var choice AssistantAdviceChoice
		if err = json.Unmarshal([]byte(raw), &choice); err != nil || !card.HasSlot(choice.Slot) || (choice.Decision != "record_selection" && choice.Decision != "request_details") {
			return card, ErrState
		}
		card.Choice = &choice
	} else if !errors.Is(err, sql.ErrNoRows) {
		return card, err
	}
	err = tx.QueryRowContext(ctx, `SELECT state FROM inbox_events WHERE source=? AND principal=? AND run_id=? ORDER BY received_at DESC LIMIT 1`,
		assistantOperationChoicePolicy.source, actor, run).Scan(&card.Attempt)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return card, err
}

func (c AssistantAdviceCard) HasSlot(slot string) bool {
	n, err := strconv.Atoi(slot)
	return err == nil && strconv.Itoa(n) == slot && n > 0 && n <= len(c.Draft.Options)
}

func (c AssistantAdviceCard) CanSelect() bool {
	return !c.Archived && c.Choice == nil && c.Attempt == "" && c.Draft.ExpiresAt > now()
}

func (s *Store) AssistantAdvice(ctx context.Context, actor, conversation, run string) (AssistantAdviceCard, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AssistantAdviceCard{}, err
	}
	defer tx.Rollback()
	return assistantAdviceCardTx(ctx, tx, actor, conversation, run)
}

func (s *Store) BeginAssistantAdviceChoice(ctx context.Context, a AssistantReadCardAction) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	card, err := assistantAdviceCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return false, err
	}
	if card.Archived || card.Revision != a.Revision || !card.HasSlot(a.Target) || card.Draft.ExpiresAt <= now() {
		return false, ErrConversationRevision
	}
	if card.Choice != nil && card.Choice.Slot != a.Target {
		return false, ErrConflict
	}
	done, err := beginAssistantReadReceipt(ctx, tx, assistantOperationChoicePolicy, a, card.Choice != nil)
	if err != nil {
		return false, err
	}
	return done, tx.Commit()
}

func (s *Store) FailAssistantAdviceChoice(ctx context.Context, a AssistantReadCardAction) error {
	return s.failAssistantReadReceipt(ctx, assistantOperationChoicePolicy, a)
}

func (s *Store) FinishAssistantAdviceChoice(ctx context.Context, a AssistantReadCardAction, decision string) error {
	if decision != "record_selection" && decision != "request_details" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	card, err := assistantAdviceCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return err
	}
	if card.Archived || card.Revision != a.Revision || !card.HasSlot(a.Target) || card.Choice != nil || card.Draft.ExpiresAt <= now() {
		return ErrConversationRevision
	}
	if err = finishAssistantReadReceipt(ctx, tx, assistantOperationChoicePolicy, a); err != nil {
		return err
	}
	if err = addEvent(tx, a.Run, assistantOperationChoiceEvent, AssistantAdviceChoice{a.Target, decision, now()}); err != nil {
		return err
	}
	return tx.Commit()
}
