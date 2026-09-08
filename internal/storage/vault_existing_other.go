//go:build !darwin && !linux

package storage

import (
	"errors"
	"os"
)

func openExistingLocked(string, int, os.FileMode) (*os.File, error) {
	return nil, errors.New("source-preserving exclusive vault opening is supported on macOS and Linux")
}
