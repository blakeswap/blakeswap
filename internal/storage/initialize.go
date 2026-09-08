package storage

import (
	"os"
	"path/filepath"
)

// Initialize publishes a fully authenticated initial state at an absent path.
// Every write happens in a private sibling directory first. A crash before
// publication leaves no final vault; after publication the final vault already
// contains its complete format marker and state. The hard link is atomic and
// refuses an existing destination instead of overwriting another initializer.
func Initialize(path string, password []byte, state any) error {
	return initialize(path, password, state, nil)
}

func initialize(path string, password []byte, state any, beforePublish func(string) error) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".vault-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	privatePath := filepath.Join(stage, "state.db")
	vault, err := Open(privatePath, password)
	if err != nil {
		return err
	}
	if err := vault.Save(state); err != nil {
		_ = vault.Close()
		return err
	}
	if err := vault.Close(); err != nil {
		return err
	}
	if beforePublish != nil {
		if err := beforePublish(privatePath); err != nil {
			return err
		}
	}
	if err := os.Link(privatePath, path); err != nil {
		return err
	}
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
