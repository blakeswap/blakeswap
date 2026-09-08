package nativebridge

import (
	"context"
	"errors"

	"github.com/blakeswap/blakeswap/internal/credential"
)

type Store struct{ Peer *Peer }
type credentialRequest struct {
	Key      credential.Key `json:"key"`
	Password []byte         `json:"password,omitempty"`
}
type credentialReply struct {
	Password []byte `json:"password"`
}

func credentialError(err error) error {
	var code ErrorCode
	if errors.As(err, &code) {
		switch code {
		case "locked":
			return credential.ErrLocked
		case "denied", "cancelled":
			return credential.ErrDenied
		case "missing":
			return credential.ErrMissing
		case "exists":
			return credential.ErrExists
		case "conflict":
			return credential.ErrConflict
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return credential.ErrUnavailable
}
func (s Store) Get(ctx context.Context, key credential.Key) ([]byte, error) {
	if s.Peer == nil {
		return nil, credential.ErrUnavailable
	}
	var reply credentialReply
	if err := s.Peer.Call(ctx, "credential.get", credentialRequest{Key: key}, &reply); err != nil {
		clear(reply.Password)
		return nil, credentialError(err)
	}
	if len(reply.Password) < 16 || len(reply.Password) > 4096 {
		clear(reply.Password)
		return nil, credential.ErrUnavailable
	}
	return reply.Password, nil
}
func (s Store) Create(ctx context.Context, key credential.Key, password []byte) error {
	if s.Peer == nil {
		return credential.ErrUnavailable
	}
	if len(password) < 16 || len(password) > 4096 {
		return credential.ErrUnavailable
	}
	if err := s.Peer.Call(ctx, "credential.create", credentialRequest{Key: key, Password: password}, nil); err != nil {
		return credentialError(err)
	}
	return nil
}
func (s Store) Delete(ctx context.Context, key credential.Key) error {
	if s.Peer == nil {
		return credential.ErrUnavailable
	}
	if err := s.Peer.Call(ctx, "credential.delete", credentialRequest{Key: key}, nil); err != nil {
		return credentialError(err)
	}
	return nil
}
