package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

const IsolatedReplyMode = "only_this_message"

var (
	ErrIsolatedReplyInput       = errors.New("仅本条消息模式需要明确选择模型版本并同意只发送这条新消息")
	ErrIsolatedReplyUnavailable = errors.New("本次回复的模型档案、凭据或会话已变化，未发送；请重新选择并提交")
)

type IsolatedReplyInput struct {
	Mode                     string `json:"mode"`
	ProfileID                string `json:"profile_id"`
	ProfileRevision          int    `json:"profile_revision"`
	ConfirmCurrentTextEgress bool   `json:"confirm_current_text_egress"`
	RequestID                string `json:"client_request_id"`
	Text                     string `json:"text"`
}

type IsolatedReplyPin struct {
	ProfileID         string
	ProfileRevision   int
	CredentialBinding string
}

type isolatedReplyProvider interface {
	IsolatedReplyProvider(context.Context, IsolatedReplyPin) (Provider, error)
}

func migrateIsolatedReplies(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS isolated_reply_pins (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 conversation_id TEXT NOT NULL, actor TEXT NOT NULL,
 profile_id TEXT NOT NULL, profile_revision INTEGER NOT NULL, credential_binding TEXT NOT NULL,
 input_mode TEXT NOT NULL CHECK(input_mode='only_this_message'), text_digest TEXT NOT NULL
)`)
	return err
}

func (s *Store) SubmitIsolatedReply(ctx context.Context, actor, conversation string, in IsolatedReplyInput,
	resolve func(context.Context, string, int) (string, error)) (Run, error) {
	if actor != "user:session" {
		return Run{}, ErrAuthorizationDenied
	}
	if conversation == "" || len(in.ProfileID) != 32 || in.ProfileRevision < 1 || in.Mode != IsolatedReplyMode ||
		!in.ConfirmCurrentTextEgress || len(in.RequestID) < 8 || len(in.RequestID) > 128 || !utf8.ValidString(in.Text) ||
		strings.TrimSpace(in.Text) == "" || strings.HasPrefix(strings.TrimSpace(in.Text), "/") ||
		utf8.RuneCountInString(in.Text) > 16000 || resolve == nil {
		return Run{}, ErrIsolatedReplyInput
	}
	key := "isolated-reply:" + digest(in.RequestID)
	fingerprint := digest(toJSON([]any{actor, conversation, in}))
	credential, resolveErr := resolve(ctx, in.ProfileID, in.ProfileRevision)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if previous, err := previousSubmission(tx, key, fingerprint); err != nil || previous.ID != "" {
		return previous, err
	}
	if err := checkConversationSubmission(tx, conversation); err != nil {
		return Run{}, err
	}
	if resolveErr != nil || credential == "" {
		return Run{}, ErrIsolatedReplyUnavailable
	}
	if err := validateIsolatedReplyScope(tx, conversation, in.ProfileID, in.ProfileRevision); err != nil {
		return Run{}, err
	}
	run, err := submitWorkTxWithMode(ctx, tx, key, fingerprint, in.Text, "", conversation, false, Delivery{}, nil, nil, true)
	if err != nil {
		return Run{}, err
	}
	if run.TaskID != "" {
		return Run{}, ErrIsolatedReplyUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO isolated_reply_pins VALUES(?,?,?,?,?,?,?,?)`,
		run.ID, conversation, actor, in.ProfileID, in.ProfileRevision, credential, in.Mode, digest(in.Text)); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func validateIsolatedReplyScope(q recordsQuerier, conversation, profile string, revision int) error {
	var eligible bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM conversations c
WHERE c.id=? AND c.archived=0
AND EXISTS(SELECT 1 FROM model_connection_profiles p WHERE p.profile_id=? AND p.revision=?))`,
		conversation, profile, revision).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrIsolatedReplyUnavailable
	}
	return nil
}

func readIsolatedReplyPin(q recordsQuerier, run Run) (IsolatedReplyPin, string, error) {
	var pin IsolatedReplyPin
	var conversation, actor, mode, textDigest string
	err := q.QueryRow(`SELECT conversation_id,actor,profile_id,profile_revision,credential_binding,input_mode,text_digest
FROM isolated_reply_pins WHERE run_id=?`, run.ID).Scan(&conversation, &actor, &pin.ProfileID, &pin.ProfileRevision, &pin.CredentialBinding, &mode, &textDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return IsolatedReplyPin{}, "", nil
	}
	if err != nil {
		return pin, "", err
	}
	// submitWorkTx creates the initial generation via the existing zero
	// default; this path deliberately never creates a resumable checkpoint.
	if run.TaskID != "" || run.Generation != 0 || conversation != run.ConversationID || actor != "user:session" || mode != IsolatedReplyMode {
		return pin, "", ErrIsolatedReplyUnavailable
	}
	if err := validateIsolatedReplyScope(q, conversation, pin.ProfileID, pin.ProfileRevision); err != nil {
		return pin, "", err
	}
	return pin, textDigest, nil
}

func (s *Store) isolatedReplyInput(ctx context.Context, run Run) (IsolatedReplyPin, []Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IsolatedReplyPin{}, nil, err
	}
	defer tx.Rollback()
	pin, d, err := readIsolatedReplyPin(tx, run)
	if err != nil || pin.ProfileID == "" {
		return pin, nil, err
	}
	var n int
	var id int64
	var text string
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(seq),0),coalesce(min(text),'') FROM messages
WHERE run_id=? AND conversation_id=? AND role='user'
AND replace_materials=0
AND NOT EXISTS(SELECT 1 FROM message_materials WHERE message_seq=messages.seq)
AND NOT EXISTS(SELECT 1 FROM message_task_refs WHERE message_seq=messages.seq)`,
		run.ID, run.ConversationID).Scan(&n, &id, &text); err != nil {
		return pin, nil, err
	}
	if n != 1 || digest(text) != d {
		return pin, nil, ErrIsolatedReplyUnavailable
	}
	return pin, []Message{{ID: id, Role: "user", Text: text, RequiredInput: true}}, nil
}
