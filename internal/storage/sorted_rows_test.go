package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSortedRowsMultipleLevelsExactOrderPagesDigestAndPrivacy(t *testing.T) {
	directory := t.TempDir()
	sorter, err := NewRowSorter(context.Background(), directory, func(a, b []byte) bool { return bytes.Compare(a, b) < 0 })
	if err != nil {
		t.Fatal(err)
	}
	defer sorter.Close()
	// More than fan-in squared runs exercises two online carries and a final
	// merge containing unequal levels. Every key remains unique across ties.
	const count = 17003
	expected := make([]string, count)
	for i := count - 1; i >= 0; i-- {
		expected[i] = fmt.Sprintf("private-wallet-linked-history-%05d", i)
		raw, _ := json.Marshal(expected[i])
		if err := sorter.Add([]byte(fmt.Sprintf("%05d", i)), raw); err != nil {
			t.Fatal(err)
		}
	}
	if len(sorter.runs) > sortRunFanIn*3 {
		t.Fatal("run bookkeeping retained lifetime batches", len(sorter.runs))
	}
	result, err := sorter.Finish()
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	raw, _ := json.Marshal(expected)
	digest := sha256.Sum256(raw)
	if result.Count != count || result.Digest != hex.EncodeToString(digest[:]) {
		t.Fatal("count or complete ordered digest mismatch")
	}
	for start := uint64(0); start < count; start += 499 {
		rows, err := result.Page(context.Background(), start, 499)
		if err != nil {
			t.Fatal(err)
		}
		for i, row := range rows {
			var got string
			if err := json.Unmarshal(row, &got); err != nil || got != expected[int(start)+i] {
				t.Fatal("page lost ordering or boundary", start, i, err)
			}
		}
	}
	if _, err := result.Page(context.Background(), count+1, 1); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	if _, err := result.Page(context.Background(), 0, 501); err == nil {
		t.Fatal("unbounded page accepted")
	}
	entries, err := os.ReadDir(result.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(result.dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("private-wallet-linked")) {
			t.Fatal("plaintext row in query scratch")
		}
	}
	key := result.key
	path := result.dir
	if err := result.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, make([]byte, 32)) {
		t.Fatal("result key survived close")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("result scratch survived close", err)
	}
}

func smallSortedRows(t *testing.T) *SortedRows {
	t.Helper()
	s, err := NewRowSorter(context.Background(), t.TempDir(), func(a, b []byte) bool { return bytes.Compare(a, b) < 0 })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, key := range []string{"b", "a", "c"} {
		if err := s.Add([]byte(key), []byte(`"private-`+key+`"`)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.Finish()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = result.Close() })
	return result
}
func TestSortedRowsRejectCorruptionSubstitutionAndCancellation(t *testing.T) {
	for _, mode := range []string{"ciphertext", "ordinal", "size", "truncated", "cross-result"} {
		t.Run(mode, func(t *testing.T) {
			r := smallSortedRows(t)
			file, err := os.OpenFile(r.run.path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			switch mode {
			case "ciphertext":
				var original [1]byte
				_, err = file.ReadAt(original[:], sortRunHeaderSize+9)
				if err == nil {
					original[0] ^= 1
					_, err = file.WriteAt(original[:], sortRunHeaderSize+9)
				}
			case "ordinal":
				var second [8]byte
				_, err = r.index.ReadAt(second[:], 8)
				if err == nil {
					_, err = r.index.WriteAt(second[:], 0)
				}
			case "size":
				_, err = file.WriteAt(binary.BigEndian.AppendUint64(nil, uint64(r.run.size)), sortRunHeaderSize)
			case "truncated":
				err = file.Truncate(sortRunHeaderSize + 8)
			case "cross-result":
				other := smallSortedRows(t)
				data, readErr := os.ReadFile(other.run.path)
				err = readErr
				if err == nil {
					_, err = file.WriteAt(data[sortRunHeaderSize:], sortRunHeaderSize)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Page(context.Background(), 0, 3); err == nil {
				t.Fatal("invalid encrypted result accepted", mode)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewRowSorter(ctx, t.TempDir(), func(a, b []byte) bool { return bytes.Compare(a, b) < 0 })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := s.Add([]byte(fmt.Sprint(i)), []byte(`"retained"`)); err != nil {
			t.Fatal(err)
		}
	}
	path := s.result.dir
	cancel()
	if _, err := s.Finish(); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled files survived", err)
	}
}
