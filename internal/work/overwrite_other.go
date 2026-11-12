//go:build !darwin && !linux

package work

import "errors"

func renameWithoutReplace(int, string, string) error {
	return errors.ErrUnsupported
}
