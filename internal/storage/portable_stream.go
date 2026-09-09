package storage

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
)

var portableStreamMagic = []byte("BLAKESWAP-STREAM\x00\x01")

const portableChunkSize = 1 << 20

var portableStreamHeaderSize = len(portableStreamMagic) + 44

type portableChunkWriter struct {
	ctx      context.Context
	out      io.Writer
	aead     cipher.AEAD
	header   []byte
	prefix   []byte
	buffer   []byte
	index    uint64
	total    uint64
	expected uint64
	digest   hash.Hash
}

func chunkNonce(prefix []byte, index uint64) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix)
	binary.BigEndian.PutUint64(nonce[4:], index)
	return nonce
}
func chunkAAD(header []byte, kind byte, index uint64, length uint32) []byte {
	aad := append([]byte(nil), header...)
	aad = append(aad, kind)
	return binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint64(aad, index), length)
}
func (w *portableChunkWriter) frame(kind byte, raw []byte) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.index == ^uint64(0) {
		return errors.New("portable chunk counter exhausted")
	}
	length := uint32(len(raw) + w.aead.Overhead())
	sealed := w.aead.Seal(nil, chunkNonce(w.prefix, w.index), raw, chunkAAD(w.header, kind, w.index, length))
	defer clear(sealed)
	frame := binary.BigEndian.AppendUint32([]byte{kind}, length)
	if _, err := w.out.Write(frame); err != nil {
		return err
	}
	if _, err := w.out.Write(sealed); err != nil {
		return err
	}
	w.index++
	return nil
}
func (w *portableChunkWriter) Write(raw []byte) (int, error) {
	n := 0
	for len(raw) > 0 {
		if err := w.ctx.Err(); err != nil {
			return n, err
		}
		size := min(len(raw), portableChunkSize-len(w.buffer))
		if uint64(size) > w.expected-w.total {
			return n, errors.New("portable producer changed after complete size preflight")
		}
		w.buffer = append(w.buffer, raw[:size]...)
		w.digest.Write(raw[:size])
		w.total += uint64(size)
		raw = raw[size:]
		n += size
		if len(w.buffer) == portableChunkSize {
			if err := w.frame(0, w.buffer); err != nil {
				return n, err
			}
			clear(w.buffer)
			w.buffer = w.buffer[:0]
		}
	}
	return n, nil
}
func (w *portableChunkWriter) finish() error {
	if w.total != w.expected {
		return errors.New("portable producer changed after complete size preflight")
	}
	if len(w.buffer) > 0 {
		if err := w.frame(0, w.buffer); err != nil {
			return err
		}
		clear(w.buffer)
		w.buffer = w.buffer[:0]
	}
	trailer := binary.BigEndian.AppendUint64(nil, w.total)
	trailer = binary.BigEndian.AppendUint64(trailer, w.index)
	trailer = w.digest.Sum(trailer)
	return w.frame(1, trailer)
}

