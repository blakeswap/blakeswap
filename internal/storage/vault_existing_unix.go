//go:build darwin || linux

package storage

import (
	"errors"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

// bbolt repeats flock on this same descriptor, retaining exclusive ownership.
// Checking size only before acquiring the lock would allow an empty replacement
// to enter bbolt's initialization path while waiting for the previous writer.
func openExistingLocked(path string, _ int, mode os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR, mode)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) { _ = f.Close(); return nil, err }
	deadline := time.Now().Add(time.Second)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fail(err)
		}
		if !time.Now().Before(deadline) {
			return fail(bolt.ErrTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fail(errors.New("existing vault is empty or not a regular file"))
	}
	return f, nil
}
