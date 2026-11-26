package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const assistantArtifactListEvent = "assistant.artifact_list_snapshot"
const assistantArtifactPreviewEvent = "assistant.artifact_preview_receipt:"
const assistantArtifactCardLimit = 10
const assistantArtifactPreviewChars = 600
const assistantArtifactPreviewLines = 8

var artifactCardReceiptPolicy = assistantReadReceiptPolicy{"assistant-artifact-preview", "preview_artifact_text", 3 * assistantArtifactCardLimit}

type assistantArtifactRef struct{ ID, Digest string }

type AssistantArtifactCard struct {
	Refs       []assistantArtifactRef
	More       bool
	Revision   int64                               `json:"-"`
	Suppressed bool                                `json:"-"`
	Rows       []AssistantArtifactCardRow          `json:"-"`
	Previews   map[string]AssistantArtifactPreview `json:"-"`
}

type AssistantArtifactCardRow struct {
	Slot, Name, TaskTitle string
	Accepted              bool
}

type AssistantArtifactPreview struct {
	Decision   string
	Lines      []string `json:"-"`
	Truncated  bool     `json:"-"`
	Unreadable bool     `json:"-"`
}

// Read in the same private Store as the existing artifact API. Reconcile
// source access with the same (current) MCP authority, not the card snapshot.
const assistantArtifactSource = ` FROM artifacts a JOIN tasks t ON t.id=a.task_id WHERE NOT `

