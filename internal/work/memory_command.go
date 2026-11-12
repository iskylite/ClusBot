package work

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ErrMemoryCommandContent = errors.New("请在「记住」后写出要保存的内容")

func memoryCommand(text string) (Memory, bool) {
	text = strings.TrimSpace(text)
	if strings.HasSuffix(text, "？") || strings.HasSuffix(text, "?") {
		return Memory{}, false
	}
	isSeparator := func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("：:，,、", r)
	}
	for _, prefix := range []string{"请帮我记住", "请你记住", "帮我记住", "请记住", "记住"} {
		content, ok := strings.CutPrefix(text, prefix)
		if !ok {
			continue
		}
		first, _ := utf8.DecodeRuneInString(content)
		if prefix == "记住" && strings.ContainsRune("了的吗没", first) {
			return Memory{}, false
		}
		content = strings.TrimLeftFunc(content, isSeparator)
		for _, label := range []string{"这件事", "以下内容"} {
			if remainder, ok := strings.CutPrefix(content, label); ok {
				next, _ := utf8.DecodeRuneInString(remainder)
				if isSeparator(next) {
					content = strings.TrimLeftFunc(remainder, isSeparator)
					break
				}
			}
		}
		switch strings.TrimRight(content, "。！! ") {
		case "吗", "了吗", "了", "没":
			return Memory{}, false
		case "这件事", "这句话", "这一点", "这个", "以下内容", "上面这件事", "刚才那件事", "刚才说的", "":
			return Memory{}, true
		}
		kind := "fact"
		for _, preference := range []string{"以后", "今后", "默认", "每次", "我喜欢", "我更喜欢", "我偏好", "我的偏好", "我习惯"} {
			if strings.HasPrefix(content, preference) {
				kind = "preference"
				break
			}
		}
		name := title(content)
		if kind == "fact" {
			subject := strings.TrimPrefix(content, "我的")
			if at := strings.IndexAny(subject, "：:是在为叫\n，,。"); at > 0 {
				head := strings.TrimSpace(subject[:at])
				if utf8.RuneCountInString(head) >= 2 {
					name = title(head)
				}
			}
		}
		return Memory{Kind: kind, Title: name, Content: content}, true
	}
	return Memory{}, false
}

// SubmitMemoryCommand records the request, memory and reply in one transaction.
// A command is a local conversation turn and does not become a task execution.
func (s *Store) SubmitMemoryCommand(ctx context.Context, key, text, scope, conversationID string) (Run, bool, error) {
	memory, handled := memoryCommand(text)
	if !handled {
		return Run{}, false, nil
	}
	fingerprint := submissionFingerprint(text, scope, conversationID, false)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, true, err
	}
	defer tx.Rollback()
	if previous, err := previousSubmission(tx, key, fingerprint); err != nil || previous.ID != "" {
		return previous, true, err
	}
	if memory.Content == "" {
		return Run{}, true, ErrMemoryCommandContent
	}
	if conversationID == "" {
		conversationID = id()
		if _, err = tx.ExecContext(ctx, "INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)",
			conversationID, title(text), now()); err != nil {
			return Run{}, true, err
		}
	} else if err = checkConversationSubmission(tx, conversationID); err != nil {
		return Run{}, true, err
	}
	run := Run{ID: id(), ConversationID: conversationID, Status: "completed", Response: "已保存到个人记忆。"}
	if _, err = tx.ExecContext(ctx, `INSERT INTO runs(id,conversation_id,status,response,created_at,memory_command,usage_tracked)
VALUES(?,?,?,?,?,1,1)`, run.ID, run.ConversationID, run.Status, run.Response, now()); err != nil {
		return Run{}, true, err
	}
	request, err := tx.ExecContext(ctx, "INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user',?)",
		run.ConversationID, run.ID, text)
	if err != nil {
		return Run{}, true, err
	}
	source, err := request.LastInsertId()
	if err != nil {
		return Run{}, true, err
	}
	if _, err = saveMemory(ctx, tx, "", memory, source); err != nil {
		return Run{}, true, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'assistant',?)",
		run.ConversationID, run.ID, run.Response); err != nil {
		return Run{}, true, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO submissions VALUES(?,?,?)", key, fingerprint, run.ID); err != nil {
		return Run{}, true, err
	}
	if err = addEvent(tx, run.ID, "run.state_changed", map[string]string{"status": run.Status}); err != nil {
		return Run{}, true, err
	}
	return run, true, tx.Commit()
}