// WritePortableStream encrypts bounded chunks directly from the producer. A
// final authenticated count, length and SHA-256 commits to the complete stream;
// sequence-bound nonces/AAD reject reordering, duplication and cross-export data.
// There is no whole-value JSON buffer or plaintext temporary file.
func WritePortableStream(ctx context.Context, path string, password []byte, produce func(io.Writer) error) error {
	if !filepath.IsAbs(path) {
		return errors.New("choose an absolute backup destination")
	}
	counter := &portableSizeCounter{ctx: ctx}
	if err := produce(counter); err != nil {
		return err
	}
	encodedSize, err := portableEncodedSize(counter.total)
	if err != nil {
		return err
	}
	if err := CheckPathSpace(filepath.Dir(path), encodedSize, 1); err != nil {
		return err
	}
	salt, prefix := make([]byte, 32), make([]byte, 4)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if _, err := rand.Read(prefix); err != nil {
		return err
	}
	aead, err := portableCipher(password, salt)
	if err != nil {
		return err
	}
	header := append(append(append([]byte(nil), portableStreamMagic...), salt...), prefix...)
	header = binary.BigEndian.AppendUint64(header, counter.total)
	file, err := os.CreateTemp(filepath.Dir(path), ".backup-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(header); err != nil {
		return err
	}
	writer := &portableChunkWriter{ctx: ctx, out: file, aead: aead, header: header, prefix: prefix, buffer: make([]byte, 0, portableChunkSize), expected: counter.total, digest: sha256.New()}
	defer clear(writer.buffer[:cap(writer.buffer)])
	if err = produce(writer); err != nil {
		return err
	}
	if err = writer.finish(); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Link(file.Name(), path); err != nil {
		return errors.New("backup not installed; choose a new filename on a filesystem supporting atomic links")
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type portableChunkReader struct {
	ctx            context.Context
	in             io.Reader
	aead           cipher.AEAD
	header, prefix []byte
	buffer         []byte
	index          uint64
	total          uint64
	expected       uint64
	digest         hash.Hash
	done           bool
}

func (r *portableChunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.buffer) > 0 {
		n := copy(p, r.buffer)
		clear(r.buffer[:n])
		r.buffer = r.buffer[n:]
		return n, nil
	}
	if r.done {
		return 0, io.EOF
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	var frame [5]byte
	if _, err := io.ReadFull(r.in, frame[:]); err != nil {
		return 0, errors.New("portable archive is incomplete")
	}
	length := binary.BigEndian.Uint32(frame[1:])
	if (frame[0] != 0 && frame[0] != 1) || length < uint32(r.aead.Overhead()) || length > portableChunkSize+uint32(r.aead.Overhead()) || r.index == ^uint64(0) {
		return 0, errors.New("invalid portable chunk")
	}
	sealed := make([]byte, length)
	if _, err := io.ReadFull(r.in, sealed); err != nil {
		return 0, errors.New("portable archive is incomplete")
	}
	raw, err := r.aead.Open(sealed[:0], chunkNonce(r.prefix, r.index), sealed, chunkAAD(r.header, frame[0], r.index, length))
	if err != nil {
		clear(sealed)
		return 0, errors.New("incorrect backup password or damaged portable archive")
	}
	if frame[0] == 1 {
		defer clear(sealed)
		if len(raw) != 48 || binary.BigEndian.Uint64(raw[:8]) != r.total || r.total != r.expected || binary.BigEndian.Uint64(raw[8:16]) != r.index || !bytes.Equal(raw[16:], r.digest.Sum(nil)) {
			return 0, errors.New("portable archive completeness check failed")
		}
		var extra [1]byte
		if n, err := r.in.Read(extra[:]); n != 0 || err != io.EOF {
			return 0, errors.New("trailing portable archive data")
		}
		r.done = true
		return 0, io.EOF
	}
	if len(raw) == 0 || uint64(len(raw)) > r.expected-r.total {
		clear(sealed)
		return 0, errors.New("portable stream exceeds its supported bound")
	}
	r.digest.Write(raw)
	r.total += uint64(len(raw))
	r.index++
	r.buffer = raw
	return r.Read(p)
}

// ReadPortableStream authenticates the complete ciphertext before invoking a
// consumer, then decrypts again in bounded chunks. The consumer must read through
// EOF; private staging must be discarded on any error. Source files stay read-only.
func ReadPortableStream(ctx context.Context, path string, password []byte, consume func(io.Reader) error) error {
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
	if !info.Mode().IsRegular() {
		return errors.New("portable stream exceeds its supported bound")
	}
	header := make([]byte, portableStreamHeaderSize)
	if _, err = io.ReadFull(file, header); err != nil || !bytes.Equal(header[:len(portableStreamMagic)], portableStreamMagic) {
		return errors.New("unsupported portable stream format")
	}
	expected := binary.BigEndian.Uint64(header[len(header)-8:])
	encodedSize, sizeErr := portableEncodedSize(expected)
	if sizeErr != nil || encodedSize != uint64(info.Size()) {
		return errors.New("portable ciphertext length does not match declared complete size")
	}
	aead, err := portableCipher(password, header[len(portableStreamMagic):len(portableStreamMagic)+32])
	if err != nil {
		return err
	}
	newReader := func() *portableChunkReader {
		return &portableChunkReader{ctx: ctx, in: file, aead: aead, header: header, prefix: header[len(header)-12 : len(header)-8], expected: expected, digest: sha256.New()}
	}
	first := newReader()
	if _, err = io.Copy(io.Discard, first); err != nil {
		return err
	}
	if _, err = file.Seek(int64(len(header)), io.SeekStart); err != nil {
		return err
	}
	second := newReader()
	defer func() { clear(second.buffer) }()
	if err = consume(second); err != nil {
		return err
	}
	if !second.done {
		return errors.New("portable consumer did not verify the complete stream")
	}
	return nil
}

// An archive is bounded by the actual filesystem representation and available
// disk, not a small lifetime history constant. Both passes use the same frozen
// producer; a changed byte count cannot publish a partial selection.
type portableSizeCounter struct {
	ctx   context.Context
	total uint64
}

func (c *portableSizeCounter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	if uint64(len(p)) > math.MaxInt64-c.total {
		return 0, errors.New("portable byte count exceeds filesystem range")
	}
	c.total += uint64(len(p))
	return len(p), nil
}
func portableEncodedSize(plain uint64) (uint64, error) {
	if plain > math.MaxInt64 {
		return 0, errors.New("portable byte count exceeds filesystem range")
	}
	chunks := plain / portableChunkSize
	if plain%portableChunkSize != 0 {
		chunks++
	}
	overhead := uint64(portableStreamHeaderSize) + chunks*21 + 21 + 48
	if plain > math.MaxInt64-overhead {
		return 0, errors.New("portable ciphertext exceeds filesystem range")
	}
	return plain + overhead, nil
}
