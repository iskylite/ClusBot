package work

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	MaxMaterials     = 4
	maxMaterialBytes = 32 * 1024
)

var (
	ErrMaterial          = errors.New("请选择 materials 内不超过 32 KiB 的 UTF-8 文本或 Markdown 文件")
	ErrMaterialsChanged  = errors.New("材料目录或文件已变更，请重新选择")
	ErrMaterialSelection = errors.New("每次最多选择四份有效且不重复的材料")
)

type Material struct {
	ID         string      `json:"id"`
	Path       string      `json:"path"`
	Digest     string      `json:"sha256"`
	Size       int         `json:"size"`
	CapturedAt int64       `json:"captured_at"`
	Content    string      `json:"-"`
	Upload     *UploadPart `json:"upload,omitempty"`
}

type MaterialFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func (s *Store) MaterialFiles(query string) ([]MaterialFile, error) {
	if !s.materials.valid() {
		return nil, ErrMaterialsChanged
	}
	dir, err := s.materials.root.Open(".")
	if err != nil {
		return nil, ErrMaterialsChanged
	}
	defer dir.Close()

	result := []MaterialFile{}
	query = strings.ToLower(strings.TrimSpace(query))
	// Limit directory scanning as well as the response size.
	for scanned := 0; scanned < 4096 && len(result) < 100; {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			scanned++
			if !validFilename(entry.Name()) || !entry.Type().IsRegular() || !strings.Contains(strings.ToLower(entry.Name()), query) {
				continue
			}
			info, err := entry.Info()
			if err == nil && materialInfo(info) {
				result = append(result, MaterialFile{Path: "materials/" + entry.Name(), Size: info.Size()})
				if len(result) == 100 {
					break
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, ErrMaterialsChanged
		}
	}
	slices.SortFunc(result, func(a, b MaterialFile) int { return strings.Compare(a.Path, b.Path) })
	return result, nil
}

func materialInfo(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= maxMaterialBytes && info.Sys().(*syscall.Stat_t).Nlink == 1
}

func (s *Store) CaptureMaterial(ctx context.Context, path string) (Material, error) {
	data, err := s.readMaterial(path)
	if err != nil {
		return Material{}, err
	}
	return saveMaterial(ctx, s.db, path, data, nil)
}

func (s *Store) readMaterial(path string) ([]byte, error) {
	name, ok := strings.CutPrefix(path, "materials/")
	if !ok || !validFilename(name) || filepath.Base(name) != name {
		return nil, ErrMaterial
	}
	root := s.materials
	if !root.valid() {
		return nil, ErrMaterialsChanged
	}
	before, err := root.root.Lstat(name)
	if err != nil || !materialInfo(before) {
		return nil, ErrMaterial
	}
	f, err := root.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrMaterialsChanged
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !materialInfo(opened) || !os.SameFile(before, opened) {
		return nil, ErrMaterialsChanged
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMaterialBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxMaterialBytes || !utf8.Valid(data) || slices.Contains(data, 0) {
		return nil, ErrMaterial
	}
	after, err := f.Stat()
	current, pathErr := root.root.Lstat(name)
	if err != nil || pathErr != nil || !root.valid() || !materialInfo(current) || !os.SameFile(current, opened) ||
		after.Size() != int64(len(data)) || !after.ModTime().Equal(opened.ModTime()) {
		return nil, ErrMaterialsChanged
	}
	return data, nil
}

type materialWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Image snapshots share material identity and retention with text snapshots.
func (m Material) ImageURL() string {
	if strings.HasPrefix(m.Path, "images/") {
		return m.Content
	}
	return ""
}

func (s *Store) UploadImage(ctx context.Context, name string, data []byte) (Material, error) {
	if !safeFilename(name) {
		return Material{}, ErrImageFormat
	}
	if len(data) > 256*1024 {
		return Material{}, ErrImageSize
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") ||
		config.Width <= 0 || config.Height <= 0 || config.Width > 4096 || config.Height > 4096 {
		return Material{}, ErrImageFormat
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return Material{}, ErrImageFormat
	}
	content := "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(data)
	return saveMaterial(ctx, s.db, "images/"+name, []byte(content), nil)
}

var (
	ErrImageSize        = errors.New("图片不能超过 256 KiB")
	ErrImageFormat      = errors.New("请选择有效的 PNG 或 JPEG 图片，宽高各不超过 4096 像素")
	ErrImageUnsupported = errors.New("当前模型未启用图片输入")
)

func saveMaterial(ctx context.Context, q materialWriter, path string, data []byte, upload *UploadPart) (Material, error) {
	m := Material{ID: id(), Path: path, Digest: digest(string(data)), Size: len(data), CapturedAt: now(), Upload: upload}
	var uploadID, part any
	if upload != nil {
		uploadID, part = upload.ID, upload.Part
	}
	_, err := q.ExecContext(ctx, `INSERT INTO materials(id,path,sha256,content,captured_at,upload_id,upload_part) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(path,sha256) DO NOTHING`, m.ID, path, m.Digest, string(data), m.CapturedAt, uploadID, part)
	if err != nil {
		return Material{}, err
	}
	err = q.QueryRowContext(ctx, "SELECT id,captured_at FROM materials WHERE path=? AND sha256=?", path, m.Digest).Scan(&m.ID, &m.CapturedAt)
	return m, err
}

func attachMaterials(tx *sql.Tx, seq int64, ids []string) error {
	if len(ids) > MaxMaterials {
		return ErrMaterialSelection
	}
	seen := map[string]bool{}
	for ordinal, key := range ids {
		if seen[key] {
			return ErrMaterialSelection
		}
		seen[key] = true
		res, err := tx.Exec(`INSERT INTO message_materials(message_seq,material_id,ordinal)
SELECT ?,id,? FROM materials WHERE id=?`, seq, ordinal, key)
		if err != nil {
			return err
		}
		count, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrMaterialSelection
		}
	}
	return nil
}

func (s *Store) modelMessages(ctx context.Context, conversationID string) ([]Message, error) {
	return s.modelMessagesForRun(ctx, conversationID, "")
}

func (s *Store) modelMessagesForRun(ctx context.Context, conversationID, runID string) ([]Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	boundary, err := readRerunInputBoundary(tx, runID)
	if err != nil {
		return nil, err
	}
	var items []Message
	if boundary == nil {
		items, err = queryMessages(tx, false, `SELECT seq,role,text,replace_materials,run_id FROM messages
WHERE conversation_id=? AND NOT EXISTS(SELECT 1 FROM isolated_reply_pins p WHERE p.run_id=messages.run_id) AND (
  seq IN (SELECT seq FROM messages recent WHERE recent.conversation_id=? AND recent.run_id NOT IN
    (SELECT id FROM runs WHERE memory_command=1)
    AND NOT EXISTS(SELECT 1 FROM isolated_reply_pins p WHERE p.run_id=recent.run_id) ORDER BY seq DESC LIMIT 50)
  OR seq IN (SELECT message_seq FROM input_deliveries WHERE run_id=? AND generation=?
    EXCEPT SELECT message_seq FROM input_dispositions)
)`, conversationID, conversationID, runID, executionGeneration(ctx))
	} else {
		// A completed-task rerun has a fixed source boundary. Where available,
		// use the source Run's actual model manifest; legacy Runs reconstruct
		// their preceding history. Its final answer is never a new instruction.
		items, err = queryMessages(tx, false, `SELECT seq,role,text,replace_materials,run_id FROM messages
WHERE conversation_id=? AND NOT EXISTS(SELECT 1 FROM isolated_reply_pins p WHERE p.run_id=messages.run_id)
AND ((run_id IN (SELECT id FROM runs WHERE conversation_id=? AND memory_command=0
                 AND rowid<=(SELECT rowid FROM runs WHERE id=?))
      AND NOT (run_id=? AND role='assistant')
      AND (coalesce(json_type((SELECT manifest FROM context_manifests WHERE run_id=?
           AND generation=(SELECT execution_generation FROM runs WHERE id=?) ORDER BY id DESC LIMIT 1), '$.messages'),'null')<>'array'
        OR seq IN (SELECT value FROM json_each((SELECT manifest FROM context_manifests WHERE run_id=?
           AND generation=(SELECT execution_generation FROM runs WHERE id=?) ORDER BY id DESC LIMIT 1), '$.messages'))))
     OR run_id IN (?,?)) ORDER BY seq DESC LIMIT 50`, conversationID, conversationID,
			boundary.SourceRunID, boundary.SourceRunID, boundary.SourceRunID, boundary.SourceRunID,
			boundary.SourceRunID, boundary.SourceRunID, boundary.RootRunID, runID)
	}
	if err != nil {
		return nil, err
	}
	// Material selection is persisted in message_materials. Resolve it from the
	// entire conversation before applying either of the history display limits.
	var materials []Material
	if boundary == nil {
		materials, err = activeMaterials(ctx, tx, conversationID)
	} else {
		materials, err = completedRunMaterials(ctx, tx, conversationID, boundary.RootRunID, true)
	}
	if err != nil {
		return nil, err
	}
	lastUser := -1
	for i := range items {
		if items[i].Incomplete {
			items[i].Text = "[上一条回复已被用户停止，以下只是未完成的片段]\n" + items[i].Text
		}
		if items[i].Role == "user" {
			lastUser = i
		}
	}
	if lastUser < 0 {
		return nil, errors.New("run has no user message")
	}
	pending, err := pendingInputs(tx, runID, executionGeneration(ctx))
	if err != nil {
		return nil, err
	}
	firstRequired := lastUser
	for i := range items {
		if pending[items[i].ID] {
			items[i].RequiredInput = true
			firstRequired = min(firstRequired, i)
		}
	}
	// Reserve the required active set before deciding how much history fits.
	const maxContextBytes = 256 * 1024
	used := 0
	for _, material := range materials {
		if material.ImageURL() == "" {
			used += len(material.Content)
		}
	}
	for _, item := range items[firstRequired:] {
		used += len(item.Text)
	}
	if used > maxContextBytes {
		return nil, ErrModelRequestBudget
	}
	first := firstRequired
	for i := firstRequired - 1; i >= 0; i-- {
		if len(items[i].Text) > maxContextBytes-used {
			break
		}
		first, used = i, used+len(items[i].Text)
	}
	if first < lastUser && items[first].Role == "assistant" {
		first++
	}
	// Preserve existing material positions/metadata when their source message
	// survives, keeping existing checkpoint context identities stable. Missing
	// sources are carried on the required input instead of silently discarded.
	active := make(map[string]Material, len(materials))
	for _, material := range materials {
		active[material.ID] = material
	}
	loaded := make(map[string]bool, len(materials))
	for i := len(items) - 1; i >= first; i-- {
		for j := range items[i].Materials {
			material := &items[i].Materials[j]
			if snapshot, ok := active[material.ID]; ok && !loaded[material.ID] {
				material.Content = snapshot.Content
				loaded[material.ID] = true
			}
		}
	}
	for _, material := range materials {
		if !loaded[material.ID] {
			items[lastUser].Materials = append(items[lastUser].Materials, material)
		}
	}
	return items[first:], tx.Commit()
}

func activeMaterials(ctx context.Context, tx *sql.Tx, conversationID string) ([]Material, error) {
	rows, err := tx.QueryContext(ctx, `WITH active AS (
SELECT refs.material_id,max(msg.seq) AS latest_seq
FROM message_materials refs JOIN messages msg ON msg.seq=refs.message_seq
WHERE msg.conversation_id=? AND msg.seq >= coalesce((
 SELECT max(seq) FROM messages WHERE conversation_id=? AND replace_materials=1),0)
GROUP BY refs.material_id)
SELECT m.id,m.path,m.sha256,m.content,m.captured_at,
CASE WHEN u.id IS NULL THEN 'null' ELSE json_object('id',u.id,'name',u.name,'sha256',u.sha256,
 'bytes',u.size,'part',m.upload_part,'parts',u.parts) END
FROM active JOIN materials m ON m.id=active.material_id
JOIN message_materials refs ON refs.message_seq=active.latest_seq AND refs.material_id=m.id
LEFT JOIN uploads u ON u.id=m.upload_id
ORDER BY active.latest_seq DESC,refs.ordinal LIMIT ?`, conversationID, conversationID, MaxMaterials)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var materials []Material
	for rows.Next() {
		var material Material
		var upload string
		if err = rows.Scan(&material.ID, &material.Path, &material.Digest, &material.Content, &material.CapturedAt, &upload); err != nil {
			return nil, err
		}
		if digest(material.Content) != material.Digest {
			return nil, errors.New("material snapshot checksum mismatch")
		}
		if err = json.Unmarshal([]byte(upload), &material.Upload); err != nil {
			return nil, err
		}
		material.Size = len(material.Content)
		materials = append(materials, material)
	}
	return materials, rows.Err()
}

// MaterialPreview 表示材料或附件的统一结构化预览内容（同时服务于待发送 Composer 芯片与已发送气泡附件）。
type MaterialPreview struct {
	ID           string     `json:"id"`
	Path         string     `json:"path"`
	Name         string     `json:"name"`
	Size         int        `json:"size"`
	Digest       string     `json:"sha256"`
	PreviewKind  string     `json:"preview_kind"` // csv, log, markdown, image, text
	Bound        bool       `json:"bound_to_message"`
	TotalLines   int        `json:"total_lines,omitempty"`
	Truncated    bool       `json:"truncated,omitempty"`
	Columns      []string   `json:"columns,omitempty"`
	Rows         [][]string `json:"rows,omitempty"`
	Lines        []string   `json:"lines,omitempty"`
	ImageDataURL string     `json:"image_data_url,omitempty"`
}

// MaterialContent reads the original contents and metadata from the caller’s store.
func (s *Store) MaterialContent(ctx context.Context, materialID string) (preview MaterialPreview, content string, err error) {
	var boundCount int
	err = s.db.QueryRowContext(ctx, `
		SELECT m.path, m.sha256, m.content,
		       (SELECT COUNT(*) FROM message_materials mm WHERE mm.material_id=m.id)
		FROM materials m WHERE m.id=?`, materialID).Scan(&preview.Path, &preview.Digest, &content, &boundCount)
	if err != nil {
		return MaterialPreview{}, "", err
	}
	preview.ID = materialID
	preview.Name = filepath.Base(preview.Path)
	if strings.HasPrefix(preview.Path, "uploads/") {
		if idx := strings.LastIndex(preview.Name, "#"); idx > 0 {
			preview.Name = preview.Name[:idx]
		}
	}
	preview.Size = len(content)
	preview.Bound = boundCount > 0
	return preview, content, nil
}

// PreviewMaterial returns a bounded structural preview of a captured material.
func (s *Store) PreviewMaterial(ctx context.Context, materialID string, limit int) (MaterialPreview, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	out, content, err := s.MaterialContent(ctx, materialID)
	if err != nil {
		return MaterialPreview{}, err
	}
	lower := strings.ToLower(out.Name)
	if strings.HasPrefix(out.Path, "images/") || strings.HasPrefix(content, "data:image/") {
		out.PreviewKind = "image"
		out.ImageDataURL = content
		return out, nil
	}
	if strings.HasSuffix(lower, ".csv") || strings.HasSuffix(lower, ".tsv") {
		out.PreviewKind = "csv"
		reader := csv.NewReader(strings.NewReader(content))
		if strings.HasSuffix(lower, ".tsv") {
			reader.Comma = '\t'
		}
		records, parseErr := reader.ReadAll()
		if parseErr == nil && len(records) > 0 {
			out.TotalLines = len(records)
			out.Columns = records[0]
			maxRows := limit
			if len(records)-1 > maxRows {
				out.Rows = records[1 : 1+maxRows]
				out.Truncated = true
			} else if len(records) > 1 {
				out.Rows = records[1:]
			}
			return out, nil
		}
	}
	switch {
	case strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown"):
		out.PreviewKind = "markdown"
	case strings.HasSuffix(lower, ".log"):
		out.PreviewKind = "log"
	default:
		out.PreviewKind = "text"
	}
	rawLines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	out.TotalLines = len(rawLines)
	if len(rawLines) > limit {
		out.Lines = rawLines[:limit]
		out.Truncated = true
	} else {
		out.Lines = rawLines
	}
	return out, nil
}

// DeletePendingMaterial 移除尚未随任何消息发送出去的待发材料，防止误删已持久化历史对话证据。
func (s *Store) DeletePendingMaterial(ctx context.Context, materialID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_materials WHERE material_id=?`, materialID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return ErrState
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM materials WHERE id=?`, materialID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
