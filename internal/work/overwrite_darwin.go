package work

import "golang.org/x/sys/unix"

func renameWithoutReplace(dir int, from, to string) error {
	return unix.RenameatxNp(dir, from, dir, to, unix.RENAME_EXCL)
}
