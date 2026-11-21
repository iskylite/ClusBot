package work

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"path/filepath"
	"unicode/utf8"
)

const MaxUploadBytes = 10 * 1024 * 1024
const uploadStride = maxMaterialBytes - utf8.UTFMax

var (
	ErrUpload        = errors.New("请选择非空的 UTF-8 文本、Markdown、CSV 或日志文件，最大 10 MiB")
	ErrUploadSize    = errors.New("文件超过 10 MiB")
	ErrUploadPart    = errors.New("所选片段超出文件范围")
	ErrUploadChanged = errors.New("上传原件无法读取或校验失败")
)

func validUploadFilename(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".md", ".txt", ".log", ".csv", ".tsv", ".json", ".yaml", ".yml":
		return safeFilename(name)
	default:
		return false
	}
}

type UploadPart struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Digest string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	Part   int    `json:"part"`
	Parts  int    `json:"parts"`
}

func (s *Store) OpenUploads(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	expected, err := os.Lstat(path)
	if err != nil || !expected.IsDir() {
		return ErrUploadChanged
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, info) {
		root.Close()
		return ErrUploadChanged
	}
	s.uploads = root
	return nil
}

func (s *Store) UploadMaterial(ctx context.Context, name string, data []byte) (Material, error) {
	if len(data) > MaxUploadBytes {
		return Material{}, ErrUploadSize
	}
	if !validUploadFilename(name) || len(data) == 0 || !utf8.Valid(data) || slices.Contains(data, 0) {
		return Material{}, ErrUpload
	}
	if s.uploads == nil {
		return Material{}, ErrUploadChanged
	}
	u := UploadPart{ID: id(), Name: name, Digest: digest(string(data)), Bytes: len(data), Part: 1, Parts: (len(data) + uploadStride - 1) / uploadStride}
	temporary, final := "."+u.ID+".tmp", u.ID+".txt"
	file, err := s.uploads.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Material{}, err
	}
	defer s.uploads.Remove(temporary)
	if err = ctx.Err(); err != nil {
		file.Close()
		return Material{}, err
	}
	_, writeErr := file.Write(data)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return Material{}, err
	}
	if err = s.uploads.Link(temporary, final); err != nil {
		return Material{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = s.uploads.Remove(final)
		}
	}()
	if err = s.uploads.Remove(temporary); err != nil {
		return Material{}, err
	}
	dir, err := s.uploads.Open(".")
	if err != nil {
		return Material{}, err
	}
	if err = errors.Join(dir.Sync(), dir.Close()); err != nil {
		return Material{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Material{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO uploads VALUES(?,?,?,?,?,?)", u.ID, u.Name, u.Digest, u.Bytes, u.Parts, now()); err != nil {
		return Material{}, err
	}
	m, err := saveUploadPart(ctx, tx, u, data)
	if err != nil {
		return Material{}, err
	}
	if err = tx.Commit(); err != nil {
		return Material{}, err
	}
	committed = true
	return m, nil
}

func (s *Store) UploadMaterialPart(ctx context.Context, id string, part int) (Material, error) {
	u := UploadPart{ID: id, Part: part}
	err := s.db.QueryRowContext(ctx, "SELECT name,sha256,size,parts FROM uploads WHERE id=?", id).Scan(&u.Name, &u.Digest, &u.Bytes, &u.Parts)
	if err != nil {
		return Material{}, err
	}
	if part < 1 || part > u.Parts {
		return Material{}, ErrUploadPart
	}
	if s.uploads == nil {
		return Material{}, ErrUploadChanged
	}
	file, info, err := openBackupFile(s.uploads, id+".txt")
	if err != nil {
		return Material{}, ErrUploadChanged
	}
	defer file.Close()
	if info.Size() != int64(u.Bytes) || u.Bytes > MaxUploadBytes {
		return Material{}, ErrUploadChanged
	}
	data, err := io.ReadAll(io.LimitReader(backupReader{ctx, file}, MaxUploadBytes+1))
	if err != nil || len(data) != u.Bytes || digest(string(data)) != u.Digest || !utf8.Valid(data) {
		return Material{}, ErrUploadChanged
	}
	return saveUploadPart(ctx, s.db, u, data)
}

func saveUploadPart(ctx context.Context, q materialWriter, u UploadPart, data []byte) (Material, error) {
	boundary := func(offset int) int {
		if offset >= len(data) {
			return len(data)
		}
		for offset > 0 && !utf8.RuneStart(data[offset]) {
			offset--
		}
		return offset
	}
	part := data[boundary((u.Part-1)*uploadStride):boundary(u.Part*uploadStride)]
	path := fmt.Sprintf("uploads/%s/%s#%d", u.ID, u.Name, u.Part)
	return saveMaterial(ctx, q, path, part, &u)
}

func validUploadPath(path string) bool {
	id, ok := strings.CutPrefix(path, "uploads/")
	if !ok {
		return false
	}
	id, ok = strings.CutSuffix(id, ".txt")
	if !ok || len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func backupUploads(ctx context.Context, database string) ([]backupFile, error) {
	db, err := sql.Open("sqlite", sqliteFileURI(database, "mode=ro"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT id,sha256,size FROM uploads ORDER BY id LIMIT 10001")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []backupFile
	for rows.Next() {
		var id string
		var file backupFile
		if err = rows.Scan(&id, &file.Digest, &file.Bytes); err != nil {
			return nil, err
		}
		file.Path = "uploads/" + id + ".txt"
		if !validUploadPath(file.Path) || file.Bytes < 1 || file.Bytes > MaxUploadBytes {
			return nil, ErrUploadChanged
		}
		entries = append(entries, file)
	}
	return entries, rows.Err()
}
