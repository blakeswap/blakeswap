package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPortableStreamAuthenticatesOrderingAndCompletenessBeforeConsumer(t *testing.T) {
	password := []byte("independent stream backup password")
	payload := bytes.Repeat([]byte("record-state\x00"), 200000)
	path := filepath.Join(t.TempDir(), "complete.backup")
	if err := WritePortableStream(context.Background(), path, password, func(w io.Writer) error { _, err := w.Write(payload); return err }); err != nil {
		t.Fatal(err)
	}
	var recovered []byte
	if err := ReadPortableStream(context.Background(), path, password, func(r io.Reader) (err error) { recovered, err = io.ReadAll(r); return }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, recovered) {
		t.Fatal("stream round trip lost bytes")
	}
	sealed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	headerLen := portableStreamHeaderSize
	header := sealed[:headerLen]
	frames := [][]byte{}
	for offset := headerLen; offset < len(sealed); {
		length := int(binary.BigEndian.Uint32(sealed[offset+1 : offset+5]))
		frames = append(frames, sealed[offset:offset+5+length])
		offset += 5 + length
	}
	other := filepath.Join(t.TempDir(), "other.backup")
	if err := WritePortableStream(context.Background(), other, password, func(w io.Writer) error { _, err := w.Write(payload); return err }); err != nil {
		t.Fatal(err)
	}
	otherSealed, _ := os.ReadFile(other)
	join := func(parts ...[]byte) []byte {
		var result []byte
		for _, p := range parts {
			result = append(result, p...)
		}
		return result
	}
	cases := map[string][]byte{
		"truncated":          sealed[:len(sealed)-1],
		"missing completion": sealed[:len(sealed)-len(frames[len(frames)-1])],
		"duplicate":          join(header, frames[0], frames[0], bytes.Join(frames[1:], nil)),
		"reordered":          join(header, frames[1], frames[0], bytes.Join(frames[2:], nil)),
		"cross export":       join(header, otherSealed[headerLen:headerLen+len(frames[0])], bytes.Join(frames[1:], nil)),
		"trailing":           join(sealed, []byte{0}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			damaged := filepath.Join(t.TempDir(), "damaged.backup")
			if err := os.WriteFile(damaged, raw, 0600); err != nil {
				t.Fatal(err)
			}
			invoked := false
			err := ReadPortableStream(context.Background(), damaged, password, func(r io.Reader) error { invoked = true; _, err := io.Copy(io.Discard, r); return err })
			if err == nil || invoked {
				t.Fatal("unverified archive reached consumer", err, invoked)
			}
		})
	}
	invoked := false
	if err := ReadPortableStream(context.Background(), path, []byte("incorrect sufficiently long password"), func(r io.Reader) error { invoked = true; return nil }); err == nil || invoked {
		t.Fatal("wrong password reached consumer")
	}
	if err := ReadPortableStream(context.Background(), path, password, func(r io.Reader) error { return nil }); err == nil {
		t.Fatal("partial consumer reported success")
	}
}
func TestPortableStreamNeverPublishesFailedOrCanceledExport(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "backup")
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		err := WritePortableStream(ctx, path, []byte("a sufficiently long backup password"), func(w io.Writer) error {
			if _, err := w.Write(bytes.Repeat([]byte{1}, portableChunkSize+1)); err != nil {
				return err
			}
			if cancel {
				stop()
				return nil
			}
			return errors.New("snapshot failed")
		})
		if err == nil {
			t.Fatal("failed snapshot published")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("failed destination exists", err)
		}
		entries, _ := os.ReadDir(filepath.Dir(path))
		if len(entries) != 0 {
			t.Fatal("private temporary file left behind")
		}
	}
}

func TestPortableStreamPreflightGuardsChangedProducerAndUint64Accounting(t *testing.T) {
	for _, length := range []uint64{0, 1, portableChunkSize, portableChunkSize + 1, 1 << 32, 1 << 40} {
		size, err := portableEncodedSize(length)
		if err != nil || size <= length {
			t.Fatal("invalid complete size", length, size, err)
		}
	}
	if _, err := portableEncodedSize(^uint64(0)); err == nil {
		t.Fatal("overflowing plaintext accepted")
	}
	if _, err := portableEncodedSize(uint64(1<<63 - 1)); err == nil {
		t.Fatal("ciphertext overhead overflow accepted")
	}
	for _, more := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "changed.backup")
		calls := 0
		err := WritePortableStream(context.Background(), path, []byte("separately chosen changed producer password"), func(w io.Writer) error {
			calls++
			payload := "exact"
			if calls == 2 {
				if more {
					payload += " changed"
				} else {
					payload = ""
				}
			}
			_, err := io.WriteString(w, payload)
			return err
		})
		if err == nil {
			t.Fatal("changed producer published")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("partial archive exists")
		}
	}
	nonceA, nonceB := chunkNonce([]byte{1, 2, 3, 4}, 1), chunkNonce([]byte{1, 2, 3, 4}, 1+(1<<32))
	if bytes.Equal(nonceA, nonceB) {
		t.Fatal("uint64 counter reused a nonce")
	}
}
