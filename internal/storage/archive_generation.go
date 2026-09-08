package storage

import (
	"crypto/rand"
	"errors"

	bolt "go.etcd.io/bbolt"
)

var archiveGenerationKey = []byte("archive-generation-v1")
var archiveGenerationAAD = []byte("blakeswap/archive-generation/v1")

// Generation identifies a particular immutable set of archive records. Unlike
// semantic freshness it changes on location-only moves, including delete/reinsert
// ABA. It is authenticated, small, and read without decoding the active state.
func (v *Vault) archiveGeneration(tx *bolt.Tx) ([]byte, error) {
	sealed := tx.Bucket(bucket).Get(archiveGenerationKey)
	if sealed == nil {
		return nil, nil
	}
	raw, err := v.unseal(sealed, archiveGenerationAAD)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		clear(raw)
		return nil, errors.New("invalid archive generation")
	}
	return raw, nil
}
func (v *Vault) advanceArchiveGeneration(tx *bolt.Tx) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	defer clear(raw)
	sealed, err := v.seal(raw, archiveGenerationAAD)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put(archiveGenerationKey, sealed)
}
func (v *Vault) ensureArchiveGeneration() error {
	return v.db.Update(func(tx *bolt.Tx) error {
		generation, err := v.archiveGeneration(tx)
		if err != nil {
			return err
		}
		if generation != nil {
			clear(generation)
			return nil
		}
		return v.advanceArchiveGeneration(tx)
	})
}