func (s *Store) OpenAssistantArtifactList(ctx context.Context, actor, conversation, request, text string) (Run, error) {
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
	key := "assistant-artifact-list:" + digest(toJSON([]string{actor, request}))
	fp := digest(toJSON([]string{assistantArtifactListEvent, conversation, text}))
	if prior, err := previousSubmission(tx, key, fp); err != nil || prior.ID != "" {
		return prior, err
	}
	card, err := snapshotAssistantArtifacts(ctx, tx)
	if err != nil {
		return Run{}, err
	}
	intent := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{
		Kind: "clarify", Question: "我把你可查看的成果列在下面。可以预览一项，不会替你验收或保存文件。"}}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fp, text, "", conversation, false, Delivery{}, &intent, nil, true)
	if err != nil {
		return Run{}, err
	}
	if err := addEvent(tx, run.ID, assistantArtifactListEvent, card); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func snapshotAssistantArtifacts(ctx context.Context, tx *sql.Tx) (AssistantArtifactCard, error) {
	var card AssistantArtifactCard
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.sha256`+assistantArtifactSource+revokedMCPReply("a.run_id")+
		" ORDER BY a.rowid DESC LIMIT ?", assistantArtifactCardLimit+1)
	if err != nil {
		return card, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(card.Refs) == assistantArtifactCardLimit {
			card.More = true
			break
		}
		var ref assistantArtifactRef
		if err := rows.Scan(&ref.ID, &ref.Digest); err != nil {
			return card, err
		}
		card.Refs = append(card.Refs, ref)
	}
	return card, rows.Err()
}

func assistantArtifactCardTx(ctx context.Context, tx *sql.Tx, actor, conversation, run string) (AssistantArtifactCard, error) {
	var card AssistantArtifactCard
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return card, err
	}
	var snapshot string
	err := tx.QueryRowContext(ctx, `SELECT c.revision,e.data FROM conversations c JOIN runs r ON r.conversation_id=c.id
JOIN run_events e ON e.run_id=r.id AND e.kind=? WHERE c.id=? AND r.id=?`, assistantArtifactListEvent, conversation, run).Scan(&card.Revision, &snapshot)
	if err != nil {
		return card, err
	}
	if err := json.Unmarshal([]byte(snapshot), &card); err != nil {
		return card, err
	}
	if len(card.Refs) > assistantArtifactCardLimit {
		return card, ErrState
	}
	card.Previews = make(map[string]AssistantArtifactPreview)
	for i, ref := range card.Refs {
		row := AssistantArtifactCardRow{Slot: strconv.Itoa(i + 1)}
		err := tx.QueryRowContext(ctx, `SELECT a.name,t.title,EXISTS(SELECT 1 FROM task_acceptances x WHERE x.task_id=a.task_id AND x.artifact_id=a.id AND x.sha256=a.sha256)`+
			assistantArtifactSource+revokedMCPReply("a.run_id")+" AND a.id=? AND a.sha256=?", ref.ID, ref.Digest).
			Scan(&row.Name, &row.TaskTitle, &row.Accepted)
		if errors.Is(err, sql.ErrNoRows) {
			card.Suppressed = true
			continue
		}
		if err != nil {
			return card, err
		}
		card.Rows = append(card.Rows, row)
		var raw string
		err = tx.QueryRowContext(ctx, "SELECT data FROM run_events WHERE run_id=? AND kind=? ORDER BY seq DESC LIMIT 1", run, assistantArtifactPreviewEvent+row.Slot).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return card, err
		}
		var preview AssistantArtifactPreview
		if err := json.Unmarshal([]byte(raw), &preview); err != nil {
			return card, err
		}
		if preview.Decision == "show_preview" {
			if err := assistantArtifactText(ctx, tx, ref, &preview); err != nil {
				return card, err
			}
		}
		card.Previews[row.Slot] = preview
	}
	return card, nil
}

func assistantArtifactText(ctx context.Context, tx *sql.Tx, ref assistantArtifactRef, out *AssistantArtifactPreview) error {
	var text, storageType string
	var hasNull bool
	err := tx.QueryRowContext(ctx, `SELECT substr(a.content,1,?),typeof(a.content),instr(a.content,char(0))>0`+
		assistantArtifactSource+revokedMCPReply("a.run_id")+" AND a.id=? AND a.sha256=?",
		assistantArtifactPreviewChars+1, ref.ID, ref.Digest).Scan(&text, &storageType, &hasNull)
	if err != nil {
		return err
	}
	if storageType != "text" || hasNull || !utf8.ValidString(text) || strings.ContainsFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\t' && r != '\r' && r != '\n'
	}) {
		out.Unreadable = true
		return nil
	}
	runes := []rune(text)
	if len(runes) > assistantArtifactPreviewChars {
		out.Truncated = true
		runes = runes[:assistantArtifactPreviewChars]
	}
	text = strings.ReplaceAll(strings.ReplaceAll(string(runes), "\r\n", "\n"), "\r", "\n")
	out.Lines = strings.Split(text, "\n")
	if len(out.Lines) > assistantArtifactPreviewLines {
		out.Truncated = true
		out.Lines = out.Lines[:assistantArtifactPreviewLines]
	}
	return nil
}

func (s *Store) AssistantArtifactCard(ctx context.Context, actor, conversation, run string) (AssistantArtifactCard, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AssistantArtifactCard{}, err
	}
	defer tx.Rollback()
	return assistantArtifactCardTx(ctx, tx, actor, conversation, run)
}

func (c AssistantArtifactCard) HasSlot(slot string) bool {
	for _, row := range c.Rows {
		if slot == row.Slot {
			return true
		}
	}
	return false
}

func (s *Store) BeginAssistantArtifactPreview(ctx context.Context, a AssistantReadCardAction) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	card, err := assistantArtifactCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return false, err
	}
	if a.Revision != card.Revision || !card.HasSlot(a.Target) {
		return false, ErrConversationRevision
	}
	_, restored := card.Previews[a.Target]
	done, err := beginAssistantReadReceipt(ctx, tx, artifactCardReceiptPolicy, a, restored)
	if err != nil {
		return false, err
	}
	return done, tx.Commit()
}

func (s *Store) FailAssistantArtifactPreview(ctx context.Context, a AssistantReadCardAction) error {
	return s.failAssistantReadReceipt(ctx, artifactCardReceiptPolicy, a)
}

func (s *Store) FinishAssistantArtifactPreview(ctx context.Context, a AssistantReadCardAction, decision string) error {
	if decision != "show_preview" && decision != "ask_for_details" && decision != "cannot_prepare" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	card, err := assistantArtifactCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return err
	}
	if a.Revision != card.Revision || !card.HasSlot(a.Target) {
		return ErrConversationRevision
	}
	if err := finishAssistantReadReceipt(ctx, tx, artifactCardReceiptPolicy, a); err != nil {
		return err
	}
	if _, exists := card.Previews[a.Target]; exists {
		return tx.Commit()
	}
	// Only the decision is stored. The content comes from the original
	// artifact on every authorized read, including recovery after a lost ACK.
	if err := addEvent(tx, a.Run, assistantArtifactPreviewEvent+a.Target, AssistantArtifactPreview{Decision: decision}); err != nil {
		return err
	}
	return tx.Commit()
}
