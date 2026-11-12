package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrFilename        = errors.New("请填写 outputs 内的 Markdown 或文本文件名")
	ErrOverwriteTarget = errors.New("无法安全覆盖此文件。请使用 256 KiB 以内的 UTF-8 文本文件，或更换文件名")
	ErrPublicationOpen = errors.New("请先处理当前写入确认或核对结果")
	ErrWorkspace       = errors.New("工作区输出目录已变更或不可访问")
)

type workspace struct {
	path string
	root *os.Root
	info os.FileInfo
	id   string
}

func (s *Store) OpenWorkspace(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	output, err := openWorkspaceDirectory(root, path, "outputs")
	if err != nil {
		return err
	}
	materials, err := openWorkspaceDirectory(root, path, "materials")
	if err != nil {
		output.root.Close()
		return err
	}
	s.workspace, s.materials = output, materials
	return nil
}

func openWorkspaceDirectory(root *os.Root, path, name string) (*workspace, error) {
	if err := root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := root.Lstat(name)
	if err != nil || !info.IsDir() {
		return nil, ErrWorkspace
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		dir.Close()
		return nil, ErrWorkspace
	}
	return &workspace{filepath.Join(path, name), dir, opened, workspaceIdentity(path, opened)}, nil
}

func workspaceIdentity(path string, info os.FileInfo) string {
	stat := info.Sys().(*syscall.Stat_t)
	return digest(fmt.Sprintf("%s:%d:%d", path, stat.Dev, stat.Ino))
}
func (w *workspace) valid() bool {
	if w == nil {
		return false
	}
	current, err := os.Lstat(w.path)
	return err == nil && current.IsDir() && os.SameFile(w.info, current)
}

type Publication struct {
	ID              string  `json:"id"`
	TaskID          string  `json:"task_id"`
	ArtifactID      string  `json:"artifact_id"`
	RunID           string  `json:"run_id"`
	Filename        string  `json:"filename"`
	Digest          string  `json:"sha256"`
	Status          string  `json:"status"`
	Decision        string  `json:"decision,omitempty"`
	Detail          string  `json:"detail,omitempty"`
	ExpiresAt       int64   `json:"expires_at"`
	PreviousDigest  string  `json:"previous_sha256,omitempty"`
	PreviousContent *string `json:"previous_content,omitempty"`
	previousVersion string
	taskRevision    int
	workspaceID     string
}

const publicationColumns = "id,task_id,artifact_id,run_id,filename,sha256,status,decision,detail,expires_at,task_revision,workspace_id,previous_sha256,previous_version"

func scanPublication(row scanner) (Publication, error) {
	var p Publication
	err := row.Scan(&p.ID, &p.TaskID, &p.ArtifactID, &p.RunID, &p.Filename, &p.Digest, &p.Status, &p.Decision, &p.Detail, &p.ExpiresAt, &p.taskRevision, &p.workspaceID, &p.PreviousDigest, &p.previousVersion)
	return p, err
}

func validFilename(name string) bool {
	extension := strings.ToLower(filepath.Ext(name))
	return safeFilename(name) && (extension == ".md" || extension == ".txt")
}

func safeFilename(name string) bool {
	return name != "" && utf8.ValidString(name) && len(name) <= 180 && strings.TrimSpace(name) == name &&
		!strings.HasPrefix(name, ".") && !strings.ContainsAny(name, "/\\:") &&
		strings.IndexFunc(name, unicode.IsControl) < 0
}

func ensureNoPublication(tx *sql.Tx, taskID string) error {
	var exists bool
	err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM publications WHERE task_id=? AND status IN ('pending','dispatched','unknown'))", taskID).Scan(&exists)
	if err == nil && exists {
		return ErrPublicationOpen
	}
	return err
}

