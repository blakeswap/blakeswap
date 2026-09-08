//go:build darwin

package storage

import (
	"errors"
	"golang.org/x/sys/unix"
)

func cloneFile(source, destination string) error {
	err := unix.Clonefile(source, destination, 0)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOSYS) {
		return ErrCloneUnsupported
	}
	return err
}
