// Package storage provides authenticated, encrypted, atomic state snapshots.
package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/crypto/scrypt"
	"os"
	"path/filepath"
	"time"
)

var bucket = []byte("vault-v1")

type Vault struct {
	path       string
	db         *bolt.DB
	aead       cipher.AEAD
	archiveKey []byte
}

func Open(path string, password []byte) (*Vault, error) {
	return openVault(path, password, false, true)
}

// OpenReadOnly authenticates an existing vault without creating directories,
// buckets, salts or empty state. It is suitable for source-preserving format
// preflight and offline projections; callers must revalidate after acquiring a
// writer before activating a profile that could have changed in between.
func OpenReadOnly(path string, password []byte) (*Vault, error) {
	return openVault(path, password, true, false)
}

// OpenExisting acquires exclusive ownership of an authenticated existing vault
// without initializing or writing it. The caller can reject its decoded format
// without changing source bytes, including after a separate read-only preflight.
func OpenExisting(path string, password []byte) (*Vault, error) {
	return openVault(path, password, false, false)
}

func openVault(path string, password []byte, readOnly, initializeMissing bool) (*Vault, error) {
	if len(password) < 16 {
		return nil, errors.New("vault password must be at least 16 bytes")
	}
	if initializeMissing {
		if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return nil, e
		}
	}
	options := &bolt.Options{Timeout: time.Second, ReadOnly: readOnly}
	if !readOnly && !initializeMissing {
		// bbolt otherwise creates an absent/empty file and may commit a freelist
		// conversion while opening. Both precede the caller's format validation.
		options.OpenFile = openExistingLocked
		options.NoFreelistSync = true
	}
	db, e := bolt.Open(path, 0600, options)
	if e != nil {
		return nil, e
	}
	fail := func(err error) (*Vault, error) { db.Close(); return nil, err }
	var salt []byte
	initialize := func(tx *bolt.Tx) error {
		b, e := tx.CreateBucketIfNotExists(bucket)
		if e != nil {
			return e
		}
		salt = append([]byte(nil), b.Get([]byte("salt"))...)
		if salt == nil {
			salt = make([]byte, 32)
			if _, e = rand.Read(salt); e != nil {
				return e
			}
			return b.Put([]byte("salt"), salt)
		}
		if len(salt) != 32 {
			return errors.New("corrupt salt")
		}
		return nil
	}
	if !initializeMissing {
		e = db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucket)
			if b == nil {
				return errors.New("existing file has no encrypted vault")
			}
			salt = append([]byte(nil), b.Get([]byte("salt"))...)
			if len(salt) != 32 {
				return errors.New("existing vault has no valid salt")
			}
			return nil
		})
	} else {
		e = db.Update(initialize)
	}
	if e != nil {
		return fail(e)
	}
	key, e := scrypt.Key(password, salt, 32768, 8, 1, 32)
	if e != nil {
		return fail(e)
	}
	block, e := aes.NewCipher(key)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("blakeswap/archive-index/v1"))
	archiveKey := mac.Sum(nil)
	clear(key)
	if e != nil {
		return fail(e)
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return fail(e)
	}
	v := &Vault{path: path, db: db, aead: aead, archiveKey: archiveKey}
	exists, e := v.authenticate()
	if e != nil {
		return fail(errors.New("vault password incorrect or state corrupted"))
	}
	if !exists {
		if !initializeMissing {
			return fail(errors.New("existing vault has no authenticated state"))
		}
		if e = v.Save(map[string]any{}); e != nil {
			return fail(e)
		}
	}
	// Authentication is complete. This only controls future caller-requested
	// commits; rejecting the decoded format and closing still performs no write.
	db.NoFreelistSync = false
	return v, nil
}
func (v *Vault) Load(out any) (bool, error) {
	var b []byte
	e := v.db.View(func(tx *bolt.Tx) error {
		b = append([]byte(nil), tx.Bucket(bucket).Get([]byte("state"))...)
		return nil
	})
	if e != nil || b == nil {
		return false, e
	}
	if len(b) < v.aead.NonceSize() {
		return false, errors.New("truncated vault")
	}
	raw, e := v.aead.Open(nil, b[:v.aead.NonceSize()], b[v.aead.NonceSize():], []byte("blakeswap/state/v1"))
	if e != nil {
		return false, e
	}
	defer clear(raw)
	return true, json.Unmarshal(raw, out)
}
func (v *Vault) Save(state any) error {
	var records []ArchiveRecord
	if portable, ok := state.(interface {
		VaultSnapshot() (any, []ArchiveRecord, error)
	}); ok {
		var err error
		state, records, err = portable.VaultSnapshot()
		if err != nil {
			return err
		}
	}
	_, err := v.CommitArchive(state, ArchiveBatch{Put: records}, 0)
	return err
}
func (v *Vault) Close() error { return v.db.Close() }
func (v *Vault) Backup(path string) error {
	return v.db.View(func(tx *bolt.Tx) error { return tx.CopyFile(path, 0600) })
}

// Authenticate without allocating a second generic object graph of the entire
// active state. The typed caller still performs semantic validation on Load.
func (v *Vault) authenticate() (bool, error) {
	exists := false
	err := v.db.View(func(tx *bolt.Tx) error {
		sealed := tx.Bucket(bucket).Get([]byte("state"))
		if sealed == nil {
			return nil
		}
		raw, err := v.unseal(sealed, []byte("blakeswap/state/v1"))
		if err != nil {
			return err
		}
		defer clear(raw)
		if !json.Valid(raw) {
			return errors.New("invalid encrypted state JSON")
		}
		exists = true
		return nil
	})
	return exists, err
}

// PrivateDirectory is the wallet-owned directory under the exclusive vault lock.
func (v *Vault) PrivateDirectory() string { return filepath.Dir(v.path) }
