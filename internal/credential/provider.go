// Package credential defines owned, short-lived password acquisition. It never
// serializes provider state or sends secrets through command-line arguments.
package credential

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
)

var (
	ErrUnavailable = errors.New("native credential service is unavailable")
	ErrLocked      = errors.New("Keychain is locked; unlock it in the native app")
	ErrDenied      = errors.New("Keychain access was denied or cancelled")
	ErrMissing     = errors.New("wallet Keychain item is missing; restore from recovery material")
	ErrExists      = errors.New("wallet Keychain item already exists")
	ErrConflict    = errors.New("existing Keychain item does not match this wallet")
)

type Key struct {
	Installation string `json:"installation"`
	Profile      string `json:"profile"`
	Record       string `json:"record"`
}

// Store is implemented by the owned native broker. Get returns caller-owned
// bytes which must be cleared. Create never replaces an existing item.
type Store interface {
	Get(context.Context, Key) ([]byte, error)
	Create(context.Context, Key, []byte) error
	Delete(context.Context, Key) error
}

type Source interface {
	Acquire(context.Context) ([]byte, error)
}

type SourceFunc func(context.Context) ([]byte, error)

func (f SourceFunc) Acquire(ctx context.Context) ([]byte, error) { return f(ctx) }

// File is an explicit operator-controlled headless credential mode. Desktop
// native mode never selects it after any Keychain error.
type File string

func (path File) Acquire(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(string(path))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return nil, errors.New("credential file must be a private regular file of at most 4096 bytes")
	}
	f, err := os.Open(string(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("credential file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	defer clear(raw)
	if err != nil {
		return nil, err
	}
	if len(raw) > 4096 || len(bytes.TrimSpace(raw)) < 16 {
		return nil, errors.New("invalid credential file length")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]byte(nil), bytes.TrimSpace(raw)...), nil
}
