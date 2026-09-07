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
	db         *bolt.DB
	aead       cipher.AEAD
	archiveKey []byte
}

func Open(path string, password []byte) (*Vault, error) {
	if len(password) < 16 {
		return nil, errors.New("vault password must be at least 16 bytes")
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	db, e := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if e != nil {
		return nil, e
	}
	fail := func(err error) (*Vault, error) { db.Close(); return nil, err }
	var salt []byte
	e = db.Update(func(tx *bolt.Tx) error {
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
	})
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
	v := &Vault{db: db, aead: aead, archiveKey: archiveKey}
	exists, e := v.authenticate()
	if e != nil {
		return fail(errors.New("vault password incorrect or state corrupted"))
	}
	if !exists {
		if e = v.Save(map[string]any{}); e != nil {
			return fail(e)
		}
	}
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
