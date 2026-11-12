package work

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

const maxOutputBytes = 256 * 1024

type outputSnapshot struct {
	info                   os.FileInfo
	hash, version, content string
}

func fileVersion(info os.FileInfo) string {
	stat := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d:%d:%d:%d", stat.Dev, stat.Ino, info.Size(), info.ModTime().UnixNano(), info.Mode())
}

func (w *workspace) readOutput(name string) (outputSnapshot, error) {
	var snapshot outputSnapshot
	info, err := w.root.Lstat(name)
	if err != nil {
		return snapshot, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxOutputBytes {
		return snapshot, ErrOverwriteTarget
	}
	f, err := w.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return snapshot, ErrOverwriteTarget
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return snapshot, ErrOverwriteTarget
	}
	body, err := io.ReadAll(io.LimitReader(f, maxOutputBytes+1))
	if err != nil || len(body) > maxOutputBytes {
		return snapshot, ErrOverwriteTarget
	}
	after, err := w.root.Lstat(name)
	version := fileVersion(info)
	if err != nil || fileVersion(after) != version || !w.valid() {
		return snapshot, ErrOverwriteTarget
	}
	snapshot.info, snapshot.version, snapshot.content = after, version, string(body)
	snapshot.hash = digest(snapshot.content)
	return snapshot, nil
}

func (w *workspace) syncOutputs() error {
	dir, err := w.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (w *workspace) moveOutput(from, to string) error {
	dir, err := w.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return renameWithoutReplace(int(dir.Fd()), from, to)
}

func (p Publication) backupName() string { return ".clusbot-" + p.ID + ".previous" }

func (p Publication) matchesPrevious(file outputSnapshot) bool {
	return file.hash == p.PreviousDigest && file.version == p.previousVersion
}

func (s *Store) replaceOutput(p Publication) (string, string) {
	w := s.workspace
	current, err := w.readOutput(p.Filename)
	if err != nil || !p.matchesPrevious(current) {
		return "conflict", "目标文件已变更，请重新检查差异并确认"
	}
	if err := w.moveOutput(p.Filename, p.backupName()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "conflict", "目标文件已变更，请重新检查差异并确认"
		}
		return "failed", "无法安全替换目标文件"
	}
	// Check the actual file moved out of the destination before installing
	// the approved bytes. A concurrent writer may have changed the name
	// between the earlier check and move.
	previous, err := w.readOutput(p.backupName())
	if err != nil || !p.matchesPrevious(previous) || w.syncOutputs() != nil {
		return s.inspectReplacement(p)
	}
	// The same inspection resolves successful links and ambiguous errors.
	_ = w.root.Link(p.tempName(), p.Filename)
	return s.inspectReplacement(p)
}

func (s *Store) inspectReplacement(p Publication) (string, string) {
	w := s.workspace
	if !w.valid() || w.id != p.workspaceID {
		return "unknown", "工作区已变更，覆盖结果待核对"
	}
	backupName := p.backupName()
	dest, destErr := w.root.Lstat(p.Filename)
	tmp, tmpErr := w.root.Lstat(p.tempName())
	installed := destErr == nil && tmpErr == nil && os.SameFile(tmp, dest)
	_, backupErr := w.root.Lstat(backupName)
	if errors.Is(backupErr, os.ErrNotExist) && !installed && (destErr == nil || errors.Is(destErr, os.ErrNotExist)) {
		return "not_applied", "本次覆盖未写入，请重新检查目标"
	}
	if backupErr == nil && errors.Is(destErr, os.ErrNotExist) {
		// Recovery may restore a moved file, but never installs the new
		// version or overwrites a name another writer has occupied.
		if err := w.moveOutput(backupName, p.Filename); err == nil {
			if w.syncOutputs() == nil {
				return "conflict", "已恢复原文件，请重新检查差异并确认"
			}
			return "unknown", "已尝试恢复原文件，请再次核对"
		}
	}
	if backupErr == nil && installed {
		previous, err := w.readOutput(backupName)
		if err == nil && p.matchesPrevious(previous) {
			current, err := w.readOutput(p.Filename)
			if err == nil && os.SameFile(tmp, current.info) && current.hash == p.Digest && w.syncOutputs() == nil {
				return "succeeded", "已保存到工作区"
			}
		}
	}
	if backupErr == nil {
		return "unknown", "覆盖结果待核对。原文件保留在 outputs/" + backupName + "。请先将当前目标移到其他位置，再核对以恢复原文件"
	}
	return "unknown", "覆盖结果待核对"
}

func (s *Store) cleanupPublication(p Publication) {
	w := s.workspace
	if p.Status == "unknown" || !w.valid() || w.id != p.workspaceID {
		return
	}
	_ = w.root.Remove(p.tempName())
	if p.PreviousDigest != "" && p.Status == "succeeded" {
		_ = w.root.Remove(p.backupName())
	}
}
