package daemon

import (
	"context"
	"errors"

	"github.com/blakeswap/blakeswap/internal/credential"
)

// acquirePassword always returns an owned copy. Native mode cannot select a
// password file after a provider failure, including a missing migrated item.
func (c Config) acquirePassword(ctx context.Context) ([]byte, error) {
	if c.Credential != nil {
		if c.CredentialMode != "native" {
			return nil, errors.New("injected credentials require native mode")
		}
		password, err := c.Credential.Acquire(ctx)
		if err != nil {
			clear(password)
			return nil, err
		}
		if len(password) < 16 || len(password) > 4096 {
			clear(password)
			return nil, errors.New("native credential length is invalid")
		}
		return password, nil
	}
	if c.CredentialMode == "native" {
		return nil, credential.ErrUnavailable
	}
	// The library retains legacy configuration compatibility for existing
	// embedders. The CLI entry point requires explicit file mode; the desktop
	// always installs its native source before constructing any engine.
	if c.CredentialMode != "" && c.CredentialMode != "file" {
		return nil, errors.New("unknown credential mode")
	}
	return credential.File(c.PasswordFile).Acquire(ctx)
}
