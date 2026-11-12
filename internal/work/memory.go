package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrMemoryInvalid   = errors.New("请输入类别、1 至 60 字的标题和 1 至 1000 字的内容")
	ErrMemoryLimit     = errors.New("最多可保存 100 条记忆，其中偏好最多 12 条")
	ErrMemoryDuplicate = errors.New("该类别已有同名记忆，请编辑现有条目或更换标题")
	ErrMemoryRevision  = errors.New("这条记忆已被其他页面修改或删除。请载入最新版本后重试")
)

type Memory struct {
	ID            string        `json:"id"`
	Kind          string        `json:"kind"`
	Title         string        `json:"title"`
	Content       string        `json:"content"`
	Pinned        bool          `json:"pinned"`
	Revision      int           `json:"revision"`
	UpdatedAt     int64         `json:"updated_at"`
	Source        *MemorySource `json:"source,omitempty"`
	SourceDeleted bool          `json:"source_deleted,omitempty"`
}

type MemorySource struct {
	MessageID int64  `json:"message_id"`
	Path      string `json:"path"`
	Title     string `json:"title"`
	Text      string `json:"text,omitempty"`
}

type MemoryRef struct {
	ID              string `json:"id"`
	Revision        int    `json:"revision"`
	Title           string `json:"title,omitempty"`
	CurrentRevision int    `json:"current_revision"`
}

const memorySelect = `SELECT m.id,m.kind,m.title,m.content,m.pinned,m.revision,m.updated_at,m.source_deleted,
coalesce(msg.seq,0),coalesce(c.id,''),coalesce(c.title,''),coalesce(t.id,'')
FROM memories m LEFT JOIN messages msg ON msg.seq=m.source_message
LEFT JOIN conversations c ON c.id=msg.conversation_id LEFT JOIN tasks t ON t.conversation_id=c.id`

func sourcePath(conversation, task string) string {
	if task != "" {
		return "/tasks/" + task
	}
	return "/chat/" + conversation
}

func scanMemory(row scanner) (Memory, error) {
	var m Memory
	var source MemorySource
	var conversation, task string
	err := row.Scan(&m.ID, &m.Kind, &m.Title, &m.Content, &m.Pinned, &m.Revision, &m.UpdatedAt, &m.SourceDeleted, &source.MessageID, &conversation, &source.Title, &task)
	if source.MessageID != 0 {
		source.Path = sourcePath(conversation, task)
		m.Source = &source
	}
	return m, err
}

type memoryQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listMemories(ctx context.Context, q memoryQueryer) ([]Memory, error) {
	rows, err := q.QueryContext(ctx, memorySelect+" ORDER BY m.pinned DESC,m.updated_at DESC,m.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

func (s *Store) Memories(ctx context.Context) ([]Memory, error) {
	return listMemories(ctx, s.db)
}

func (s *Store) MemorySource(ctx context.Context, message int64) (MemorySource, error) {
	var src MemorySource
	var conversation, task string
	err := s.db.QueryRowContext(ctx, `SELECT m.seq,m.text,c.id,c.title,coalesce(t.id,'')
FROM messages m JOIN conversations c ON c.id=m.conversation_id LEFT JOIN tasks t ON t.conversation_id=c.id
WHERE m.seq=? AND m.role='user'`, message).Scan(&src.MessageID, &src.Text, &conversation, &src.Title, &task)
	src.Path = sourcePath(conversation, task)
	return src, err
}

func (s *Store) SaveMemory(ctx context.Context, key string, m Memory, sourceID int64) (Memory, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Memory{}, err
	}
	defer tx.Rollback()
	saved, err := saveMemory(ctx, tx, key, m, sourceID)
	if err != nil {
		return Memory{}, err
	}
	return saved, tx.Commit()
}

func saveMemory(ctx context.Context, tx *sql.Tx, key string, m Memory, sourceID int64) (Memory, error) {
	m.Title, m.Content = strings.TrimSpace(m.Title), strings.TrimSpace(m.Content)
	if (m.Kind != "preference" && m.Kind != "fact") || m.Title == "" || m.Content == "" ||
		utf8.RuneCountInString(m.Title) > 60 || utf8.RuneCountInString(m.Content) > 1000 ||
		(key != "" && (m.Revision < 1 || sourceID != 0)) || sourceID < 0 {
		return Memory{}, ErrMemoryInvalid
	}
	if key != "" {
		var current int
		err := tx.QueryRowContext(ctx, "SELECT revision FROM memories WHERE id=?", key).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && current != m.Revision) {
			return Memory{}, ErrMemoryRevision
		}
		if err != nil {
			return Memory{}, err
		}
	}
	var total, preferences int
	var duplicate bool
	err := tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE kind='preference'),
coalesce(max(kind=? AND title=?),0) FROM memories WHERE id<>?`, m.Kind, m.Title, key).Scan(&total, &preferences, &duplicate)
	if err != nil {
		return Memory{}, err
	}
	if duplicate {
		return Memory{}, ErrMemoryDuplicate
	}
	if total >= 100 || (m.Kind == "preference" && preferences >= 12) {
		return Memory{}, ErrMemoryLimit
	}
	if key == "" {
		var source any
		if sourceID != 0 {
			var valid bool
			if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM messages WHERE seq=? AND role='user')", sourceID).Scan(&valid); err != nil {
				return Memory{}, err
			}
			if !valid {
				return Memory{}, sql.ErrNoRows
			}
			source = sourceID
		}
		key = id()
		_, err = tx.ExecContext(ctx, "INSERT INTO memories(id,kind,title,content,pinned,source_message,updated_at) VALUES(?,?,?,?,?,?,?)",
			key, m.Kind, m.Title, m.Content, m.Pinned, source, now())
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE memories SET kind=?,title=?,content=?,pinned=?,revision=revision+1,updated_at=? WHERE id=?",
			m.Kind, m.Title, m.Content, m.Pinned, now(), key)
	}
	if err != nil {
		return Memory{}, err
	}
	return scanMemory(tx.QueryRowContext(ctx, memorySelect+" WHERE m.id=?", key))
}

func (s *Store) DeleteMemory(ctx context.Context, key string, revision int) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM memories WHERE id=? AND revision=?", key, revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrMemoryRevision
	}
	return err
}

func matchingMemories(items []Memory, query string) []Memory {
	query = strings.ToLower(query)
	selected := make([]Memory, 0, 16)
	for _, m := range items {
		if m.Kind == "preference" {
			selected = append(selected, m)
		}
	}
	facts := 0
	for _, m := range items {
		if facts == 4 {
			break
		}
		if m.Kind != "fact" {
			continue
		}
		terms := strings.FieldsFunc(strings.ToLower(m.Title), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		for _, term := range terms {
			if utf8.RuneCountInString(term) >= 2 && strings.Contains(query, term) {
				selected = append(selected, m)
				facts++
				break
			}
		}
	}
	return selected
}

const memoryRefsJSON = `SELECT json_group_array(json_object(
	'id',ref.memory_id,'revision',ref.revision,'title',ref.title,'current_revision',coalesce(m.revision,0)
) ORDER BY ref.rowid)
FROM run_memories ref LEFT JOIN memories m ON m.id=ref.memory_id WHERE ref.run_id=`

func readMemoryRefs(tx *sql.Tx, run string) ([]MemoryRef, error) {
	var value string
	if err := tx.QueryRow(memoryRefsJSON+"?", run).Scan(&value); err != nil {
		return nil, err
	}
	var refs []MemoryRef
	err := json.Unmarshal([]byte(value), &refs)
	return refs, err
}