func (s *Store) RequestPublication(ctx context.Context, taskID, artifactID, filename, key string, revision int) (Publication, error) {
	if !validFilename(filename) {
		return Publication{}, ErrFilename
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Publication{}, err
	}
	defer tx.Rollback()
	previous, err := scanPublication(tx.QueryRow("SELECT "+publicationColumns+" FROM publications WHERE request_key=?", key))
	if err == nil {
		if previous.TaskID != taskID || previous.ArtifactID != artifactID || previous.Filename != filename {
			return Publication{}, ErrConflict
		}
		return previous, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Publication{}, err
	}
	if !s.workspace.valid() {
		return Publication{}, ErrWorkspace
	}
	if err = ensureNoPublication(tx, taskID); err != nil {
		return Publication{}, err
	}
	if err = ensureReadConfirmed(tx, taskID); err != nil {
		return Publication{}, err
	}

	p := Publication{ID: id(), TaskID: taskID, ArtifactID: artifactID, Filename: filename, Status: "pending",
		ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli(), taskRevision: revision + 1, workspaceID: s.workspace.id}
	var currentArtifact, status, content, taskStatus, requiredFile string
	var currentRevision int
	err = tx.QueryRow(`SELECT t.revision,t.status,r.id,r.status,coalesce(a.id,''),coalesce(a.content,''),coalesce(a.sha256,''),t.required_filename
FROM tasks t JOIN runs r ON r.task_id=t.id LEFT JOIN artifacts a ON a.run_id=r.id WHERE t.id=? ORDER BY r.rowid DESC LIMIT 1`, taskID).
		Scan(&currentRevision, &taskStatus, &p.RunID, &status, &currentArtifact, &content, &p.Digest, &requiredFile)
	if err != nil {
		return Publication{}, err
	}
	if currentRevision != revision || taskStatus != "waiting_user" || status != "completed" || currentArtifact != artifactID || digest(content) != p.Digest {
		return Publication{}, ErrState
	}
	if requiredFile != "" && requiredFile != filename {
		return Publication{}, ErrDeliveryPending
	}
	var before string
	current, err := s.workspace.readOutput(filename)
	switch {
	case err == nil:
		if !utf8.ValidString(current.content) || strings.ContainsRune(current.content, 0) {
			return Publication{}, ErrOverwriteTarget
		}
		p.PreviousDigest, p.previousVersion, before = current.hash, current.version, current.content
	case errors.Is(err, os.ErrNotExist):
	case errors.Is(err, ErrOverwriteTarget):
		return Publication{}, err
	default:
		return Publication{}, ErrWorkspace
	}
	_, err = tx.Exec(`INSERT INTO publications(id,request_key,task_id,artifact_id,run_id,task_revision,workspace_id,filename,sha256,status,expires_at,updated_at,previous_sha256,previous_version,previous_content)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, key, taskID, artifactID, p.RunID, p.taskRevision, p.workspaceID, filename, p.Digest, p.Status, p.ExpiresAt, now(), p.PreviousDigest, p.previousVersion, before)
	if err != nil {
		return Publication{}, err
	}
	if err = publicationEvent(tx, p, "待确认保存到工作区"); err != nil {
		return Publication{}, err
	}
	return p, tx.Commit()
}

func publicationEvent(tx *sql.Tx, p Publication, reason string) error {
	if _, err := tx.Exec(`UPDATE tasks SET status_reason=CASE ?
WHEN 'pending' THEN 'approval_required' WHEN 'dispatched' THEN 'file_writing'
WHEN 'unknown' THEN 'reconciliation_required' WHEN 'succeeded' THEN 'acceptance_required'
ELSE CASE WHEN required_filename<>'' THEN 'delivery_required' ELSE 'acceptance_required' END END,
reason=?,revision=revision+1,updated_at=? WHERE id=?`, p.Status, reason, now(), p.TaskID); err != nil {
		return err
	}
	return addEvent(tx, p.RunID, "publication.updated", p)
}

func (s *Store) DecidePublication(ctx context.Context, key, decision string, revision int) (Publication, error) {
	if decision != "approve" && decision != "deny" {
		return Publication{}, ErrState
	}
	s.publicationMu.Lock()
	defer s.publicationMu.Unlock()
	p, content, err := s.beginPublication(ctx, key, decision, revision)
	if err != nil || content == nil {
		return p, err
	}
	status, detail, prepared := s.writePublication(p, *content)
	p, err = s.finishPublication(p, status, detail)
	if err == nil && prepared {
		s.cleanupPublication(p)
	}
	return p, err
}

func (s *Store) beginPublication(ctx context.Context, key, decision string, revision int) (Publication, *string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Publication{}, nil, err
	}
	defer tx.Rollback()
	p, err := scanPublication(tx.QueryRow("SELECT "+publicationColumns+" FROM publications WHERE id=?", key))
	if err != nil {
		return p, nil, err
	}
	if p.Status == "expired" {
		return p, nil, ErrApprovalExpired
	}
	if p.Status != "pending" {
		if p.Decision == decision {
			return p, nil, nil
		}
		return p, nil, ErrState
	}
	if now() >= p.ExpiresAt {
		p = expiredPublication(p)
		if err = savePublication(tx, p); err != nil {
			return p, nil, err
		}
		if err = tx.Commit(); err != nil {
			return p, nil, err
		}
		return p, nil, ErrApprovalExpired
	}
	var currentRevision int
	var taskStatus, content string
	err = tx.QueryRow("SELECT t.revision,t.status,a.content FROM tasks t JOIN artifacts a ON a.task_id=t.id WHERE t.id=? AND a.id=?", p.TaskID, p.ArtifactID).Scan(&currentRevision, &taskStatus, &content)
	if err != nil {
		return p, nil, err
	}
	if revision != p.taskRevision || currentRevision != revision || taskStatus != "waiting_user" || digest(content) != p.Digest {
		return p, nil, ErrState
	}
	p.Decision = decision
	var payload *string
	switch {
	case decision == "deny":
		p.Status, p.Detail = "denied", "已拒绝本次写入"
	case !s.workspace.valid() || p.workspaceID != s.workspace.id:
		p.Status, p.Detail = "conflict", "工作区已变更，请重新发起保存"
	default:
		p.Status, p.Detail = "dispatched", "正在保存到工作区"
		payload = &content
	}
	if err = savePublication(tx, p); err != nil {
		return p, nil, err
	}
	return p, payload, tx.Commit()
}

func expiredPublication(p Publication) Publication {
	p.Status, p.Detail, p.Decision = "expired", "写入确认已过期", ""
	return p
}

func savePublication(tx *sql.Tx, p Publication) error {
	if _, err := tx.Exec("UPDATE publications SET status=?,decision=?,detail=?,updated_at=?,previous_content='' WHERE id=?", p.Status, p.Decision, p.Detail, now(), p.ID); err != nil {
		return err
	}
	return publicationEvent(tx, p, p.Detail)
}

func (p Publication) tempName() string { return ".clusbot-" + p.ID + ".tmp" }

func (s *Store) writePublication(p Publication, content string) (string, string, bool) {
	w := s.workspace
	if !w.valid() || p.workspaceID != w.id {
		return "conflict", "工作区已变更，请重新发起保存", false
	}
	tmp := p.tempName()
	f, err := w.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "failed", "无法准备写入文件", false
	}
	_, err = io.WriteString(f, content)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "failed", "无法保存临时文件", true
	}
	if !w.valid() {
		return "conflict", "工作区已变更，请重新发起保存", true
	}
	if p.PreviousDigest != "" {
		status, detail := s.replaceOutput(p)
		return status, detail, true
	}
	if err = w.root.Link(tmp, p.Filename); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "conflict", "目标文件已出现，本次写入已停止", true
		}
		return "unknown", "写入结果待核对", true
	}
	if err = w.syncOutputs(); err != nil {
		return "unknown", "写入结果待核对", true
	}
	status, detail := s.inspectPublication(p, true)
	return status, detail, true
}

// Before a write is recorded as successful, the temporary inode proves that
// the approved operation created the destination. Acceptance uses the saved
// publication and checks the file's current bytes after that inode is removed.
func (s *Store) inspectPublication(p Publication, requireWriteEvidence bool) (string, string) {
	w := s.workspace
	if !w.valid() || p.workspaceID != w.id {
		return "unknown", "工作区已变更，写入结果待核对"
	}
	if requireWriteEvidence && p.PreviousDigest != "" {
		return s.inspectReplacement(p)
	}
	dest, err := w.readOutput(p.Filename)
	if errors.Is(err, os.ErrNotExist) {
		return "not_applied", "核对时未发现目标文件，可重新申请写入"
	}
	if err != nil {
		return "unknown", "无法确认目标文件，写入结果待核对"
	}
	if requireWriteEvidence {
		tmp, err := w.root.Lstat(p.tempName())
		if err != nil || !os.SameFile(tmp, dest.info) {
			return "unknown", "目标文件与本次写入的记录不符"
		}
	}
	if dest.hash != p.Digest {
		return "unknown", "目标文件内容发生变化，写入结果待核对"
	}
	return "succeeded", "已保存到工作区"
}

func (s *Store) finishPublication(p Publication, status, detail string) (Publication, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	p.Status, p.Detail = status, detail
	if err = savePublication(tx, p); err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func (s *Store) ReconcilePublication(key string) (Publication, error) {
	s.publicationMu.Lock()
	defer s.publicationMu.Unlock()
	p, err := scanPublication(s.db.QueryRow("SELECT "+publicationColumns+" FROM publications WHERE id=?", key))
	if err != nil {
		return p, err
	}
	if p.Status != "dispatched" && p.Status != "unknown" {
		return p, nil
	}
	return s.reconcilePublication(p)
}

func (s *Store) reconcilePublication(p Publication) (Publication, error) {
	status, detail := s.inspectPublication(p, true)
	p, err := s.finishPublication(p, status, detail)
	if err == nil {
		s.cleanupPublication(p)
	}
	return p, err
}

func (s *Store) RecoverPublications() error {
	rows, err := s.db.Query("SELECT " + publicationColumns + " FROM publications WHERE status IN ('dispatched','unknown')")
	if err != nil {
		return err
	}
	var entries []Publication
	for rows.Next() {
		p, err := scanPublication(rows)
		if err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range entries {
		if _, err = s.reconcilePublication(p); err != nil {
			return err
		}
	}
	return nil
}
