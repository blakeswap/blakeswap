package storage

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// PrivateIndex is an encrypted, disposable exact-key lookup. The independent
// random key is never persisted or derived from wallet credentials. It stores
// no authority: callers rebuild it from an authenticated completed checkpoint.
// Callers serialize transactions and close/rollback each before Close. The same
// exclusive-wallet cleanup as SortedRows removes abandoned index directories.
type PrivateIndex struct {
	dir  string
	key  []byte
	db   *bolt.DB
	once sync.Once
}

type PrivateIndexTx struct {
	index *PrivateIndex
	ctx   context.Context
	tx    *bolt.Tx
}

var privateIndexBucket = []byte("entries")

func NewPrivateIndex(ctx context.Context, directory string) (*PrivateIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(directory, sortRowsPrefix)
	if err != nil {
		return nil, err
	}
	index := &PrivateIndex{dir: dir, key: make([]byte, 32)}
	fail := func(err error) (*PrivateIndex, error) { _ = index.Close(); return nil, err }
	if _, err = rand.Read(index.key); err != nil {
		return fail(err)
	}
	index.db, err = bolt.Open(filepath.Join(dir, "index.db"), 0600, &bolt.Options{Timeout: time.Second, NoSync: true, NoFreelistSync: true})
	if err != nil {
		return fail(err)
	}
	if err = index.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := tx.CreateBucket(privateIndexBucket)
		return err
	}); err != nil {
		return fail(err)
	}
	return index, nil
}

func (i *PrivateIndex) Close() error {
	var err error
	i.once.Do(func() {
		if i.db != nil {
			err = i.db.Close()
		}
		clear(i.key)
		err = errors.Join(err, os.RemoveAll(i.dir))
	})
	return err
}

func (i *PrivateIndex) Begin(ctx context.Context) (*PrivateIndexTx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := i.db.Begin(true)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return &PrivateIndexTx{index: i, ctx: ctx, tx: tx}, nil
}

func (t *PrivateIndexTx) Rollback() {
	if t != nil && t.tx != nil {
		_ = t.tx.Rollback()
		t.tx = nil
	}
}
func (t *PrivateIndexTx) Commit() error {
	if t == nil || t.tx == nil {
		return errors.New("private index transaction is closed")
	}
	if err := t.ctx.Err(); err != nil {
		t.Rollback()
		return err
	}
	err := t.tx.Commit()
	t.tx = nil
	return err
}
func (t *PrivateIndexTx) address(key string) []byte {
	mac := hmac.New(sha256.New, t.index.key)
	_, _ = mac.Write([]byte("blakeswap/private-index/key\x00" + key))
	return mac.Sum(nil)
}
func (t *PrivateIndexTx) cipher() (cipher.AEAD, error) {
	block, err := aes.NewCipher(t.index.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Get returns an owned value, which the caller clears. Both plaintext identity
// and value are authenticated, even if an encrypted entry is moved to a new key.
func (t *PrivateIndexTx) Get(key string) ([]byte, bool, error) {
	if t == nil || t.tx == nil {
		return nil, false, errors.New("private index transaction is closed")
	}
	if err := t.ctx.Err(); err != nil {
		return nil, false, err
	}
	address := t.address(key)
	sealed := t.tx.Bucket(privateIndexBucket).Get(address)
	if sealed == nil {
		return nil, false, nil
	}
	aead, err := t.cipher()
	if err != nil {
		return nil, false, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, false, errors.New("truncated private index entry")
	}
	raw, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], address)
	if err != nil {
		return nil, false, err
	}
	defer clear(raw)
	if len(raw) < 8 {
		return nil, false, errors.New("invalid private index entry")
	}
	size := binary.BigEndian.Uint64(raw[:8])
	if size > uint64(len(raw)-8) || !bytes.Equal(raw[8:8+size], []byte(key)) {
		return nil, false, errors.New("private index identity mismatch")
	}
	return bytes.Clone(raw[8+size:]), true, nil
}

// Assign refuses different ownership at an existing exact key. Release is
// likewise conditional, so a missing or altered expected owner fails closed.
func (t *PrivateIndexTx) Assign(key, owner string) error {
	if key == "" || owner == "" || len(key) > 1024 || len(owner) > 1024 {
		return errors.New("invalid private index entry bounds")
	}
	previous, found, err := t.Get(key)
	defer clear(previous)
	if err != nil {
		return err
	}
	if found {
		if string(previous) != owner {
			return errors.New("private index ownership conflict")
		}
		return nil
	}
	aead, err := t.cipher()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	address := t.address(key)
	raw := binary.BigEndian.AppendUint64(nil, uint64(len(key)))
	raw = append(raw, key...)
	raw = append(raw, owner...)
	defer clear(raw)
	sealed := aead.Seal(nonce, nonce, raw, address)
	return t.tx.Bucket(privateIndexBucket).Put(address, sealed)
}
func (t *PrivateIndexTx) Release(key, owner string) error {
	previous, found, err := t.Get(key)
	defer clear(previous)
	if err != nil {
		return err
	}
	if !found || string(previous) != owner {
		return errors.New("private index expected owner missing or changed")
	}
	return t.tx.Bucket(privateIndexBucket).Delete(t.address(key))
}

// OwnPrivateIndex joins private validation lifetime to the vault's custody,
// including callers which close the vault without constructing a live Engine.
func (v *Vault) OwnPrivateIndex(index *PrivateIndex) error {
	v.privateMu.Lock()
	defer v.privateMu.Unlock()
	if v.privateClosed {
		_ = index.Close()
		return errors.New("vault is closed")
	}
	v.privateIndexes = append(v.privateIndexes, index)
	return nil
}
