//go:build linux

package storage

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func cloneFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	closeErr := out.Close()
	if err != nil {
		_ = os.Remove(destination)
	}
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EINVAL) {
		return ErrCloneUnsupported
	}
	if err != nil {
		return err
	}
	return closeErr
}
