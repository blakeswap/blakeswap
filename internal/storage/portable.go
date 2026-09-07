package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/scrypt"
)

// Only this format marker, random salt and nonce are public. The manifest,
// identities, network inventory and complete wallet state are authenticated and
// encrypted together. A backup never includes installation passwords or cookies.
var portableMagic = []byte("BLAKESWAP-BACKUP\x00\x01")

const PortableLimit = 256 << 20

func portableCipher(password, salt []byte) (cipher.AEAD, error) {
	if len(password) < 16 {
		return nil, errors.New("backup password must contain at least 16 bytes")
	}
	key, err := scrypt.Key(password, salt, 32768, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// WritePortable publishes a fully fsynced archive atomically, without replacing
// any existing destination. The temporary file is private and on the same volume.
func WritePortable(ctx context.Context, path string, password []byte, value any) error {
	if !filepath.IsAbs(path) {
		return errors.New("choose an absolute backup destination")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	defer clear(raw)
	if len(raw) > PortableLimit {
		return errors.New("portable backup exceeds 256 MiB")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	aead, err := portableCipher(password, salt)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	header := append(append([]byte(nil), portableMagic...), salt...)
	sealed := aead.Seal(nil, nonce, raw, header)
	defer clear(sealed)
	file, err := os.CreateTemp(filepath.Dir(path), ".backup-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	for _, part := range [][]byte{header, nonce, sealed} {
		if _, err := file.Write(part); err != nil {
			return err
		}
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(file.Name(), path); err != nil {
		return errors.New("backup not installed; choose a new filename on a filesystem supporting atomic links")
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// ReadPortable never opens the source for writing and authenticates the whole
// encrypted manifest before decoding anything supplied by it.
func ReadPortable(ctx context.Context, path string, password []byte, out any) error {
	if !filepath.IsAbs(path) {
		return errors.New("choose an absolute backup source")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > PortableLimit+1024 {
		return errors.New("choose a portable backup up to 256 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(file, PortableLimit+1025))
	if err != nil {
		return err
	}
	defer clear(raw)
	if len(raw) > PortableLimit+1024 || len(raw) < len(portableMagic)+32+12+16 {
		return errors.New("invalid portable backup")
	}
	for i, b := range portableMagic {
		if raw[i] != b {
			return errors.New("unsupported portable backup format")
		}
	}
	headerSize := len(portableMagic) + 32
	aead, err := portableCipher(password, raw[len(portableMagic):headerSize])
	if err != nil {
		return err
	}
	nonceEnd := headerSize + aead.NonceSize()
	plain, err := aead.Open(nil, raw[headerSize:nonceEnd], raw[nonceEnd:], raw[:headerSize])
	if err != nil {
		return errors.New("cannot authenticate backup; check its password and file")
	}
	defer clear(plain)
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(plain) > PortableLimit {
		return errors.New("portable backup exceeds 256 MiB")
	}
	return json.Unmarshal(plain, out)
}
